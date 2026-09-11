package model

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTokenAutoGroupsRoundTripThroughRedisHashCache(t *testing.T) {
	useUserCacheMiniRedis(t)
	token := Token{
		Id:         42,
		UserId:     7,
		Key:        "token-auto-groups-cache-key",
		Name:       "auto-cache",
		Group:      "auto",
		AutoGroups: `["vip","default"]`,
	}

	require.NoError(t, cacheSetTokenForTest(token))
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, token.AutoGroups, cached.AutoGroups)
	groups, err := cached.GetAutoGroups()
	require.NoError(t, err)
	assert.Equal(t, []string{"vip", "default"}, groups)
}

func TestTokenRequestPolicyRoundTripsThroughRedisHashCache(t *testing.T) {
	useUserCacheMiniRedis(t)
	token := Token{
		Id:                       43,
		UserId:                   7,
		Key:                      "token-request-policy-cache-key",
		Name:                     "request-policy-cache",
		DefaultRoutingStrategy:   "economy",
		AllowedRoutingStrategies: `["economy","latency"]`,
		DefaultConversionPolicy:  "safe",
		AllowLossyConversion:     true,
	}

	require.NoError(t, cacheSetTokenForTest(token))
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, token.DefaultRoutingStrategy, cached.DefaultRoutingStrategy)
	assert.Equal(t, token.AllowedRoutingStrategies, cached.AllowedRoutingStrategies)
	assert.Equal(t, token.DefaultConversionPolicy, cached.DefaultConversionPolicy)
	assert.Equal(t, token.AllowLossyConversion, cached.AllowLossyConversion)
}

func TestTokenUpdateSynchronouslyNarrowsPreheatedAutoGroupsCache(t *testing.T) {
	truncateTables(t)
	useUserCacheMiniRedis(t)
	token := Token{
		UserId:          7,
		Key:             "token-auto-groups-update-cache-key",
		Name:            "auto-cache-update",
		Status:          common.TokenStatusEnabled,
		ExpiredTime:     -1,
		UnlimitedQuota:  true,
		Group:           "auto",
		CrossGroupRetry: true,
		AutoGroups:      `["default","vip"]`,
	}
	require.NoError(t, token.Insert())
	require.NoError(t, cacheSetTokenForTest(token))

	preheated, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.JSONEq(t, `["default","vip"]`, preheated.AutoGroups)

	require.NoError(t, token.SetAutoGroups([]string{"vip"}))
	require.NoError(t, token.Update())
	// Update 是限制性变更：写库前删除缓存并设置 fence。缓存不再提供旧的
	// 宽分组值，下一次读取必须看到收紧后的分组。
	_, cacheErr := cacheGetTokenByKey(token.Key)
	require.Error(t, cacheErr, "the pre-update cache entry must be invalidated")
	reloaded, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.JSONEq(t, `["vip"]`, reloaded.AutoGroups)
}

func TestTokenMetadataMutationsFailClosedWhenFenceUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Token) error
	}{
		{
			name: "update",
			mutate: func(token *Token) error {
				token.AllowLossyConversion = false
				return token.Update()
			},
		},
		{
			name: "status update",
			mutate: func(token *Token) error {
				token.Status = common.TokenStatusDisabled
				return token.SelectUpdate()
			},
		},
		{
			name: "delete",
			mutate: func(token *Token) error {
				return token.Delete()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			truncateTables(t)
			server := useUserCacheMiniRedis(t)
			token := Token{
				UserId:               7,
				Key:                  "token-fence-failure-" + test.name,
				Name:                 "fence-failure",
				Status:               common.TokenStatusEnabled,
				ExpiredTime:          -1,
				UnlimitedQuota:       true,
				AllowLossyConversion: true,
			}
			require.NoError(t, token.Insert())
			server.SetError("ERR token cache unavailable")

			require.Error(t, test.mutate(&token))

			var stored Token
			require.NoError(t, DB.Unscoped().First(&stored, token.Id).Error)
			assert.Equal(t, common.TokenStatusEnabled, stored.Status)
			assert.True(t, stored.AllowLossyConversion)
			assert.False(t, stored.DeletedAt.Valid)
		})
	}
}

func TestTokenMutationFailureClearsPendingFenceForUnchangedDatabaseState(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	token := Token{
		UserId:               7,
		Key:                  "token-fence-db-failure",
		Name:                 "fence-db-failure",
		Status:               common.TokenStatusEnabled,
		ExpiredTime:          -1,
		UnlimitedQuota:       true,
		AllowLossyConversion: true,
	}
	require.NoError(t, token.Insert())
	require.NoError(t, cacheSetTokenForTest(token))

	forcedErr := errors.New("forced token update failure")
	const callbackName = "test:fail_token_update_after_fence"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" {
			tx.AddError(forcedErr)
		}
	}))
	callbackRegistered := true
	t.Cleanup(func() {
		if callbackRegistered {
			_ = DB.Callback().Update().Remove(callbackName)
		}
	})

	token.AllowLossyConversion = false
	assert.ErrorIs(t, token.Update(), forcedErr)
	require.NoError(t, DB.Callback().Update().Remove(callbackName))
	callbackRegistered = false

	assert.False(t, server.Exists(getTokenCacheFenceKey(token.Key)))
	reloaded, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.True(t, reloaded.AllowLossyConversion)
	assert.True(t, server.Exists(getTokenCacheKey(token.Key)))
}

func TestTokenReaderReloadsSnapshotAfterConcurrentPolicyCommit(t *testing.T) {
	truncateTables(t)
	useUserCacheMiniRedis(t)
	token := Token{
		UserId:                   7,
		Key:                      "token-policy-race",
		Name:                     "policy-race",
		Status:                   common.TokenStatusEnabled,
		ExpiredTime:              -1,
		RemainQuota:              100,
		AllowedRoutingStrategies: `["stable","economy"]`,
		DefaultRoutingStrategy:   "economy",
		DefaultConversionPolicy:  "allow",
		AllowLossyConversion:     true,
	}
	require.NoError(t, token.Insert())
	_, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	result, err := cacheApplyTokenQuotaDelta(token.Id, token.Key, -70)
	require.NoError(t, err)
	require.Equal(t, cacheQuotaOK, result)

	snapshotRead := make(chan struct{})
	releaseReader := make(chan struct{})
	var intercepted atomic.Bool
	const callbackName = "test:block_stale_token_reader"
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" && intercepted.CompareAndSwap(false, true) {
			close(snapshotRead)
			<-releaseReader
		}
	}))
	t.Cleanup(func() {
		_ = DB.Callback().Query().Remove(callbackName)
	})

	type readResult struct {
		token *Token
		err   error
	}
	readerResult := make(chan readResult, 1)
	go func() {
		loaded, readErr := GetTokenByKey(token.Key, true)
		readerResult <- readResult{token: loaded, err: readErr}
	}()
	<-snapshotRead

	token.DefaultRoutingStrategy = "stable"
	token.AllowedRoutingStrategies = `["stable"]`
	token.DefaultConversionPolicy = "strict"
	token.AllowLossyConversion = false
	require.NoError(t, token.Update())
	close(releaseReader)

	loaded := <-readerResult
	require.NoError(t, loaded.err)
	require.NotNil(t, loaded.token)
	assert.Equal(t, "stable", loaded.token.DefaultRoutingStrategy)
	assert.JSONEq(t, `["stable"]`, loaded.token.AllowedRoutingStrategies)
	assert.Equal(t, "strict", loaded.token.DefaultConversionPolicy)
	assert.False(t, loaded.token.AllowLossyConversion)
	assert.Zero(t, common.RDB.Exists(t.Context(), getTokenCacheKey(token.Key)).Val(),
		"a stale DB snapshot must not repopulate the token hash after commit")
}

func TestTokenMutationCommitsFenceAfterPendingGenerationExpires(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	token := Token{
		UserId:               7,
		Key:                  "token-expired-pending-fence",
		Name:                 "expired-pending-fence",
		Status:               common.TokenStatusEnabled,
		ExpiredTime:          -1,
		UnlimitedQuota:       true,
		AllowLossyConversion: true,
	}
	require.NoError(t, token.Insert())
	require.NoError(t, cacheSetTokenForTest(token))

	updateReachedDB := make(chan struct{})
	releaseUpdate := make(chan struct{})
	const callbackName = "test:block_token_update_past_fence_expiry"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" {
			close(updateReachedDB)
			<-releaseUpdate
		}
	}))
	t.Cleanup(func() {
		_ = DB.Callback().Update().Remove(callbackName)
	})

	token.AllowLossyConversion = false
	updateResult := make(chan error, 1)
	go func() {
		updateResult <- token.Update()
	}()
	<-updateReachedDB

	server.FastForward(time.Duration(tokenCacheFenceSeconds+1) * time.Second)
	code, err := cacheInitToken(Token{
		Id:                   token.Id,
		UserId:               token.UserId,
		Key:                  token.Key,
		Status:               token.Status,
		Name:                 token.Name,
		ExpiredTime:          token.ExpiredTime,
		UnlimitedQuota:       token.UnlimitedQuota,
		AllowLossyConversion: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, code)
	close(releaseUpdate)

	require.NoError(t, <-updateResult)
	assert.False(t, server.Exists(getTokenCacheKey(token.Key)))
	assert.True(t, server.Exists(getTokenCacheFenceKey(token.Key)))
	reloaded, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.False(t, reloaded.AllowLossyConversion)
}

// cacheSetTokenForTest 以测试身份写入完整 token 缓存（含额度字段），
// 模拟“已水合”的缓存状态。
func cacheSetTokenForTest(token Token) error {
	_, err := cacheInitToken(token)
	return err
}
