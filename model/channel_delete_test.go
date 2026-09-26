package model

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func enableChannelDeleteCache(t *testing.T) {
	t.Helper()

	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	InitChannelCache()
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
}

func disableChannelDeleteCache(t *testing.T) {
	t.Helper()

	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})
}

func assertChannelDeleteRolledBack(t *testing.T, db *gorm.DB, channelID int) {
	t.Helper()

	var channel Channel
	require.NoError(t, db.First(&channel, channelID).Error)
	var abilities int64
	require.NoError(t, db.Model(&Ability{}).Where("channel_id = ?", channelID).Count(&abilities).Error)
	assert.Positive(t, abilities)
	cached, err := CacheGetChannel(channelID)
	require.NoError(t, err)
	assert.Equal(t, channel.Id, cached.Id)
}

func assertChannelDeleted(t *testing.T, db *gorm.DB, channelID int) {
	t.Helper()

	var channel Channel
	result := db.Where("id = ?", channelID).Limit(1).Find(&channel)
	require.NoError(t, result.Error)
	assert.Zero(t, result.RowsAffected)
	var abilities int64
	require.NoError(t, db.Model(&Ability{}).Where("channel_id = ?", channelID).Count(&abilities).Error)
	assert.Zero(t, abilities)
	if common.MemoryCacheEnabled {
		_, err := CacheGetChannel(channelID)
		assert.Error(t, err)
	}
}

func TestBatchDeleteChannelsLocksDomainsThenChannelsAndPublishesCacheDeletion(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	first := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-delete-z",
		"plan:test:delete-z",
		PlanQuotaDomainStateActive,
		0,
		0,
	)
	second := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-delete-a",
		"plan:test:delete-a",
		PlanQuotaDomainStateActive,
		0,
		0,
	)
	ordinary := Channel{
		Name: "ordinary-delete", Key: "ordinary-secret",
		Status: common.ChannelStatusEnabled, Models: "gpt-4.1", Group: "default",
	}
	require.NoError(t, db.Create(&ordinary).Error)
	require.NoError(t, ordinary.AddAbilities(db))
	enableChannelDeleteCache(t)

	var lockedHashes []string
	var lockedChannelIDs []int
	var lockMutex sync.Mutex
	callbackName := "test:channel_delete_lock_order"
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Schema == nil {
			return
		}
		if _, inTransaction := tx.Statement.ConnPool.(*sql.Tx); !inTransaction {
			return
		}
		lockMutex.Lock()
		defer lockMutex.Unlock()
		switch tx.Statement.Schema.Name {
		case "PlanQuotaDomain":
			domain, ok := tx.Statement.Dest.(*PlanQuotaDomain)
			if ok && domain.CredentialHash != "" {
				lockedHashes = append(lockedHashes, domain.CredentialHash)
			}
		case "Channel":
			channel, ok := tx.Statement.Dest.(*Channel)
			if ok && channel.Id != 0 {
				lockedChannelIDs = append(lockedChannelIDs, channel.Id)
			}
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	deleted, err := BatchDeleteChannels([]int{first.Id, ordinary.Id, second.Id, first.Id, 999_999})
	require.NoError(t, err)
	assert.Equal(t, int64(3), deleted)

	expectedHashes := make([]string, 0, 2)
	for _, credential := range []string{first.Key, second.Key} {
		hash, ok := PlanQuotaDomainHash(credential)
		require.True(t, ok)
		expectedHashes = append(expectedHashes, hash)
	}
	sort.Strings(expectedHashes)
	assert.Equal(t, expectedHashes, lockedHashes)
	expectedChannelIDs := []int{first.Id, second.Id, ordinary.Id}
	sort.Ints(expectedChannelIDs)
	assert.Equal(t, expectedChannelIDs, lockedChannelIDs)

	for _, channelID := range expectedChannelIDs {
		assertChannelDeleted(t, db, channelID)
	}
	var authorities int64
	require.NoError(t, db.Model(&PlanQuotaDomain{}).Count(&authorities).Error)
	assert.Equal(t, int64(2), authorities)
}

func TestChannelDeleteSerializesCommitThroughCacheDeletion(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	channel := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-delete-publication",
		"plan:test:delete-publication",
		PlanQuotaDomainStateActive,
		0,
		0,
	)
	enableChannelDeleteCache(t)
	barrier := installChannelPublicationBarrier(t)

	deleteResult := make(chan error, 1)
	go func() {
		deleteResult <- (&Channel{Id: channel.Id}).Delete()
	}()
	<-barrier.firstCommitted

	publicationLocked := !channelStatusLock.TryLock()
	if !publicationLocked {
		channelStatusLock.Unlock()
	}
	require.True(t, publicationLocked, "the publication lock must span commit through cache deletion")
	_, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)

	barrier.Release()
	require.NoError(t, <-deleteResult)
	assertChannelDeleted(t, db, channel.Id)
}

func runDeletePlanQuotaRace(
	t *testing.T,
	first func() error,
	second func() error,
) {
	t.Helper()

	authorityLocked, releaseAuthority := installFirstAuthorityLockBarrier(t, DB)
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseAuthority)
		})
	}
	defer release()
	secondAttempted := make(chan struct{})
	var attempts atomic.Int32
	previousObserver := channelStatusPublicationObserver
	channelStatusPublicationObserver = func(phase channelStatusPublicationPhase) {
		if phase == channelStatusPublicationBeforeWrite && attempts.Add(1) == 2 {
			close(secondAttempted)
		}
	}
	t.Cleanup(func() {
		channelStatusPublicationObserver = previousObserver
	})

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- first()
	}()
	select {
	case <-authorityLocked:
	case err := <-firstResult:
		t.Fatalf("first operation returned before locking Plan authority: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first operation did not lock Plan authority")
	}

	secondResult := make(chan error, 1)
	go func() {
		secondResult <- second()
	}()
	select {
	case <-secondAttempted:
	case <-time.After(5 * time.Second):
		t.Fatal("second operation did not enter the serialized write protocol")
	}
	release()

	require.NoError(t, <-firstResult)
	require.NoError(t, <-secondResult)
}

func TestChannelDeleteSerializesPlanQuotaDisableBothOrders(t *testing.T) {
	for _, firstOperation := range []string{"delete", "disable"} {
		t.Run(firstOperation+"_first", func(t *testing.T) {
			db := setupPlanQuotaAuthorityTest(t, "")
			channel := createPlanQuotaDomainFixture(
				t,
				db,
				"authority-secret-delete-disable",
				"plan:test:delete-disable",
				PlanQuotaDomainStateActive,
				0,
				0,
			)
			disableChannelDeleteCache(t)

			deleteChannel := func() error {
				return (&Channel{Id: channel.Id}).Delete()
			}
			disableDomain := func() error {
				_, err := DisablePlanQuotaDomain(PlanQuotaDomainDisableRequest{
					FailingChannelID:   channel.Id,
					ObservedCredential: channel.Key,
					ObservedTag:        channel.GetTag(),
					Reason:             "quota exhausted",
					ResetAt:            2_000_000_000,
				})
				return err
			}
			if firstOperation == "delete" {
				runDeletePlanQuotaRace(t, deleteChannel, disableDomain)
			} else {
				runDeletePlanQuotaRace(t, disableDomain, deleteChannel)
			}

			assertChannelDeleted(t, db, channel.Id)
			hash, ok := PlanQuotaDomainHash(channel.Key)
			require.True(t, ok)
			var authority PlanQuotaDomain
			require.NoError(t, db.First(&authority, "credential_hash = ?", hash).Error)
			assert.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
		})
	}
}

func TestChannelDeleteSerializesPlanQuotaRecoveryBothOrders(t *testing.T) {
	for _, firstOperation := range []string{"delete", "recovery"} {
		t.Run(firstOperation+"_first", func(t *testing.T) {
			db := setupPlanQuotaAuthorityTest(t, "")
			channel := createPlanQuotaDomainFixture(
				t,
				db,
				"authority-secret-delete-recovery",
				"plan:test:delete-recovery",
				PlanQuotaDomainStateDisabled,
				41,
				1,
			)
			disableChannelDeleteCache(t)
			snapshot, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)

			deleteChannel := func() error {
				return (&Channel{Id: channel.Id}).Delete()
			}
			recoverDomain := func() error {
				_, err := RecoverPlanQuotaDomain(PlanQuotaDomainRecoveryRequest{
					Source:     snapshot,
					RecoveryAt: 2,
					RequireDue: true,
				})
				return err
			}
			if firstOperation == "delete" {
				runDeletePlanQuotaRace(t, deleteChannel, recoverDomain)
			} else {
				runDeletePlanQuotaRace(t, recoverDomain, deleteChannel)
			}

			assertChannelDeleted(t, db, channel.Id)
			hash, ok := PlanQuotaDomainHash(channel.Key)
			require.True(t, ok)
			var authority PlanQuotaDomain
			require.NoError(t, db.First(&authority, "credential_hash = ?", hash).Error)
			if firstOperation == "recovery" {
				assert.Equal(t, PlanQuotaDomainStateActive, authority.State)
			} else {
				assert.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
				assert.Equal(t, int64(41), authority.Generation)
			}
		})
	}
}

func TestChannelDeleteRollsBackOnAbilityFailure(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	channel := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-delete-ability-failure",
		"plan:test:delete-ability-failure",
		PlanQuotaDomainStateActive,
		0,
		0,
	)
	enableChannelDeleteCache(t)

	forcedErr := errors.New("forced delete ability failure")
	callbackName := "test:channel_delete_ability_failure"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil &&
			tx.Statement.Schema != nil &&
			tx.Statement.Schema.Name == "Ability" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Delete().Remove(callbackName))
	})

	require.ErrorIs(t, (&Channel{Id: channel.Id}).Delete(), forcedErr)
	assertChannelDeleteRolledBack(t, db, channel.Id)
}

func TestBatchDeleteChannelsRollsBackOnChannelFailure(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	channel := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-delete-channel-failure",
		"plan:test:delete-channel-failure",
		PlanQuotaDomainStateActive,
		0,
		0,
	)
	enableChannelDeleteCache(t)

	forcedErr := errors.New("forced delete channel failure")
	callbackName := "test:channel_delete_channel_failure"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil &&
			tx.Statement.Schema != nil &&
			tx.Statement.Schema.Name == "Channel" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Delete().Remove(callbackName))
	})

	deleted, err := BatchDeleteChannels([]int{channel.Id})
	require.ErrorIs(t, err, forcedErr)
	assert.Zero(t, deleted)
	assertChannelDeleteRolledBack(t, db, channel.Id)
}

func TestBatchDeleteChannelsDoesNotRetryUnknownCommitFailure(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	var databaseSequence int
	var databaseName string
	var databasePath string
	require.NoError(t, db.Raw("PRAGMA database_list").Row().Scan(
		&databaseSequence,
		&databaseName,
		&databasePath,
	))
	require.NoError(t, db.Exec(
		"CREATE TABLE channel_delete_commit_parents (id INTEGER PRIMARY KEY)",
	).Error)
	require.NoError(t, db.Exec(
		"CREATE TABLE channel_delete_commit_children ("+
			"id INTEGER PRIMARY KEY, parent_id INTEGER, "+
			"FOREIGN KEY(parent_id) REFERENCES channel_delete_commit_parents(id) "+
			"DEFERRABLE INITIALLY DEFERRED)",
	).Error)
	channel := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-delete-commit-failure",
		"plan:test:delete-commit-failure",
		PlanQuotaDomainStateActive,
		0,
		0,
	)
	enableChannelDeleteCache(t)

	var channelWrites atomic.Int32
	callbackName := "test:channel_delete_commit_failure"
	require.NoError(t, db.Callback().Delete().After("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement == nil ||
			tx.Statement.Schema == nil ||
			tx.Statement.Schema.Name != "Channel" {
			return
		}
		channelWrites.Add(1)
		_, err := tx.Statement.ConnPool.ExecContext(
			tx.Statement.Context,
			"INSERT INTO channel_delete_commit_children (id, parent_id) VALUES (?, ?)",
			1,
			999,
		)
		tx.AddError(err)
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Delete().Remove(callbackName))
	})

	deleted, err := BatchDeleteChannels([]int{channel.Id})
	require.Error(t, err)
	assert.Contains(t, strings.ToUpper(err.Error()), "FOREIGN KEY")
	assert.Zero(t, deleted)
	assert.Equal(t, int32(1), channelWrites.Load())

	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	reopened, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate",
		databasePath,
	)), &gorm.Config{})
	require.NoError(t, err)
	reopenedSQL, err := reopened.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, reopenedSQL.Close())
	})
	DB = reopened
	assertChannelDeleteRolledBack(t, reopened, channel.Id)
}

func TestDeleteDisabledChannelUsesFencedDeletionAndRemovesAbilities(t *testing.T) {
	db := setupPlanQuotaAuthorityTest(t, "")
	channel := createPlanQuotaDomainFixture(
		t,
		db,
		"authority-secret-delete-disabled",
		"plan:test:delete-disabled",
		PlanQuotaDomainStateDisabled,
		51,
		2_000_000_000,
	)
	enableChannelDeleteCache(t)

	deleted, err := DeleteDisabledChannel()
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	assertChannelDeleted(t, db, channel.Id)

	hash, ok := PlanQuotaDomainHash(channel.Key)
	require.True(t, ok)
	var authority PlanQuotaDomain
	require.NoError(t, db.First(&authority, "credential_hash = ?", hash).Error)
	assert.Equal(t, int64(51), authority.Generation)
	assert.Equal(t, PlanQuotaDomainStateDisabled, authority.State)
}

func TestChannelDeleteConfiguredDatabases(t *testing.T) {
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

			tablePrefix := fmt.Sprintf("channel_delete_%d_%d_", os.Getpid(), time.Now().UnixNano())
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
			))
			tag := "plan:test:configured-delete"
			channel := Channel{
				Name: "configured-delete", Key: "authority-secret-configured-delete",
				Tag: &tag, Status: common.ChannelStatusEnabled,
				Models: "gpt-4.1", Group: "default",
			}
			require.NoError(t, channel.Insert())
			require.NoError(t, channel.Delete())

			assertChannelDeleted(t, database, channel.Id)
			hash, ok := PlanQuotaDomainHash(channel.Key)
			require.True(t, ok)
			var authority PlanQuotaDomain
			require.NoError(t, database.First(&authority, "credential_hash = ?", hash).Error)
			assert.Equal(t, PlanQuotaDomainStateActive, authority.State)
		})
	}
}
