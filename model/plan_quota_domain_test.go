package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func setupPlanQuotaAuthorityTest(t *testing.T, tablePrefix string) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate",
		filepath.Join(t.TempDir(), "authority.db"),
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: tablePrefix},
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)

	previousDB := DB
	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, previousLogType)
	initCol()
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		initCol()
		require.NoError(t, sqlDB.Close())
	})

	require.NoError(t, db.AutoMigrate(
		&Channel{},
		&PlanQuotaDomain{},
		&Ability{},
		&UpstreamManagedRoute{},
	))
	return db
}

func TestInitializePlanQuotaDomains(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")

	activeTag := "plan:test:active"
	disabledTagA := "plan:test:disabled-a"
	disabledTagB := "plan:test:disabled-b"
	legacyTag := "plan:test:legacy"
	emptyTag := "plan:test:empty"
	multiTag := "plan:test:multi"
	ordinaryTag := "ordinary:test"
	activeCredential := "authority-secret-active"
	disabledCredential := "authority-secret-disabled"
	legacyCredential := "authority-secret-legacy"
	multiCredential := "authority-secret-multi-a\nauthority-secret-multi-b"

	disabledHash, ok := PlanQuotaDomainHash(disabledCredential)
	require.True(t, ok)
	channels := []Channel{
		{Name: "active", Key: activeCredential, Status: common.ChannelStatusEnabled, Tag: &activeTag},
		{Name: "disabled-old", Key: disabledCredential, Status: common.ChannelStatusAutoDisabled, Tag: &disabledTagA},
		{Name: "disabled-new", Key: disabledCredential, Status: common.ChannelStatusAutoDisabled, Tag: &disabledTagB},
		{Name: "legacy", Key: legacyCredential, Status: common.ChannelStatusAutoDisabled, Tag: &legacyTag},
		{Name: "empty", Key: "", Status: common.ChannelStatusEnabled, Tag: &emptyTag},
		{
			Name: "multi", Key: multiCredential, Status: common.ChannelStatusEnabled, Tag: &multiTag,
			ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
		},
		{Name: "ordinary", Key: activeCredential, Status: common.ChannelStatusEnabled, Tag: &ordinaryTag},
	}
	channels[1].SetOtherInfo(map[string]any{
		"disabled_until":   int64(2_000),
		"quota_domain_id":  disabledHash,
		"quota_generation": "17",
		"quota_type":       "plan",
	})
	channels[2].SetOtherInfo(map[string]any{
		"disabled_until":   int64(1_500),
		"quota_domain_id":  disabledHash,
		"quota_generation": "23",
		"quota_type":       "plan",
	})
	channels[3].SetOtherInfo(map[string]any{
		"disabled_until": int64(3_000),
		"quota_domain":   legacyTag,
		"quota_reset_at": int64(2_940),
		"quota_type":     "plan",
	})
	require.NoError(t, db.Create(&channels).Error)

	require.NoError(t, InitializePlanQuotaDomains())

	var domains []PlanQuotaDomain
	require.NoError(t, db.Order("credential_hash").Find(&domains).Error)
	require.Len(t, domains, 3)

	byHash := make(map[string]PlanQuotaDomain, len(domains))
	for _, domain := range domains {
		byHash[domain.CredentialHash] = domain
	}
	activeHash, ok := PlanQuotaDomainHash(activeCredential)
	require.True(t, ok)
	legacyHash, ok := PlanQuotaDomainHash(legacyCredential)
	require.True(t, ok)

	assert.Equal(t, PlanQuotaDomain{
		CredentialHash: activeHash,
		Generation:     0,
		State:          PlanQuotaDomainStateActive,
		DisabledUntil:  0,
	}, byHash[activeHash])
	assert.Equal(t, PlanQuotaDomain{
		CredentialHash: disabledHash,
		Generation:     23,
		State:          PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_000,
	}, byHash[disabledHash])
	assert.Equal(t, PlanQuotaDomain{
		CredentialHash: legacyHash,
		Generation:     0,
		State:          PlanQuotaDomainStateDisabled,
		DisabledUntil:  3_000,
	}, byHash[legacyHash])

	serialized, err := json.Marshal(domains)
	require.NoError(t, err)
	for _, credential := range []string{
		activeCredential,
		disabledCredential,
		legacyCredential,
		"authority-secret-multi-a",
		"authority-secret-multi-b",
	} {
		assert.NotContains(t, string(serialized), credential)
	}
}

func TestInitializePlanQuotaDomainsNormalizesGenerationAbsentMarkersForAutomaticRecovery(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tagA := "plan:test:generation-zero-a"
	tagB := "plan:test:generation-zero-b"
	const credential = "authority-secret-generation-zero"
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	channels := []Channel{
		{
			Name: "generation-zero-a", Key: credential, Tag: &tagA,
			Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "generation-zero-b", Key: credential, Tag: &tagB,
			Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
		},
	}
	channels[0].SetOtherInfo(map[string]any{
		"disabled_until":  int64(1),
		"quota_domain":    tagA,
		"quota_domain_id": hash,
		"quota_type":      "plan",
	})
	channels[1].SetOtherInfo(map[string]any{
		"disabled_until":  int64(2),
		"quota_domain":    tagB,
		"quota_domain_id": hash,
		"quota_type":      "plan",
	})
	require.NoError(t, db.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(db))
		require.NoError(t, db.Model(&Ability{}).
			Where("channel_id = ?", channels[index].Id).
			Update("enabled", true).Error)
	}

	require.NoError(t, InitializePlanQuotaDomains())

	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority, "credential_hash = ?", hash).Error)
	assert.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
	assert.Zero(t, authority.Generation)
	assert.Equal(t, int64(2), authority.DisabledUntil)
	for index := range channels {
		assertPlanQuotaDomainChannelState(
			t, db, channels[index].Id, common.ChannelStatusAutoDisabled, 0, 2,
		)
	}

	source, err := GetChannelById(channels[0].Id, true)
	require.NoError(t, err)
	result, err := RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
		Source: source, RecoveryAt: 3, RequireDue: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.NewlyEnabled)
	for index := range channels {
		assertPlanQuotaDomainChannelState(
			t, db, channels[index].Id, common.ChannelStatusEnabled, 0, 0,
		)
	}
}

func TestInitializePlanQuotaDomainsNormalizesConflictsForManualRecovery(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tagA := "plan:test:normalize-conflict-a"
	tagB := "plan:test:normalize-conflict-b"
	const credential = "authority-secret-normalize-conflict"
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	channels := []Channel{
		{
			Name: "normalize-conflict-a", Key: credential, Tag: &tagA,
			Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "normalize-conflict-b", Key: credential, Tag: &tagB,
			Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
		},
	}
	channels[0].SetOtherInfo(map[string]any{
		"disabled_until":   int64(2_000),
		"quota_domain":     tagA,
		"quota_domain_id":  hash,
		"quota_generation": "17",
		"quota_type":       "plan",
	})
	channels[1].SetOtherInfo(map[string]any{
		"disabled_until":   int64(1_500),
		"quota_domain":     tagB,
		"quota_domain_id":  hash,
		"quota_generation": "23",
		"quota_type":       "plan",
	})
	require.NoError(t, db.Create(&channels).Error)
	for index := range channels {
		require.NoError(t, channels[index].AddAbilities(db))
		require.NoError(t, db.Model(&Ability{}).
			Where("channel_id = ?", channels[index].Id).
			Update("enabled", true).Error)
	}

	require.NoError(t, InitializePlanQuotaDomains())

	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority, "credential_hash = ?", hash).Error)
	assert.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
	assert.Equal(t, int64(23), authority.Generation)
	assert.Equal(t, int64(2_000), authority.DisabledUntil)
	for index := range channels {
		assertPlanQuotaDomainChannelState(
			t, db, channels[index].Id, common.ChannelStatusAutoDisabled, 23, 2_000,
		)
	}

	source, err := GetChannelById(channels[0].Id, true)
	require.NoError(t, err)
	result, err := RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
		Source: source,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.NewlyEnabled)
	for index := range channels {
		assertPlanQuotaDomainChannelState(
			t, db, channels[index].Id, common.ChannelStatusEnabled, 0, 0,
		)
	}
}

func TestInitializePlanQuotaDomainsRejectsMalformedOwnership(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:malformed"
	channel := Channel{
		Name: "malformed", Key: "authority-secret-malformed",
		Status: common.ChannelStatusAutoDisabled, Tag: &tag,
	}
	channel.SetOtherInfo(map[string]any{
		"quota_domain_id":  strings.Repeat("0", 64),
		"quota_generation": "not-a-generation",
		"quota_type":       "plan",
	})
	require.NoError(t, db.Create(&channel).Error)

	require.Error(t, InitializePlanQuotaDomains())

	var count int64
	require.NoError(t, db.Model(&PlanQuotaDomain{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestInitializePlanQuotaDomainsRejectsMixedMarkerGenerations(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:mixed-marker-generation"
	const credential = "authority-secret-mixed-marker-generation"
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	channels := []Channel{
		{
			Name: "generation-absent", Key: credential, Tag: &tag,
			Status: common.ChannelStatusAutoDisabled,
		},
		{
			Name: "generation-present", Key: credential, Tag: &tag,
			Status: common.ChannelStatusAutoDisabled,
		},
	}
	channels[0].SetOtherInfo(map[string]any{
		"disabled_until":  int64(100),
		"quota_domain":    tag,
		"quota_domain_id": hash,
		"quota_type":      "plan",
	})
	channels[1].SetOtherInfo(map[string]any{
		"disabled_until":   int64(200),
		"quota_domain":     tag,
		"quota_domain_id":  hash,
		"quota_generation": "7",
		"quota_type":       "plan",
	})
	require.NoError(t, db.Create(&channels).Error)

	require.Error(t, InitializePlanQuotaDomains())

	var count int64
	require.NoError(t, db.Model(&PlanQuotaDomain{}).Count(&count).Error)
	assert.Zero(t, count)
	for index := range channels {
		var stored Channel
		require.NoError(t, db.First(&stored, channels[index].Id).Error)
		assert.Equal(t, channels[index].OtherInfo, stored.OtherInfo)
	}
}

func TestPlanQuotaDomainUsesExactlyOneSelectedKey(t *testing.T) {
	tag := "plan:test:selected-key"
	expectedHash, ok := PlanQuotaDomainHash("shared")
	require.True(t, ok)

	tests := []struct {
		name    string
		channel Channel
		want    bool
		hash    string
	}{
		{
			name: "trailing newline",
			channel: Channel{
				Key: "shared\n", Tag: &tag,
			},
			want: true,
			hash: expectedHash,
		},
		{
			name: "cached one-entry representation",
			channel: Channel{
				Key: "ignored", Keys: []string{"shared"}, Tag: &tag,
			},
			want: true,
			hash: expectedHash,
		},
		{
			name: "empty selected key",
			channel: Channel{
				Key: "\n", Tag: &tag,
			},
		},
		{
			name: "multiple selected keys",
			channel: Channel{
				Key: "shared\nother", Tag: &tag,
			},
		},
		{
			name: "case remains significant",
			channel: Channel{
				Key: "Shared", Tag: &tag,
			},
			want: true,
			hash: func() string {
				hash, valid := PlanQuotaDomainHash("Shared")
				require.True(t, valid)
				return hash
			}(),
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			hash, member := PlanQuotaDomainMembership(&testCase.channel)
			assert.Equal(t, testCase.want, member)
			assert.Equal(t, testCase.hash, hash)
		})
	}
}

func TestPlanQuotaDomainDisableIncludesTrailingNewlineMember(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:selected-key-disable"
	const credential = "shared"
	source := createPlanQuotaDomainFixture(
		t, db, credential, tag, PlanQuotaDomainStateActive, 0, 0,
	)
	joining := Channel{
		Name: "newline-member", Key: credential + "\n", Tag: &tag,
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, joining.Insert())

	result, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
		FailingChannelID:   source.Id,
		ObservedCredential: credential,
		ObservedTag:        tag,
		Reason:             "quota exhausted",
		ResetAt:            2_000_000_000,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.NewlyDisabled)

	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority).Error)
	assertPlanQuotaDomainChannelState(
		t, db, source.Id, common.ChannelStatusAutoDisabled,
		authority.Generation, authority.DisabledUntil,
	)
	assertPlanQuotaDomainChannelState(
		t, db, joining.Id, common.ChannelStatusAutoDisabled,
		authority.Generation, authority.DisabledUntil,
	)
}

func TestPlanQuotaDomainHonorsNamingStrategy(t *testing.T) {
	const prefix = "isolated_authority_"
	db := setupPlanQuotaAuthorityTest(t, prefix)
	tag := "plan:test:prefix"
	channel := Channel{
		Name: "prefixed", Key: "authority-secret-prefix",
		Status: common.ChannelStatusEnabled, Tag: &tag,
	}
	require.NoError(t, db.Create(&channel).Error)

	require.NoError(t, InitializePlanQuotaDomains())
	assert.True(t, db.Migrator().HasTable(&PlanQuotaDomain{}))

	var tables []string
	require.NoError(t, db.Raw(
		"SELECT name FROM sqlite_master WHERE type = ? AND name LIKE ?",
		"table",
		"%plan_quota_domains",
	).Scan(&tables).Error)
	assert.Equal(t, []string{prefix + "plan_quota_domains"}, tables)
}

func TestInitializePlanQuotaDomainsLocksExistingAuthority(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:backfill-lock"
	credential := "authority-secret-backfill-lock"
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	require.NoError(t, db.Create(&Channel{
		Name: "backfill-lock", Key: credential, Status: common.ChannelStatusEnabled, Tag: &tag,
	}).Error)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: hash,
		State:          PlanQuotaDomainStateActive,
	}).Error)

	var authorityLocked atomic.Bool
	callbackName := "test:plan_quota_backfill_lock"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "PlanQuotaDomain" {
			return
		}
		if _, exists := tx.Statement.Clauses["FOR"]; exists {
			authorityLocked.Store(true)
			delete(tx.Statement.Clauses, "FOR")
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	previousMainType := common.MainDatabaseType()
	previousLogType := common.LogDatabaseType()
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, previousLogType)
	t.Cleanup(func() {
		common.SetDatabaseTypes(previousMainType, previousLogType)
	})

	require.NoError(t, InitializePlanQuotaDomains())
	assert.True(t, authorityLocked.Load())
}

func createPlanQuotaDomainFixture(
	t *testing.T,
	db *gorm.DB,
	credential string,
	tag string,
	state string,
	generation int64,
	disabledUntil int64,
) Channel {
	t.Helper()

	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: hash,
		Generation:     generation,
		State:          state,
		DisabledUntil:  disabledUntil,
	}).Error)
	status := common.ChannelStatusEnabled
	channel := Channel{
		Name: "fixture", Key: credential, Tag: &tag, Status: status,
		Models: "gpt-4.1", Group: "default",
	}
	if state == PlanQuotaDomainStateDisabled {
		channel.Status = common.ChannelStatusAutoDisabled
		channel.SetOtherInfo(map[string]any{
			"disabled_until":   disabledUntil,
			"quota_domain":     tag,
			"quota_domain_id":  hash,
			"quota_generation": fmt.Sprintf("%d", generation),
			"quota_type":       "plan",
		})
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))
	return channel
}

func assertPlanQuotaDomainChannelState(
	t *testing.T,
	db *gorm.DB,
	channelID int,
	status int,
	generation int64,
	disabledUntil int64,
) {
	t.Helper()

	var channel Channel
	require.NoError(t, db.First(&channel, channelID).Error)
	assert.Equal(t, status, channel.Status)
	info := channel.GetOtherInfo()
	if status == common.ChannelStatusAutoDisabled {
		assert.Equal(t, fmt.Sprintf("%d", generation), info["quota_generation"])
		assert.EqualValues(t, disabledUntil, info["disabled_until"])
	} else {
		assert.NotContains(t, info, "quota_domain_id")
		assert.NotContains(t, info, "quota_generation")
		assert.NotContains(t, info, "quota_type")
	}
	var enabled int64
	require.NoError(t, db.Model(&Ability{}).
		Where("channel_id = ? AND enabled = ?", channelID, true).
		Count(&enabled).Error)
	if status == common.ChannelStatusEnabled {
		assert.Positive(t, enabled)
	} else {
		assert.Zero(t, enabled)
	}
}

func installFirstAuthorityLockBarrier(t *testing.T, db *gorm.DB) (<-chan struct{}, chan<- struct{}) {
	t.Helper()

	locked := make(chan struct{})
	release := make(chan struct{})
	var intercepted atomic.Bool
	callbackName := "test:plan_quota_authority_lock_barrier:" + strings.ReplaceAll(t.Name(), "/", "_")
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "PlanQuotaDomain" ||
			!intercepted.CompareAndSwap(false, true) {
			return
		}
		close(locked)
		<-release
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})
	return locked, release
}

func runSerializedPlanQuotaOperations(
	t *testing.T,
	locked <-chan struct{},
	release chan<- struct{},
	first func() error,
	second func() error,
) {
	t.Helper()

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- first()
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("first operation did not acquire the authority lock")
	}

	secondResult := make(chan error, 1)
	go func() {
		secondResult <- second()
	}()
	close(release)

	require.NoError(t, <-firstResult)
	require.NoError(t, <-secondResult)
}

func TestPlanQuotaDomainDisableSerializesConcurrentCreate(t *testing.T) {
	for _, firstOperation := range []string{"disable", "create"} {
		t.Run(firstOperation+"_first", func(t *testing.T) {
			db := setupPlanQuotaAuthorityTest(t, "")
			tag := "plan:test:create-race"
			credential := "authority-secret-create-race"
			source := createPlanQuotaDomainFixture(
				t, db, credential, tag, PlanQuotaDomainStateActive, 0, 0,
			)
			created := Channel{
				Name: "concurrent-create", Key: credential, Tag: &tag,
				Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
			}
			locked, release := installFirstAuthorityLockBarrier(t, db)
			disable := func() error {
				_, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
					FailingChannelID:   source.Id,
					ObservedCredential: credential,
					ObservedTag:        tag,
					Reason:             "quota exhausted",
					ResetAt:            2_000_000_000,
				})
				return err
			}
			create := func() error {
				return created.Insert()
			}
			if firstOperation == "disable" {
				runSerializedPlanQuotaOperations(t, locked, release, disable, create)
			} else {
				runSerializedPlanQuotaOperations(t, locked, release, create, disable)
			}

			var authority PlanQuotaDomain
			require.NoError(t, db.First(&authority).Error)
			require.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
			assertPlanQuotaDomainChannelState(
				t, db, source.Id, common.ChannelStatusAutoDisabled,
				authority.Generation, authority.DisabledUntil,
			)
			assertPlanQuotaDomainChannelState(
				t, db, created.Id, common.ChannelStatusAutoDisabled,
				authority.Generation, authority.DisabledUntil,
			)
		})
	}
}

func TestPlanQuotaDomainDisableSerializesConcurrentRotationIntoDomain(t *testing.T) {
	for _, firstOperation := range []string{"disable", "rotation"} {
		t.Run(firstOperation+"_first", func(t *testing.T) {
			db := setupPlanQuotaAuthorityTest(t, "")
			targetTag := "plan:test:rotation-target"
			targetCredential := "authority-secret-rotation-target"
			target := createPlanQuotaDomainFixture(
				t, db, targetCredential, targetTag, PlanQuotaDomainStateActive, 0, 0,
			)
			oldTag := "plan:test:rotation-old"
			oldCredential := "authority-secret-rotation-old"
			rotating := createPlanQuotaDomainFixture(
				t, db, oldCredential, oldTag, PlanQuotaDomainStateActive, 0, 0,
			)
			rotating.Key = targetCredential
			rotating.Tag = &targetTag

			locked, release := installFirstAuthorityLockBarrier(t, db)
			disable := func() error {
				_, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
					FailingChannelID:   target.Id,
					ObservedCredential: targetCredential,
					ObservedTag:        targetTag,
					Reason:             "quota exhausted",
					ResetAt:            2_000_000_100,
				})
				return err
			}
			rotate := func() error {
				return rotating.Update()
			}
			if firstOperation == "disable" {
				runSerializedPlanQuotaOperations(t, locked, release, disable, rotate)
			} else {
				runSerializedPlanQuotaOperations(t, locked, release, rotate, disable)
			}

			targetHash, ok := PlanQuotaDomainHash(targetCredential)
			require.True(t, ok)
			var authority PlanQuotaDomain
			require.NoError(t, db.First(&authority, "credential_hash = ?", targetHash).Error)
			require.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
			assertPlanQuotaDomainChannelState(
				t, db, target.Id, common.ChannelStatusAutoDisabled,
				authority.Generation, authority.DisabledUntil,
			)
			assertPlanQuotaDomainChannelState(
				t, db, rotating.Id, common.ChannelStatusAutoDisabled,
				authority.Generation, authority.DisabledUntil,
			)
		})
	}
}

func TestPlanQuotaDomainRecoverySerializesConcurrentCreate(t *testing.T) {
	for _, firstOperation := range []string{"recovery", "create"} {
		t.Run(firstOperation+"_first", func(t *testing.T) {
			db := setupPlanQuotaAuthorityTest(t, "")
			tag := "plan:test:recovery-create-race"
			credential := "authority-secret-recovery-create-race"
			source := createPlanQuotaDomainFixture(
				t, db, credential, tag, PlanQuotaDomainStateDisabled, 41, 1,
			)
			created := Channel{
				Name: "concurrent-recovery-create", Key: credential, Tag: &tag,
				Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
			}

			locked, release := installFirstAuthorityLockBarrier(t, db)
			recoverDomain := func() error {
				snapshot, err := GetChannelById(source.Id, true)
				if err != nil {
					return err
				}
				_, err = RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
					Source:     snapshot,
					RecoveryAt: 2,
					RequireDue: true,
				})
				return err
			}
			create := func() error {
				return created.Insert()
			}
			if firstOperation == "recovery" {
				runSerializedPlanQuotaOperations(t, locked, release, recoverDomain, create)
			} else {
				runSerializedPlanQuotaOperations(t, locked, release, create, recoverDomain)
			}

			var authority PlanQuotaDomain
			require.NoError(t, db.First(&authority).Error)
			assert.Equal(t, PlanQuotaDomainStateActive, authority.State)
			assert.Greater(t, authority.Generation, int64(41))
			assertPlanQuotaDomainChannelState(
				t, db, source.Id, common.ChannelStatusEnabled, 0, 0,
			)
			assertPlanQuotaDomainChannelState(
				t, db, created.Id, common.ChannelStatusEnabled, 0, 0,
			)
		})
	}
}

func TestPlanQuotaDomainUpdateLocksOldAndNewHashesInOrder(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	oldTag := "plan:test:lock-old"
	newTag := "plan:test:lock-new"
	oldCredential := "authority-secret-lock-z"
	newCredential := "authority-secret-lock-a"
	channel := createPlanQuotaDomainFixture(
		t, db, oldCredential, oldTag, PlanQuotaDomainStateActive, 0, 0,
	)
	newHash, ok := PlanQuotaDomainHash(newCredential)
	require.True(t, ok)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: newHash,
		State:          PlanQuotaDomainStateActive,
	}).Error)

	var lockedHashes []string
	var lockMutex sync.Mutex
	callbackName := "test:plan_quota_authority_lock_order"
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "PlanQuotaDomain" {
			return
		}
		domain, isDomain := tx.Statement.Dest.(*PlanQuotaDomain)
		if !isDomain || domain.CredentialHash == "" {
			return
		}
		lockMutex.Lock()
		lockedHashes = append(lockedHashes, domain.CredentialHash)
		lockMutex.Unlock()
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	channel.Key = newCredential
	channel.Tag = &newTag
	require.NoError(t, channel.Update())

	oldHash, ok := PlanQuotaDomainHash(oldCredential)
	require.True(t, ok)
	expected := []string{oldHash, newHash}
	sort.Strings(expected)
	assert.Equal(t, expected, lockedHashes)
}

func TestPlanQuotaDomainMissingAuthorityFailsClosed(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:missing-authority"
	existing := Channel{
		Name: "unfenced-existing", Key: "authority-secret-missing",
		Tag: &tag, Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&existing).Error)

	joining := Channel{
		Name: "joining", Key: existing.Key, Tag: &tag,
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
	}
	require.Error(t, joining.Insert())
	assert.Zero(t, joining.Id)

	existing.Name = "must-not-update"
	require.Error(t, existing.Update())
	var stored Channel
	require.NoError(t, db.First(&stored, existing.Id).Error)
	assert.NotEqual(t, existing.Name, stored.Name)
}

func TestPlanQuotaDomainUnknownWriteIsNotRetried(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	require.NoError(t, db.Exec(
		"CREATE TABLE commit_failure_parents (id INTEGER PRIMARY KEY)",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE TABLE commit_failure_children ("+
			"id INTEGER PRIMARY KEY, parent_id INTEGER, "+
			"FOREIGN KEY(parent_id) REFERENCES commit_failure_parents(id) "+
			"DEFERRABLE INITIALLY DEFERRED)",
	).Error)
	tag := "plan:test:unknown-write"
	channel := Channel{
		Name: "unknown-write", Key: "authority-secret-unknown-write",
		Tag: &tag, Status: common.ChannelStatusEnabled,
		Models: "gpt-4.1", Group: "default",
	}

	var writes atomic.Int32
	callbackName := "test:plan_quota_unknown_write"
	require.NoError(t, db.Callback().Create().After("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "Channel" {
			return
		}
		writes.Add(1)
		tx.AddError(tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
			Exec("INSERT INTO commit_failure_children (id, parent_id) VALUES (?, ?)", 1, 999).
			Error)
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Create().Remove(callbackName))
	})

	require.Error(t, channel.Insert())
	assert.Equal(t, int32(1), writes.Load())
}

func TestChannelStatusEnableCannotBypassDisabledPlanQuotaDomain(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:status-enable"
	channel := createPlanQuotaDomainFixture(
		t, db, "authority-secret-status-enable", tag,
		PlanQuotaDomainStateDisabled, 51, 2_000_000_000,
	)

	assert.False(t, UpdateChannelStatus(
		channel.Id,
		"",
		common.ChannelStatusEnabled,
		"generic enable",
	))
	assertPlanQuotaDomainChannelState(
		t, db, channel.Id, common.ChannelStatusAutoDisabled, 51, 2_000_000_000,
	)
}

func TestUpdateChannelStatusRejectsStaleMultiKeyToDisabledSingleKeyRotation(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	tag := "plan:test:stale-multi-key-status"
	const credential = "authority-secret-stale-multi-key-status"
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: hash,
		Generation:     41,
		State:          PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_000_000_000,
	}).Error)
	channel := Channel{
		Name: "stale-multi-key-status", Key: "key-a\nkey-b", Tag: &tag,
		Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusAutoDisabled,
			},
		},
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))

	snapshotLoaded := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	var intercepted atomic.Bool
	callbackName := "test:stale_multi_key_status_rotation"
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "Channel" {
			return
		}
		current, isChannel := tx.Statement.Dest.(*Channel)
		if !isChannel ||
			current.Id != channel.Id ||
			!current.ChannelInfo.IsMultiKey ||
			!intercepted.CompareAndSwap(false, true) {
			return
		}
		close(snapshotLoaded)
		<-releaseSnapshot
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	statusResult := make(chan bool, 1)
	go func() {
		statusResult <- UpdateChannelStatus(
			channel.Id,
			"key-a",
			common.ChannelStatusEnabled,
			"stale recovery",
		)
	}()
	<-snapshotLoaded

	rotatedInfo := map[string]any{
		"disabled_until":   int64(2_000_000_000),
		"quota_domain":     tag,
		"quota_domain_id":  hash,
		"quota_generation": "41",
		"quota_type":       "plan",
	}
	rotated := Channel{}
	rotated.SetOtherInfo(rotatedInfo)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Channel{}).
			Where("id = ?", channel.Id).
			Updates(map[string]any{
				"key":          credential,
				"status":       common.ChannelStatusAutoDisabled,
				"other_info":   rotated.OtherInfo,
				"channel_info": ChannelInfo{},
			}).Error; err != nil {
			return err
		}
		return tx.Model(&Ability{}).
			Where("channel_id = ?", channel.Id).
			Update("enabled", false).Error
	}))
	close(releaseSnapshot)

	assert.False(t, <-statusResult)
	assertPlanQuotaDomainChannelState(
		t, db, channel.Id, common.ChannelStatusAutoDisabled, 41, 2_000_000_000,
	)
	var stored Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.False(t, stored.ChannelInfo.IsMultiKey)
}

func TestSaveChannelInfoRejectsStaleMultiKeyToDisabledSingleKeyRotation(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	tag := "plan:test:stale-polling-status"
	const credential = "authority-secret-stale-polling-status"
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: hash,
		Generation:     51,
		State:          PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_100_000_000,
	}).Error)
	channel := Channel{
		Name: "stale-polling-status", Key: "key-a\nkey-b", Tag: &tag,
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
			MultiKeyMode: constant.MultiKeyModePolling,
		},
	}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, channel.AddAbilities(db))
	stale, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	rotated := Channel{}
	rotated.SetOtherInfo(map[string]any{
		"disabled_until":   int64(2_100_000_000),
		"quota_domain":     tag,
		"quota_domain_id":  hash,
		"quota_generation": "51",
		"quota_type":       "plan",
	})
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Channel{}).
			Where("id = ?", channel.Id).
			Updates(map[string]any{
				"key":          credential,
				"status":       common.ChannelStatusAutoDisabled,
				"other_info":   rotated.OtherInfo,
				"channel_info": ChannelInfo{},
			}).Error; err != nil {
			return err
		}
		return tx.Model(&Ability{}).
			Where("channel_id = ?", channel.Id).
			Update("enabled", false).Error
	}))

	stale.ChannelInfo.MultiKeyPollingIndex = 1
	require.NoError(t, stale.SaveChannelInfo())

	assertPlanQuotaDomainChannelState(
		t, db, channel.Id, common.ChannelStatusAutoDisabled, 51, 2_100_000_000,
	)
	var stored Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.False(t, stored.ChannelInfo.IsMultiKey)
}

func TestPlanQuotaDomainDisableRollbackIncludesAuthorityAndAbilities(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tagA := "plan:test:disable-rollback-a"
	tagB := "plan:test:disable-rollback-b"
	credential := "authority-secret-disable-rollback"
	first := createPlanQuotaDomainFixture(
		t, db, credential, tagA, PlanQuotaDomainStateActive, 0, 0,
	)
	second := Channel{
		Name: "second", Key: credential, Tag: &tagB,
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&second).Error)
	require.NoError(t, second.AddAbilities(db))

	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	InitChannelCache()
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	forcedErr := errors.New("forced domain ability failure")
	var abilityWrites atomic.Int32
	callbackName := "test:plan_quota_disable_rollback"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "Ability" {
			return
		}
		if abilityWrites.Add(1) == 2 {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Update().Remove(callbackName))
	})

	result, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
		FailingChannelID:   first.Id,
		ObservedCredential: credential,
		ObservedTag:        tagA,
		Reason:             "quota exhausted",
		ResetAt:            2_000_000_000,
	})
	require.ErrorIs(t, err, forcedErr)
	assert.Empty(t, result.Channels)

	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority).Error)
	assert.Equal(t, PlanQuotaDomainStateActive, authority.State)
	assert.Zero(t, authority.Generation)
	for _, channelID := range []int{first.Id, second.Id} {
		assertPlanQuotaDomainChannelState(
			t, db, channelID, common.ChannelStatusEnabled, 0, 0,
		)
		cached, cacheErr := CacheGetChannel(channelID)
		require.NoError(t, cacheErr)
		assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
	}
}

func TestPlanQuotaDomainCreateAfterDisableInheritsGeneration(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:create-after-disable"
	credential := "authority-secret-create-after-disable"
	source := createPlanQuotaDomainFixture(
		t, db, credential, tag, PlanQuotaDomainStateActive, 0, 0,
	)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	InitChannelCache()
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
	result, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
		FailingChannelID:   source.Id,
		ObservedCredential: credential,
		ObservedTag:        tag,
		Reason:             "quota exhausted",
		ResetAt:            2_000_000_000,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.NewlyDisabled)

	created := Channel{
		Name: "after-disable", Key: credential, Tag: &tag,
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, created.Insert())

	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority).Error)
	var disabledSource Channel
	require.NoError(t, db.First(&disabledSource, source.Id).Error)
	assert.Equal(t, "quota exhausted", disabledSource.GetOtherInfo()["status_reason"])
	assertPlanQuotaDomainChannelState(
		t, db, created.Id, common.ChannelStatusAutoDisabled,
		authority.Generation, authority.DisabledUntil,
	)
	cached, err := CacheGetChannel(created.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	selected, err := GetRandomSatisfiedChannel(created.Group, created.Models, 0, nil)
	require.NoError(t, err)
	assert.Nil(t, selected)
}

func TestPlanQuotaDomainDisableSerializesCommitThroughCachePublication(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:disable-publication-order"
	const credential = "authority-secret-disable-publication-order"
	source := createPlanQuotaDomainFixture(
		t, db, credential, tag, PlanQuotaDomainStateActive, 0, 0,
	)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	InitChannelCache()
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
	barrier := installChannelPublicationBarrier(t)

	firstResult := make(chan error, 1)
	go func() {
		_, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
			FailingChannelID:   source.Id,
			ObservedCredential: credential,
			ObservedTag:        tag,
			Reason:             "first quota event",
			ResetAt:            100,
		})
		firstResult <- err
	}()
	<-barrier.firstCommitted

	publicationLocked := !channelStatusLock.TryLock()
	if !publicationLocked {
		channelStatusLock.Unlock()
	}
	require.True(t, publicationLocked, "the publication lock must span commit through cache publication")

	secondResult := make(chan error, 1)
	go func() {
		_, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
			FailingChannelID:   source.Id,
			ObservedCredential: credential,
			ObservedTag:        tag,
			Reason:             "second quota event",
			ResetAt:            200,
		})
		secondResult <- err
	}()
	<-barrier.secondAttempted
	barrier.Release()

	require.NoError(t, <-firstResult)
	require.NoError(t, <-secondResult)

	var stored Channel
	require.NoError(t, db.First(&stored, source.Id).Error)
	cached, err := CacheGetChannel(source.Id)
	require.NoError(t, err)
	assert.Equal(t, stored.Status, cached.Status)
	assert.Equal(t, stored.OtherInfo, cached.OtherInfo)
	assert.EqualValues(t, 260, cached.GetOtherInfo()["disabled_until"])
	routingIDs, err := ListSatisfiedChannelIDsAtPriority(
		source.Group,
		source.Models,
		source.GetPriority(),
		nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, routingIDs, source.Id)
}

func TestPlanQuotaDomainBatchInsertInheritsDisabledAuthority(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:batch-insert"
	credential := "authority-secret-batch-insert"
	createPlanQuotaDomainFixture(
		t, db, credential, tag, PlanQuotaDomainStateDisabled, 31, 2_000_000_000,
	)
	channels := []Channel{
		{
			Name: "batch-first", Key: credential, Tag: &tag,
			Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
		},
		{
			Name: "batch-second", Key: credential, Tag: &tag,
			Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
		},
	}

	require.NoError(t, BatchInsertChannels(channels))
	for index := range channels {
		assertPlanQuotaDomainChannelState(
			t,
			db,
			channels[index].Id,
			common.ChannelStatusAutoDisabled,
			31,
			2_000_000_000,
		)
	}
}

func TestPlanQuotaDomainSingleToMultiKeyClearsSharedOwnership(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:single-to-multi"
	channel := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-single-to-multi",
		tag,
		PlanQuotaDomainStateDisabled,
		41,
		2_000_000_000,
	)
	channel.Key += "\nauthority-secret-second-key"
	channel.ChannelInfo = ChannelInfo{
		IsMultiKey:   true,
		MultiKeySize: 2,
	}

	require.NoError(t, channel.Update())

	var stored Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	require.True(t, stored.ChannelInfo.IsMultiKey)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	info := stored.GetOtherInfo()
	assert.NotContains(t, info, "quota_domain")
	assert.NotContains(t, info, "quota_domain_id")
	assert.NotContains(t, info, "quota_generation")
	assert.NotContains(t, info, "quota_type")
	assert.NotContains(t, info, "quota_reset_at")
	assert.NotContains(t, info, "disabled_until")
}

func TestPlanQuotaDomainRecoveryIsAtomic(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tagA := "plan:test:recovery-rollback-a"
	tagB := "plan:test:recovery-rollback-b"
	credential := "authority-secret-recovery-rollback"
	first := createPlanQuotaDomainFixture(
		t, db, credential, tagA, PlanQuotaDomainStateDisabled, 61, 1,
	)
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	second := Channel{
		Name: "second", Key: credential, Tag: &tagB,
		Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
	}
	second.SetOtherInfo(map[string]any{
		"disabled_until":   int64(1),
		"quota_domain":     tagB,
		"quota_domain_id":  hash,
		"quota_generation": "61",
		"quota_type":       "plan",
	})
	require.NoError(t, db.Create(&second).Error)
	require.NoError(t, second.AddAbilities(db))

	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	InitChannelCache()
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	forcedErr := errors.New("forced recovery ability failure")
	var abilityWrites atomic.Int32
	callbackName := "test:plan_quota_recovery_rollback"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "Ability" {
			return
		}
		if abilityWrites.Add(1) == 2 {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Update().Remove(callbackName))
	})

	snapshot, err := GetChannelById(first.Id, true)
	require.NoError(t, err)
	result, err := RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
		Source: snapshot, RecoveryAt: 2, RequireDue: true,
	})
	require.ErrorIs(t, err, forcedErr)
	assert.Empty(t, result.Channels)

	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority).Error)
	assert.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
	assert.Equal(t, int64(61), authority.Generation)
	for _, channelID := range []int{first.Id, second.Id} {
		assertPlanQuotaDomainChannelState(
			t, db, channelID, common.ChannelStatusAutoDisabled, 61, 1,
		)
		cached, cacheErr := CacheGetChannel(channelID)
		require.NoError(t, cacheErr)
		assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	}
}

func TestPlanQuotaDomainRecoveryRejectsStaleAuthorityGeneration(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:stale-generation"
	credential := "authority-secret-stale-generation"
	source := createPlanQuotaDomainFixture(
		t, db, credential, tag, PlanQuotaDomainStateDisabled, 71, 1,
	)
	snapshot, err := GetChannelById(source.Id, true)
	require.NoError(t, err)
	require.NoError(t, db.Model(&PlanQuotaDomain{}).
		Where("credential_hash <> ?", "").
		Update("generation", int64(72)).Error)

	result, err := RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
		Source: snapshot, RecoveryAt: 2, RequireDue: true,
	})
	require.NoError(t, err)
	assert.Empty(t, result.Channels)

	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority).Error)
	assert.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
	assert.Equal(t, int64(72), authority.Generation)
	assertPlanQuotaDomainChannelState(
		t, db, source.Id, common.ChannelStatusAutoDisabled, 71, 1,
	)
}

func TestPlanQuotaDomainRecoveryClearsQuotaOwnerButPreservesManagedRouteDisable(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	sourceTag := "plan:test:managed-route-source"
	managedTag := "plan:managed:test:managed-route-peer"
	credential := "authority-secret-managed-route-recovery"
	source := createPlanQuotaDomainFixture(
		t, db, credential, sourceTag, PlanQuotaDomainStateDisabled, 81, 1,
	)
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	managed := Channel{
		Name: "managed", Key: credential, Tag: &managedTag,
		Status: common.ChannelStatusAutoDisabled, Models: "gpt-4.1", Group: "default",
	}
	managed.SetOtherInfo(map[string]any{
		"disabled_until":   int64(1),
		"quota_domain":     managedTag,
		"quota_domain_id":  hash,
		"quota_generation": "81",
		"quota_type":       "plan",
	})
	require.NoError(t, db.Create(&managed).Error)
	require.NoError(t, managed.AddAbilities(db))
	require.NoError(t, db.Create(&UpstreamManagedRoute{
		SourceID:        1,
		ExternalGroupID: "managed-route-peer",
		Platform:        "plan",
		Protocol:        UpstreamProtocolOpenAI,
		ChannelID:       managed.Id,
		State:           UpstreamRouteStatePaused,
		Rank:            0,
	}).Error)

	snapshot, err := GetChannelById(source.Id, true)
	require.NoError(t, err)
	result, err := RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
		Source: snapshot, RecoveryAt: 2, RequireDue: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.NewlyEnabled)

	assertPlanQuotaDomainChannelState(
		t, db, source.Id, common.ChannelStatusEnabled, 0, 0,
	)
	var storedManaged Channel
	require.NoError(t, db.First(&storedManaged, managed.Id).Error)
	assert.Equal(t, common.ChannelStatusAutoDisabled, storedManaged.Status)
	assert.NotContains(t, storedManaged.GetOtherInfo(), "quota_domain_id")
	assert.NotContains(t, storedManaged.GetOtherInfo(), "quota_generation")
	var managedEnabled int64
	require.NoError(t, db.Model(&Ability{}).
		Where("channel_id = ? AND enabled = ?", managed.Id, true).
		Count(&managedEnabled).Error)
	assert.Zero(t, managedEnabled)
}

func TestPlanQuotaDomainBatchTagMutationEntersDisabledAuthority(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	planTag := "plan:test:batch-tag"
	ordinaryTag := "ordinary:test:batch-tag"
	credential := "authority-secret-batch-tag"
	source := createPlanQuotaDomainFixture(
		t, db, credential, planTag, PlanQuotaDomainStateDisabled, 91, 2_000_000_000,
	)
	joining := Channel{
		Name: "joining", Key: credential, Tag: &ordinaryTag,
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&joining).Error)
	require.NoError(t, joining.AddAbilities(db))

	require.NoError(t, BatchSetChannelTag([]int{joining.Id}, &planTag))

	var authority PlanQuotaDomain
	hash, ok := PlanQuotaDomainHash(credential)
	require.True(t, ok)
	require.NoError(t, db.First(&authority, "credential_hash = ?", hash).Error)
	assertPlanQuotaDomainChannelState(
		t, db, source.Id, common.ChannelStatusAutoDisabled,
		authority.Generation, authority.DisabledUntil,
	)
	assertPlanQuotaDomainChannelState(
		t, db, joining.Id, common.ChannelStatusAutoDisabled,
		authority.Generation, authority.DisabledUntil,
	)
}

func TestPlanQuotaDomainEditTagMutationEntersDisabledAuthority(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	planTag := "plan:test:edit-tag"
	ordinaryTag := "ordinary:test:edit-tag"
	credential := "authority-secret-edit-tag"
	createPlanQuotaDomainFixture(
		t, db, credential, planTag, PlanQuotaDomainStateDisabled, 101, 2_000_000_000,
	)
	joining := Channel{
		Name: "joining", Key: credential, Tag: &ordinaryTag,
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&joining).Error)
	require.NoError(t, joining.AddAbilities(db))

	require.NoError(t, EditChannelByTag(
		ordinaryTag,
		&planTag,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	))

	assertPlanQuotaDomainChannelState(
		t, db, joining.Id, common.ChannelStatusAutoDisabled, 101, 2_000_000_000,
	)
}

func TestPlanQuotaDomainCredentialRotationUsesSnapshotFence(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	oldTag := "plan:test:credential-old"
	newTag := "plan:test:credential-new"
	oldCredential := "authority-secret-credential-old"
	newCredential := "authority-secret-credential-new"
	channel := createPlanQuotaDomainFixture(
		t, db, oldCredential, oldTag, PlanQuotaDomainStateActive, 0, 0,
	)
	newHash, ok := PlanQuotaDomainHash(newCredential)
	require.True(t, ok)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: newHash,
		Generation:     111,
		State:          PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_000_000_000,
	}).Error)

	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.NoError(t, db.Model(&Channel{}).
		Where("id = ?", channel.Id).
		Update("tag", newTag).Error)

	updated, changed, err := UpdateChannelCredentialIfUnchanged(expected, newCredential)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, updated)

	var stored Channel
	require.NoError(t, db.First(&stored, channel.Id).Error)
	assert.Equal(t, oldCredential, stored.Key)
	assert.Equal(t, newTag, stored.GetTag())
}

func TestPlanQuotaDomainCredentialRotationIntoDisabledAuthority(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:credential-disabled"
	oldCredential := "authority-secret-credential-source"
	newCredential := "authority-secret-credential-disabled"
	channel := createPlanQuotaDomainFixture(
		t, db, oldCredential, tag, PlanQuotaDomainStateActive, 0, 0,
	)
	newHash, ok := PlanQuotaDomainHash(newCredential)
	require.True(t, ok)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: newHash,
		Generation:     121,
		State:          PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_000_000_000,
	}).Error)
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	updated, changed, err := UpdateChannelCredentialIfUnchanged(expected, newCredential)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, updated)
	assert.Equal(t, newCredential, updated.Key)
	assertPlanQuotaDomainChannelState(
		t, db, channel.Id, common.ChannelStatusAutoDisabled, 121, 2_000_000_000,
	)
}

func TestPlanQuotaDomainRotationIntoDisabledAuthorityPreservesIndependentDisable(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:credential-independent-disable"
	oldCredential := "authority-secret-independent-source"
	newCredential := "authority-secret-independent-target"
	channel := createPlanQuotaDomainFixture(
		t, db, oldCredential, tag, PlanQuotaDomainStateActive, 0, 0,
	)
	independentInfo := map[string]any{
		"disabled_until": int64(2_100_000_000),
		"quota_type":     "provider",
		"status_reason":  "provider maintenance",
	}
	channel.Status = common.ChannelStatusAutoDisabled
	channel.SetOtherInfo(independentInfo)
	require.NoError(t, db.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
		"status":     channel.Status,
		"other_info": channel.OtherInfo,
	}).Error)
	require.NoError(t, db.Model(&Ability{}).Where("channel_id = ?", channel.Id).
		Update("enabled", false).Error)

	newHash, ok := PlanQuotaDomainHash(newCredential)
	require.True(t, ok)
	require.NoError(t, db.Create(&PlanQuotaDomain{
		CredentialHash: newHash,
		Generation:     122,
		State:          PlanQuotaDomainStateDisabled,
		DisabledUntil:  2_000_000_000,
	}).Error)
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	updated, changed, err := UpdateChannelCredentialIfUnchanged(expected, newCredential)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, updated)
	assert.Equal(t, common.ChannelStatusAutoDisabled, updated.Status)
	assert.Equal(t, "provider", updated.GetOtherInfo()["quota_type"])
	assert.Equal(t, "provider maintenance", updated.GetOtherInfo()["status_reason"])
	assert.NotContains(t, updated.GetOtherInfo(), "quota_domain_id")
	var enabled int64
	require.NoError(t, db.Model(&Ability{}).
		Where("channel_id = ? AND enabled = ?", channel.Id, true).
		Count(&enabled).Error)
	assert.Zero(t, enabled)
}

func TestPlanQuotaDomainTagEnableCannotBypassDisabledAuthority(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:tag-enable"
	channel := createPlanQuotaDomainFixture(
		t, db, "authority-secret-tag-enable", tag,
		PlanQuotaDomainStateDisabled, 131, 2_000_000_000,
	)

	require.Error(t, EnableChannelByTag(tag))
	assertPlanQuotaDomainChannelState(
		t, db, channel.Id, common.ChannelStatusAutoDisabled, 131, 2_000_000_000,
	)
}

func TestPlanQuotaDomainBulkEnableCannotBypassDisabledAuthority(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	tag := "plan:test:bulk-enable"
	channel := createPlanQuotaDomainFixture(
		t, db, "authority-secret-bulk-enable", tag,
		PlanQuotaDomainStateDisabled, 141, 2_000_000_000,
	)
	expected, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)

	changed, err := UpdateSingleKeyChannelStatusesIfUnchanged(
		[]SingleKeyChannelStatusUpdate{{
			Expected:  expected,
			Status:    common.ChannelStatusEnabled,
			OtherInfo: expected.OtherInfo,
		}},
	)
	require.Error(t, err)
	assert.False(t, changed)
	assertPlanQuotaDomainChannelState(
		t, db, channel.Id, common.ChannelStatusAutoDisabled, 141, 2_000_000_000,
	)
}

func TestPlanQuotaDomainConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		databaseType common.DatabaseType
		dialector    func(string) gorm.Dialector
	}{
		{
			name:         "mysql",
			env:          "TEST_MYSQL_DSN",
			databaseType: common.DatabaseTypeMySQL,
			dialector: func(dsn string) gorm.Dialector {
				return mysql.Open(dsn)
			},
		},
		{
			name:         "postgres",
			env:          "TEST_POSTGRES_DSN",
			databaseType: common.DatabaseTypePostgreSQL,
			dialector: func(dsn string) gorm.Dialector {
				return postgres.New(postgres.Config{
					DSN:                  dsn,
					PreferSimpleProtocol: true,
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skip(test.env + " is not configured")
			}

			tablePrefix := fmt.Sprintf("plan_domain_%d_%d_", os.Getpid(), time.Now().UnixNano())
			database, err := gorm.Open(test.dialector(dsn), &gorm.Config{
				NamingStrategy: schema.NamingStrategy{TablePrefix: tablePrefix},
			})
			require.NoError(t, err)
			sqlDB, err := database.DB()
			require.NoError(t, err)

			previousDB := DB
			previousMainType := common.MainDatabaseType()
			previousLogType := common.LogDatabaseType()
			previousMemoryCacheEnabled := common.MemoryCacheEnabled
			DB = database
			common.SetDatabaseTypes(test.databaseType, previousLogType)
			initCol()
			common.MemoryCacheEnabled = false
			t.Cleanup(func() {
				require.NoError(t, database.Migrator().DropTable(
					&UpstreamManagedRoute{},
					&Ability{},
					&PlanQuotaDomain{},
					&Channel{},
				))
				require.NoError(t, sqlDB.Close())
				DB = previousDB
				common.SetDatabaseTypes(previousMainType, previousLogType)
				initCol()
				common.MemoryCacheEnabled = previousMemoryCacheEnabled
			})

			require.NoError(t, database.AutoMigrate(
				&Channel{},
				&PlanQuotaDomain{},
				&Ability{},
				&UpstreamManagedRoute{},
			))
			tag := "plan:test:configured"
			credential := "authority-secret-configured"
			source := Channel{
				Name: "configured-source", Key: credential, Tag: &tag,
				Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
			}
			require.NoError(t, source.Insert())
			disabled, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
				FailingChannelID:   source.Id,
				ObservedCredential: credential,
				ObservedTag:        tag,
				Reason:             "quota exhausted",
				ResetAt:            2_000_000_000,
			})
			require.NoError(t, err)
			require.Equal(t, 1, disabled.NewlyDisabled)

			joining := Channel{
				Name: "configured-joining", Key: credential, Tag: &tag,
				Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
			}
			require.NoError(t, joining.Insert())
			snapshot, err := GetChannelById(source.Id, true)
			require.NoError(t, err)
			recovered, err := RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
				Source: snapshot,
			})
			require.NoError(t, err)
			assert.Equal(t, 2, recovered.NewlyEnabled)
		})
	}
}
