package operation_setting

import (
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/setting/config"
)

type StreamRecoverySetting struct {
	Enabled              bool     `json:"enabled"`
	AllowedModels        []string `json:"allowed_models"`
	TTLSeconds           int      `json:"ttl_seconds"`
	HeartbeatSeconds     int      `json:"heartbeat_seconds"`
	LeaseSeconds         int      `json:"lease_seconds"`
	MaxAttempts          int      `json:"max_attempts"`
	MaxEventsPerAttempt  int64    `json:"max_events_per_attempt"`
	MaxBytesPerExecution int64    `json:"max_bytes_per_execution"`
}

var streamRecoverySetting = StreamRecoverySetting{
	Enabled:              false,
	AllowedModels:        []string{"glm-5.3"},
	TTLSeconds:           24 * 60 * 60,
	HeartbeatSeconds:     10,
	LeaseSeconds:         30,
	MaxAttempts:          2,
	MaxEventsPerAttempt:  100_000,
	MaxBytesPerExecution: 64 * 1024 * 1024,
}

func init() {
	config.GlobalConfig.Register("stream_recovery", &streamRecoverySetting)
}

func GetStreamRecoverySetting() *StreamRecoverySetting {
	normalizeStreamRecoverySetting(&streamRecoverySetting)
	return &streamRecoverySetting
}

func IsStreamRecoveryModelAllowed(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	return slices.Contains(GetStreamRecoverySetting().AllowedModels, model)
}

func normalizeStreamRecoverySetting(setting *StreamRecoverySetting) {
	if setting.TTLSeconds < 60 {
		setting.TTLSeconds = 24 * 60 * 60
	}
	if setting.HeartbeatSeconds < 1 {
		setting.HeartbeatSeconds = 10
	}
	if setting.LeaseSeconds <= setting.HeartbeatSeconds {
		setting.LeaseSeconds = setting.HeartbeatSeconds * 3
	}
	if setting.MaxAttempts < 1 || setting.MaxAttempts > 5 {
		setting.MaxAttempts = 2
	}
	if setting.MaxEventsPerAttempt < 100 {
		setting.MaxEventsPerAttempt = 100_000
	}
	if setting.MaxBytesPerExecution < 1024*1024 {
		setting.MaxBytesPerExecution = 64 * 1024 * 1024
	}
	for index, model := range setting.AllowedModels {
		setting.AllowedModels[index] = strings.TrimSpace(model)
	}
	setting.AllowedModels = slices.DeleteFunc(
		slices.Compact(setting.AllowedModels),
		func(model string) bool { return model == "" },
	)
	if len(setting.AllowedModels) == 0 {
		setting.AllowedModels = []string{"glm-5.3"}
	}
}
