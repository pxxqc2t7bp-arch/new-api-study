package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type failRedisEvalHook struct {
	callCount atomic.Int64
	failAt    int64
	err       error
}

func (h *failRedisEvalHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if cmd.Name() == "eval" && h.callCount.Add(1) == h.failAt {
		return ctx, h.err
	}
	return ctx, nil
}

func (*failRedisEvalHook) AfterProcess(context.Context, redis.Cmder) error {
	return nil
}

func (*failRedisEvalHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (*failRedisEvalHook) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}

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
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.JSONEq(t, `["vip"]`, cached.AutoGroups)
	reloaded, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.JSONEq(t, `["vip"]`, reloaded.AutoGroups)
}

func TestTokenPolicyUpdatePreservesLiveCachedQuota(t *testing.T) {
	truncateTables(t)
	useUserCacheMiniRedis(t)
	token := Token{
		UserId:                   7,
		Key:                      "token-policy-live-quota",
		Name:                     "policy-live-quota",
		Status:                   common.TokenStatusEnabled,
		ExpiredTime:              -1,
		RemainQuota:              100,
		AllowedRoutingStrategies: `["stable","economy"]`,
		DefaultRoutingStrategy:   "economy",
		DefaultConversionPolicy:  "allow",
		AllowLossyConversion:     true,
	}
	require.NoError(t, token.Insert())
	require.NoError(t, cacheSetTokenForTest(token))
	result, err := cacheApplyTokenQuotaDelta(token.Id, token.Key, -70)
	require.NoError(t, err)
	require.Equal(t, cacheQuotaOK, result)

	token.AllowedRoutingStrategies = `["stable"]`
	token.DefaultRoutingStrategy = "stable"
	token.DefaultConversionPolicy = "strict"
	token.AllowLossyConversion = false
	require.NoError(t, token.Update())

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, 100, stored.RemainQuota)
	assert.Zero(t, stored.UsedQuota)
	assert.Equal(t, "stable", stored.DefaultRoutingStrategy)
	assert.Equal(t, "strict", stored.DefaultConversionPolicy)

	generation, pending, err := getTokenCacheGenerationState(token.Key)
	require.NoError(t, err)
	require.False(t, pending)
	code, err := cacheInitToken(stored, generation)
	require.NoError(t, err)
	require.Equal(t, 2, code)

	reloaded, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	assert.Equal(t, 30, reloaded.RemainQuota)
	assert.Equal(t, 70, reloaded.UsedQuota)
	assert.Equal(t, "stable", reloaded.DefaultRoutingStrategy)
	assert.JSONEq(t, `["stable"]`, reloaded.AllowedRoutingStrategies)
	assert.Equal(t, "strict", reloaded.DefaultConversionPolicy)
	assert.False(t, reloaded.AllowLossyConversion)

	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 30, cached.RemainQuota)
	assert.Equal(t, 70, cached.UsedQuota)
	assert.Equal(t, "stable", cached.DefaultRoutingStrategy)
	assert.Equal(t, "strict", cached.DefaultConversionPolicy)
}

func TestTokenPolicyUpdateLeavesColdCacheCold(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	token := Token{
		UserId:         7,
		Key:            "token-policy-cold-cache",
		Name:           "policy-cold-cache",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		RemainQuota:    100,
		UnlimitedQuota: true,
	}
	require.NoError(t, token.Insert())

	token.DefaultRoutingStrategy = "economy"
	require.NoError(t, token.Update())

	assert.False(t, server.Exists(getTokenCacheKey(token.Key)))
	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, "economy", stored.DefaultRoutingStrategy)
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
		RemainQuota:          100,
		UnlimitedQuota:       true,
		AllowLossyConversion: true,
	}
	require.NoError(t, token.Insert())
	require.NoError(t, cacheSetTokenForTest(token))
	result, err := cacheApplyTokenQuotaDelta(token.Id, token.Key, -70)
	require.NoError(t, err)
	require.Equal(t, cacheQuotaOK, result)

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

	_, pending, err := getTokenCacheGenerationState(token.Key)
	require.NoError(t, err)
	assert.False(t, pending)
	reloaded, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.True(t, reloaded.AllowLossyConversion)
	assert.Equal(t, 30, reloaded.RemainQuota)
	assert.Equal(t, 70, reloaded.UsedQuota)
	assert.True(t, server.Exists(getTokenCacheKey(token.Key)))
}

func TestTokenReaderReloadsSnapshotAfterConcurrentPolicyCommit(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	common.SyncFrequency = tokenCacheFenceSeconds * 2
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
	server.FastForward(time.Duration(tokenCacheFenceSeconds+1) * time.Second)
	close(releaseReader)

	loaded := <-readerResult
	require.NoError(t, loaded.err)
	require.NotNil(t, loaded.token)
	assert.Equal(t, "stable", loaded.token.DefaultRoutingStrategy)
	assert.JSONEq(t, `["stable"]`, loaded.token.AllowedRoutingStrategies)
	assert.Equal(t, "strict", loaded.token.DefaultConversionPolicy)
	assert.False(t, loaded.token.AllowLossyConversion)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, "stable", cached.DefaultRoutingStrategy)
	assert.JSONEq(t, `["stable"]`, cached.AllowedRoutingStrategies)
	assert.Equal(t, "strict", cached.DefaultConversionPolicy)
	assert.False(t, cached.AllowLossyConversion)
	assert.Equal(t, 30, cached.RemainQuota)
	assert.Equal(t, 70, cached.UsedQuota)
}

func TestTokenMutationFinalizationFailureReportsCommittedAndRejectsStaleReader(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	common.SyncFrequency = tokenCacheFenceSeconds * 2
	token := Token{
		UserId:                 7,
		Key:                    "token-finalization-failure",
		Name:                   "finalization-failure",
		Status:                 common.TokenStatusEnabled,
		ExpiredTime:            -1,
		RemainQuota:            100,
		UnlimitedQuota:         true,
		DefaultRoutingStrategy: "economy",
		AllowLossyConversion:   true,
	}
	require.NoError(t, token.Insert())
	require.NoError(t, cacheSetTokenForTest(token))
	result, err := cacheApplyTokenQuotaDelta(token.Id, token.Key, -70)
	require.NoError(t, err)
	require.Equal(t, cacheQuotaOK, result)

	snapshotRead := make(chan struct{})
	releaseReader := make(chan struct{})
	var intercepted atomic.Bool
	const callbackName = "test:block_stale_token_reader_during_finalization_failure"
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

	finalizationErr := errors.New("forced token cache finalization failure")
	hook := &failRedisEvalHook{failAt: 2, err: finalizationErr}
	common.RDB.AddHook(hook)
	token.DefaultRoutingStrategy = "stable"
	token.AllowLossyConversion = false
	mutationErr := token.Update()
	require.Error(t, mutationErr)
	assert.ErrorIs(t, mutationErr, finalizationErr)
	var committedOutcome interface {
		CommittedCount() int
	}
	if assert.ErrorAs(t, mutationErr, &committedOutcome) {
		assert.Equal(t, 1, committedOutcome.CommittedCount())
	}

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, "stable", stored.DefaultRoutingStrategy)
	assert.False(t, stored.AllowLossyConversion)

	server.FastForward(time.Duration(tokenCacheFenceSeconds+1) * time.Second)
	reconciled, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, "stable", reconciled.DefaultRoutingStrategy)
	assert.False(t, reconciled.AllowLossyConversion)
	assert.Equal(t, 30, reconciled.RemainQuota)
	assert.Equal(t, 70, reconciled.UsedQuota)

	close(releaseReader)
	loaded := <-readerResult
	require.NoError(t, loaded.err)
	require.NotNil(t, loaded.token)
	assert.Equal(t, "stable", loaded.token.DefaultRoutingStrategy)
	assert.False(t, loaded.token.AllowLossyConversion)
	generation, pending, err := getTokenCacheGenerationState(token.Key)
	require.NoError(t, err)
	assert.EqualValues(t, 2, generation)
	assert.False(t, pending)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, "stable", cached.DefaultRoutingStrategy)
	assert.False(t, cached.AllowLossyConversion)
	assert.Equal(t, 30, cached.RemainQuota)
	assert.Equal(t, 70, cached.UsedQuota)
}

func TestTokenMutationRestoresGenerationFromDatabaseFloor(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	token := Token{
		UserId:         7,
		Key:            "token-generation-database-floor",
		Name:           "generation-database-floor",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, token.Insert())

	token.DefaultRoutingStrategy = "economy"
	require.NoError(t, token.Update())
	assert.EqualValues(t, 2, token.CacheGeneration)
	require.NoError(t, common.RDB.Del(t.Context(), getTokenCacheGenerationKey(token.Key)).Err())
	assert.False(t, server.Exists(getTokenCacheGenerationKey(token.Key)))

	token.DefaultRoutingStrategy = "stable"
	require.NoError(t, token.Update())
	assert.EqualValues(t, 4, token.CacheGeneration)
	generation, pending, err := getTokenCacheGenerationState(token.Key)
	require.NoError(t, err)
	assert.EqualValues(t, 4, generation)
	assert.False(t, pending)

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.EqualValues(t, 4, stored.CacheGeneration)
	assert.Equal(t, "stable", stored.DefaultRoutingStrategy)
}

func TestTokenUpdateWithoutRedisPreservesDatabaseGeneration(t *testing.T) {
	truncateTables(t)
	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
	})

	token := Token{
		UserId:          7,
		Key:             "token-generation-without-redis",
		Name:            "generation-without-redis",
		Status:          common.TokenStatusEnabled,
		ExpiredTime:     -1,
		UnlimitedQuota:  true,
		CacheGeneration: 6,
	}
	require.NoError(t, token.Insert())

	token.Name = "generation-preserved"
	require.NoError(t, token.Update())

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.EqualValues(t, 6, stored.CacheGeneration)
	assert.Equal(t, "generation-preserved", stored.Name)
}

func TestBatchDeleteTokensFinalizesEveryCommittedMutation(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	tokens := []Token{
		{
			UserId:         7,
			Key:            "token-batch-finalization-first",
			Name:           "batch-finalization-first",
			Status:         common.TokenStatusEnabled,
			ExpiredTime:    -1,
			UnlimitedQuota: true,
		},
		{
			UserId:         7,
			Key:            "token-batch-finalization-second",
			Name:           "batch-finalization-second",
			Status:         common.TokenStatusEnabled,
			ExpiredTime:    -1,
			UnlimitedQuota: true,
		},
	}
	for i := range tokens {
		require.NoError(t, tokens[i].Insert())
		require.NoError(t, cacheSetTokenForTest(tokens[i]))
	}

	finalizationErr := errors.New("forced first batch finalization failure")
	hook := &failRedisEvalHook{failAt: 3, err: finalizationErr}
	common.RDB.AddHook(hook)
	count, err := BatchDeleteTokens([]int{tokens[0].Id, tokens[1].Id}, tokens[0].UserId)

	require.Error(t, err)
	assert.ErrorIs(t, err, finalizationErr)
	assert.Equal(t, 2, count)
	var committedOutcome interface {
		CommittedCount() int
	}
	if assert.ErrorAs(t, err, &committedOutcome) {
		assert.Equal(t, 2, committedOutcome.CommittedCount())
	}
	assert.EqualValues(t, 4, hook.callCount.Load(), "both committed token fences must be finalized")
	assert.False(t, server.Exists(getTokenCachePendingFenceKey(tokens[1].Key)))

	var remaining int64
	require.NoError(t, DB.Model(&Token{}).Where("id IN ?", []int{tokens[0].Id, tokens[1].Id}).Count(&remaining).Error)
	assert.Zero(t, remaining)
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
	}, 0)
	require.NoError(t, err)
	require.Zero(t, code)
	close(releaseUpdate)

	require.NoError(t, <-updateResult)
	assert.False(t, server.Exists(getTokenCacheKey(token.Key)))
	generation, pending, err := getTokenCacheGenerationState(token.Key)
	require.NoError(t, err)
	assert.EqualValues(t, 2, generation)
	assert.False(t, pending)
	reloaded, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.False(t, reloaded.AllowLossyConversion)
}

// cacheSetTokenForTest 以测试身份写入完整 token 缓存（含额度字段），
// 模拟“已水合”的缓存状态。
func cacheSetTokenForTest(token Token) error {
	generation, pending, err := getTokenCacheGenerationState(token.Key)
	if err != nil {
		return err
	}
	if pending {
		return errTokenCacheMutationPending
	}
	_, err = cacheInitToken(token, generation)
	return err
}
