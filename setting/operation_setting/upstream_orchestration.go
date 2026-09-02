package operation_setting

import (
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/setting/config"
)

type UpstreamOrchestrationSetting struct {
	Enabled                   bool                `json:"enabled"`
	AutoEnroll                bool                `json:"auto_enroll"`
	TargetGroups              []string            `json:"target_groups"`
	CandidateLimit            int                 `json:"candidate_limit"`
	RequestAttemptLimit       int                 `json:"request_attempt_limit"`
	FailoverBudgetSeconds     int                 `json:"failover_budget_seconds"`
	ProbeFreshnessMinutes     int                 `json:"probe_freshness_minutes"`
	ProbeTimeoutSeconds       int                 `json:"probe_timeout_seconds"`
	ProbeConcurrencyPerSource int                 `json:"probe_concurrency_per_source"`
	FailureThreshold          int                 `json:"failure_threshold"`
	FailureWindowMinutes      int                 `json:"failure_window_minutes"`
	RedLongTermHours          int                 `json:"red_long_term_hours"`
	SyncIntervalHours         int                 `json:"sync_interval_hours"`
	DailyReconcileTime        string              `json:"daily_reconcile_time"`
	Timezone                  string              `json:"timezone"`
	MaxUpstreamMultiplier     float64             `json:"max_upstream_multiplier"`
	DetailRetentionDays       int                 `json:"detail_retention_days"`
	ManualPauseHours          int                 `json:"manual_pause_hours"`
	ShadowSuccessesRequired   int                 `json:"shadow_successes_required"`
	StaticEgressIPs           map[string][]string `json:"static_egress_ips"`
	ModelAliases              map[string]string   `json:"model_aliases"`
}

var upstreamOrchestrationSetting = UpstreamOrchestrationSetting{
	Enabled:                   false,
	AutoEnroll:                true,
	TargetGroups:              []string{"default", "cxy"},
	CandidateLimit:            5,
	RequestAttemptLimit:       5,
	FailoverBudgetSeconds:     90,
	ProbeFreshnessMinutes:     15,
	ProbeTimeoutSeconds:       30,
	ProbeConcurrencyPerSource: 1,
	FailureThreshold:          2,
	FailureWindowMinutes:      5,
	RedLongTermHours:          24,
	SyncIntervalHours:         4,
	DailyReconcileTime:        "03:00",
	Timezone:                  "Asia/Shanghai",
	MaxUpstreamMultiplier:     1,
	DetailRetentionDays:       90,
	ManualPauseHours:          24,
	ShadowSuccessesRequired:   3,
	StaticEgressIPs:           map[string][]string{},
	ModelAliases:              map[string]string{},
}

func init() {
	config.GlobalConfig.Register("upstream_orchestration", &upstreamOrchestrationSetting)
}

func GetUpstreamOrchestrationSetting() *UpstreamOrchestrationSetting {
	normalizeUpstreamOrchestrationSetting(&upstreamOrchestrationSetting)
	return &upstreamOrchestrationSetting
}

func normalizeUpstreamOrchestrationSetting(setting *UpstreamOrchestrationSetting) {
	if setting.CandidateLimit < 1 || setting.CandidateLimit > 5 {
		setting.CandidateLimit = 5
	}
	if setting.RequestAttemptLimit < 1 || setting.RequestAttemptLimit > 5 {
		setting.RequestAttemptLimit = 5
	}
	if setting.FailoverBudgetSeconds <= 0 {
		setting.FailoverBudgetSeconds = 90
	}
	if setting.ProbeFreshnessMinutes <= 0 {
		setting.ProbeFreshnessMinutes = 15
	}
	if setting.ProbeTimeoutSeconds <= 0 {
		setting.ProbeTimeoutSeconds = 30
	}
	if setting.ProbeConcurrencyPerSource < 1 {
		setting.ProbeConcurrencyPerSource = 1
	}
	if setting.FailureThreshold < 1 {
		setting.FailureThreshold = 2
	}
	if setting.FailureWindowMinutes <= 0 {
		setting.FailureWindowMinutes = 5
	}
	if setting.RedLongTermHours <= 0 {
		setting.RedLongTermHours = 24
	}
	if setting.SyncIntervalHours <= 0 {
		setting.SyncIntervalHours = 4
	}
	if strings.TrimSpace(setting.DailyReconcileTime) == "" {
		setting.DailyReconcileTime = "03:00"
	}
	if strings.TrimSpace(setting.Timezone) == "" {
		setting.Timezone = "Asia/Shanghai"
	}
	if setting.MaxUpstreamMultiplier <= 0 {
		setting.MaxUpstreamMultiplier = 1
	}
	if setting.DetailRetentionDays <= 0 {
		setting.DetailRetentionDays = 90
	}
	if setting.ManualPauseHours <= 0 {
		setting.ManualPauseHours = 24
	}
	if setting.ShadowSuccessesRequired < 1 {
		setting.ShadowSuccessesRequired = 3
	}
	if len(setting.TargetGroups) == 0 {
		setting.TargetGroups = []string{"default", "cxy"}
	}
	setting.TargetGroups = slices.Compact(setting.TargetGroups)
	if setting.StaticEgressIPs == nil {
		setting.StaticEgressIPs = map[string][]string{}
	}
	if setting.ModelAliases == nil {
		setting.ModelAliases = map[string]string{}
	}
}
