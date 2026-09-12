package model

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestCreateRealtimeTicketPersistsOnlyDigestAndBindings(t *testing.T) {
	truncateTables(t)
	before := time.Now().Unix()

	raw, ticket, err := CreateRealtimeTicket(RealtimeTicketCreate{
		UserId:          7,
		TokenId:         11,
		Model:           "gpt-realtime",
		RoutingStrategy: hosttypes.RoutingStrategyLatency,
	})
	require.NoError(t, err)
	require.NotNil(t, ticket)
	assert.True(t, strings.HasPrefix(raw, RealtimeTicketPrefix))
	assert.Equal(t, 7, ticket.UserId)
	assert.Equal(t, 11, ticket.TokenId)
	assert.Equal(t, "gpt-realtime", ticket.Model)
	assert.Equal(t, hosttypes.RoutingStrategyLatency, ticket.RoutingStrategy)
	assert.GreaterOrEqual(t, ticket.ExpiresAt, before+int64(RealtimeTicketTTL/time.Second))

	var flow AuthFlow
	require.NoError(t, DB.Where("purpose = ?", AuthFlowPurposeRealtimeTicket).First(&flow).Error)
	assert.NotEqual(t, raw, flow.TokenHash)
	assert.NotContains(t, flow.Payload, raw)
	assert.Equal(t, authFlowTokenHash(strings.TrimPrefix(raw, RealtimeTicketPrefix)), flow.TokenHash)
}

func TestConsumeRealtimeTicketIsSingleUse(t *testing.T) {
	truncateTables(t)
	raw, _, err := CreateRealtimeTicket(RealtimeTicketCreate{
		UserId:          7,
		TokenId:         11,
		Model:           "gpt-realtime",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	consumed, err := ConsumeRealtimeTicket(raw, "gpt-realtime")
	require.NoError(t, err)
	assert.Equal(t, 7, consumed.UserId)
	assert.Equal(t, 11, consumed.TokenId)
	assert.Equal(t, hosttypes.RoutingStrategyStable, consumed.RoutingStrategy)

	_, err = ConsumeRealtimeTicket(raw, "gpt-realtime")
	assert.ErrorIs(t, err, ErrAuthFlowConsumed)
}

func TestRealtimeTicketBindingMismatchDoesNotConsumeTicket(t *testing.T) {
	truncateTables(t)
	raw, _, err := CreateRealtimeTicket(RealtimeTicketCreate{
		UserId:          7,
		TokenId:         11,
		Model:           "gpt-realtime",
		RoutingStrategy: hosttypes.RoutingStrategyEconomy,
	})
	require.NoError(t, err)

	_, err = ConsumeRealtimeTicket(raw, "different-model")
	assert.ErrorIs(t, err, ErrRealtimeTicketBinding)

	consumed, err := ConsumeRealtimeTicket(raw, "gpt-realtime")
	require.NoError(t, err)
	assert.Equal(t, "gpt-realtime", consumed.Model)
}

func TestGetRealtimeTicketClaimsDefersModelBindingWithoutConsuming(t *testing.T) {
	truncateTables(t)
	raw, _, err := CreateRealtimeTicket(RealtimeTicketCreate{
		UserId:          7,
		TokenId:         11,
		Model:           "gemini-live-test",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	claims, err := GetRealtimeTicketClaims(raw)
	require.NoError(t, err)
	assert.Equal(t, "gemini-live-test", claims.Model)

	consumed, err := ConsumeRealtimeTicket(raw, "gemini-live-test")
	require.NoError(t, err)
	assert.Equal(t, claims.TokenId, consumed.TokenId)
}

func TestRealtimeTicketConcurrentConsumptionHasOneWinner(t *testing.T) {
	truncateTables(t)
	raw, _, err := CreateRealtimeTicket(RealtimeTicketCreate{
		UserId:          7,
		TokenId:         11,
		Model:           "gpt-realtime",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)

	var successes atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, consumeErr := ConsumeRealtimeTicket(raw, "gpt-realtime"); consumeErr == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, successes.Load())
}

func TestRealtimeTicketExpiresAfterSixtySeconds(t *testing.T) {
	truncateTables(t)
	raw, _, err := CreateRealtimeTicket(RealtimeTicketCreate{
		UserId:          7,
		TokenId:         11,
		Model:           "gpt-realtime",
		RoutingStrategy: hosttypes.RoutingStrategyStable,
	})
	require.NoError(t, err)
	require.NoError(t, DB.Model(&AuthFlow{}).
		Where("purpose = ?", AuthFlowPurposeRealtimeTicket).
		Update("expires_at", time.Now().Add(-time.Second)).Error)

	_, err = ConsumeRealtimeTicket(raw, "gpt-realtime")
	assert.ErrorIs(t, err, ErrAuthFlowExpired)
}

func TestRealtimeTicketConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		dbType    common.DatabaseType
		dialector func(string) gorm.Dialector
	}{
		{
			name:   "sqlite",
			dbType: common.DatabaseTypeSQLite,
			dialector: func(dsn string) gorm.Dialector {
				return sqlite.Open(dsn)
			},
		},
		{
			name:   "mysql",
			env:    "TEST_MYSQL_DSN",
			dbType: common.DatabaseTypeMySQL,
			dialector: func(dsn string) gorm.Dialector {
				return mysql.Open(dsn)
			},
		},
		{
			name:   "postgres",
			env:    "TEST_POSTGRES_DSN",
			dbType: common.DatabaseTypePostgreSQL,
			dialector: func(dsn string) gorm.Dialector {
				return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "realtime-ticket.db")
			if test.env != "" {
				dsn = strings.TrimSpace(os.Getenv(test.env))
				if dsn == "" {
					t.Skip(test.env + " is not configured")
				}
			}
			db, err := gorm.Open(test.dialector(dsn), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			if test.env != "" && db.Migrator().HasTable(&AuthFlow{}) {
				t.Skip("refusing to run realtime ticket test against an existing auth_flows table")
			}
			require.NoError(t, db.AutoMigrate(&AuthFlow{}))

			previousDB := DB
			previousType := common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(test.dbType)
			t.Cleanup(func() {
				_ = db.Migrator().DropTable(&AuthFlow{})
				DB = previousDB
				common.SetMainDatabaseType(previousType)
			})

			raw, _, err := CreateRealtimeTicket(RealtimeTicketCreate{
				UserId:          7,
				TokenId:         11,
				Model:           "gpt-realtime",
				RoutingStrategy: hosttypes.RoutingStrategyStable,
			})
			require.NoError(t, err)
			_, err = ConsumeRealtimeTicket(raw, "wrong-model")
			assert.ErrorIs(t, err, ErrRealtimeTicketBinding)

			var successes atomic.Int64
			var wg sync.WaitGroup
			for range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, consumeErr := ConsumeRealtimeTicket(raw, "gpt-realtime"); consumeErr == nil {
						successes.Add(1)
					}
				}()
			}
			wg.Wait()
			assert.EqualValues(t, 1, successes.Load())
		})
	}
}
