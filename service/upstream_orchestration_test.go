package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupUpstreamOrchestrationTest(t *testing.T) {
	t.Helper()
	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.UpstreamSource{},
		&model.UpstreamGroup{},
		&model.UpstreamManagedRoute{},
		&model.UpstreamMetricSnapshot{},
		&model.UpstreamSyncDevice{},
		&model.UpstreamSyncBatch{},
	))
	model.DB = db
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func TestNormalizeUpstreamEndpoints(t *testing.T) {
	t.Run("selects the fastest healthy allowlisted endpoint", func(t *testing.T) {
		slow := int64(50)
		fast := int64(10)
		healthy := true
		endpoints, selected, err := normalizeUpstreamEndpoints(
			model.UpstreamSourceKeyLeyi,
			"https://leyi12.xyz",
			[]dto.UpstreamEndpointSnapshot{
				{Name: "slow", URL: "https://api.leyiapi.com/", LatencyMS: &slow, Healthy: &healthy},
				{Name: "fast", URL: "https://leyiapi.com", LatencyMS: &fast, Healthy: &healthy},
			},
		)
		require.NoError(t, err)
		assert.Len(t, endpoints, 3)
		assert.Equal(t, "https://leyiapi.com", selected)
	})

	t.Run("rejects endpoints outside the source allowlist", func(t *testing.T) {
		_, _, err := normalizeUpstreamEndpoints(
			model.UpstreamSourceKeyLeyi,
			"",
			[]dto.UpstreamEndpointSnapshot{{URL: "https://example.com"}},
		)
		assert.Error(t, err)
	})
}

func TestIngestUpstreamSnapshot(t *testing.T) {
	setupUpstreamOrchestrationTest(t)
	now := common.GetTimestamp()
	device := &model.UpstreamSyncDevice{
		DeviceID: "device-one",
		Status:   model.UpstreamSyncDeviceActive,
	}

	t.Run("stores a valid source group and metric once", func(t *testing.T) {
		multiplier := 0.15
		availability := 99.5
		latency := int64(1500)
		snapshot := dto.UpstreamSyncSnapshot{
			SchemaVersion: upstreamSnapshotSchemaVersion,
			SnapshotID:    "snapshot-valid",
			DeviceID:      device.DeviceID,
			CapturedAt:    now,
			Sources: []dto.UpstreamSourceSnapshot{{
				Key:            model.UpstreamSourceKeyHualong,
				Name:           "Hualong",
				ConsoleURL:     "https://api.hualong.online",
				APIBaseURL:     "https://api-fast.hualong.online",
				Status:         model.UpstreamHealthOperational,
				Balance:        floatPointer(25),
				AdapterVersion: "sub2api-1",
				Groups: []dto.UpstreamGroupSnapshot{{
					ExternalID:         "group-1",
					Name:               "GPT Pro",
					Platform:           "openai",
					RateMultiplier:     0.2,
					UserRateMultiplier: &multiplier,
					HealthStatus:       model.UpstreamHealthOperational,
					Availability:       &availability,
					LatencyMS:          &latency,
					Models:             []dto.UpstreamModelSnapshot{{Name: "gpt-5.5"}},
				}},
			}},
		}

		result, err := IngestUpstreamSnapshot(device, snapshot)
		require.NoError(t, err)
		assert.Equal(t, 1, result.Sources)
		assert.Equal(t, 1, result.Groups)
		assert.Equal(t, 1, result.Metrics)

		result, err = IngestUpstreamSnapshot(device, snapshot)
		require.NoError(t, err)
		assert.Zero(t, result.Sources)
		var count int64
		require.NoError(t, model.DB.Model(&model.UpstreamMetricSnapshot{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})

	t.Run("failed partial snapshot preserves last valid operational values", func(t *testing.T) {
		source := model.UpstreamSource{
			Key:                 model.UpstreamSourceKeyEBond,
			Name:                "EBond",
			ConsoleURL:          "https://ebondai.com",
			EndpointCandidates:  `[{"name":"default","url":"https://api.ebondai.com"}]`,
			SelectedEndpoint:    "https://api.ebondai.com",
			Status:              model.UpstreamHealthOperational,
			Enabled:             false,
			Balance:             floatPointer(12),
			LowBalanceThreshold: 7,
			LastSnapshotID:      "old",
			LastSnapshotAt:      now - 60,
			LastSuccessAt:       now - 60,
		}
		require.NoError(t, model.DB.Create(&source).Error)

		_, err := IngestUpstreamSnapshot(device, dto.UpstreamSyncSnapshot{
			SchemaVersion: upstreamSnapshotSchemaVersion,
			SnapshotID:    "snapshot-error",
			DeviceID:      device.DeviceID,
			CapturedAt:    now,
			Sources: []dto.UpstreamSourceSnapshot{{
				Key:        model.UpstreamSourceKeyEBond,
				Name:       "EBond",
				ConsoleURL: "https://ebondai.com",
				Status:     model.UpstreamHealthError,
				Error:      "login required",
			}},
		})
		require.NoError(t, err)

		stored, err := model.GetUpstreamSourceByKey(model.UpstreamSourceKeyEBond)
		require.NoError(t, err)
		assert.False(t, stored.Enabled)
		assert.Equal(t, "https://api.ebondai.com", stored.SelectedEndpoint)
		require.NotNil(t, stored.Balance)
		assert.Equal(t, 12.0, *stored.Balance)
		assert.Equal(t, now-60, stored.LastSuccessAt)
		assert.Equal(t, "login required", stored.LastError)
		assert.Equal(t, model.UpstreamHealthError, stored.Status)
	})
}

func floatPointer(value float64) *float64 {
	return &value
}
