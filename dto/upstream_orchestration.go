package dto

type UpstreamSyncSnapshot struct {
	SchemaVersion int                      `json:"schema_version"`
	SnapshotID    string                   `json:"snapshot_id"`
	DeviceID      string                   `json:"device_id"`
	CapturedAt    int64                    `json:"captured_at"`
	Sources       []UpstreamSourceSnapshot `json:"sources"`
}

type UpstreamSourceSnapshot struct {
	Key                string                     `json:"key"`
	Name               string                     `json:"name"`
	ConsoleURL         string                     `json:"console_url"`
	AdapterVersion     string                     `json:"adapter_version"`
	Status             string                     `json:"status"`
	Error              string                     `json:"error,omitempty"`
	Balance            *float64                   `json:"balance,omitempty"`
	APIBaseURL         string                     `json:"api_base_url,omitempty"`
	EndpointCandidates []UpstreamEndpointSnapshot `json:"endpoint_candidates,omitempty"`
	Groups             []UpstreamGroupSnapshot    `json:"groups,omitempty"`
	Monitors           []UpstreamMonitorSnapshot  `json:"monitors,omitempty"`
	Keys               []UpstreamKeySnapshot      `json:"keys,omitempty"`
}

type UpstreamEndpointSnapshot struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Healthy   *bool  `json:"healthy,omitempty"`
}

type UpstreamGroupSnapshot struct {
	ExternalID         string                  `json:"external_id"`
	Name               string                  `json:"name"`
	Platform           string                  `json:"platform"`
	SubscriptionType   string                  `json:"subscription_type,omitempty"`
	RateMultiplier     float64                 `json:"rate_multiplier"`
	UserRateMultiplier *float64                `json:"user_rate_multiplier,omitempty"`
	PeakRateEnabled    bool                    `json:"peak_rate_enabled,omitempty"`
	PeakStart          string                  `json:"peak_start,omitempty"`
	PeakEnd            string                  `json:"peak_end,omitempty"`
	PeakMultiplier     *float64                `json:"peak_rate_multiplier,omitempty"`
	IsExclusive        bool                    `json:"is_exclusive,omitempty"`
	MonitorExternalID  string                  `json:"monitor_external_id,omitempty"`
	HealthStatus       string                  `json:"health_status,omitempty"`
	Availability       *float64                `json:"availability,omitempty"`
	LatencyMS          *int64                  `json:"latency_ms,omitempty"`
	EndpointPingMS     *int64                  `json:"endpoint_ping_ms,omitempty"`
	Models             []UpstreamModelSnapshot `json:"models,omitempty"`
}

type UpstreamModelSnapshot struct {
	Name     string         `json:"name"`
	Platform string         `json:"platform,omitempty"`
	Pricing  map[string]any `json:"pricing,omitempty"`
}

type UpstreamMonitorSnapshot struct {
	ExternalID     string   `json:"external_id,omitempty"`
	Name           string   `json:"name"`
	Provider       string   `json:"provider"`
	Model          string   `json:"model,omitempty"`
	Status         string   `json:"status"`
	Availability   *float64 `json:"availability,omitempty"`
	LatencyMS      *int64   `json:"latency_ms,omitempty"`
	EndpointPingMS *int64   `json:"endpoint_ping_ms,omitempty"`
	ObservedAt     int64    `json:"observed_at,omitempty"`
}

type UpstreamKeySnapshot struct {
	ExternalID      string   `json:"external_id"`
	Name            string   `json:"name"`
	GroupExternalID string   `json:"group_external_id,omitempty"`
	Status          string   `json:"status"`
	Fingerprint     string   `json:"fingerprint"`
	InputTokens     *int64   `json:"input_tokens,omitempty"`
	OutputTokens    *int64   `json:"output_tokens,omitempty"`
	CachedTokens    *int64   `json:"cached_tokens,omitempty"`
	TotalTokens     *int64   `json:"total_tokens,omitempty"`
	Usage5H         *float64 `json:"usage_5h,omitempty"`
	Limit5H         *float64 `json:"limit_5h,omitempty"`
	Reset5HAt       *int64   `json:"reset_5h_at,omitempty"`
	Usage1D         *float64 `json:"usage_1d,omitempty"`
	Limit1D         *float64 `json:"limit_1d,omitempty"`
	Reset1DAt       *int64   `json:"reset_1d_at,omitempty"`
	Usage7D         *float64 `json:"usage_7d,omitempty"`
	Limit7D         *float64 `json:"limit_7d,omitempty"`
	Reset7DAt       *int64   `json:"reset_7d_at,omitempty"`
	Usage30D        *float64 `json:"usage_30d,omitempty"`
	Limit30D        *float64 `json:"limit_30d,omitempty"`
	Reset30DAt      *int64   `json:"reset_30d_at,omitempty"`
	DataQuality     string   `json:"data_quality,omitempty"`
}

type UpstreamPairDeviceRequest struct {
	PairingCode string `json:"pairing_code"`
	DeviceName  string `json:"device_name"`
}

type UpstreamPairDeviceResponse struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

type UpstreamPairingCodeResponse struct {
	DeviceID    string `json:"device_id"`
	PairingCode string `json:"pairing_code"`
	ExpiresAt   int64  `json:"expires_at"`
}

type UpstreamEnrollmentResult struct {
	CommandID       string `json:"command_id"`
	Success         bool   `json:"success"`
	SourceKey       string `json:"source_key"`
	ExternalGroupID string `json:"external_group_id"`
	ExternalKeyID   string `json:"external_key_id,omitempty"`
	KeyFingerprint  string `json:"key_fingerprint,omitempty"`
	APIKey          string `json:"api_key,omitempty"`
	Error           string `json:"error,omitempty"`
}

type UpstreamEnrollmentCommand struct {
	SourceKey          string   `json:"source_key"`
	ExternalGroupID    string   `json:"external_group_id"`
	GroupName          string   `json:"group_name"`
	Platform           string   `json:"platform"`
	APIBaseURL         string   `json:"api_base_url"`
	Models             []string `json:"models"`
	KeyName            string   `json:"key_name"`
	IPWhitelist        []string `json:"ip_whitelist,omitempty"`
	ResponsesPath      string   `json:"responses_path,omitempty"`
	ResponsesConverter string   `json:"responses_converter,omitempty"`
	MessagesPath       string   `json:"messages_path,omitempty"`
	MessagesConverter  string   `json:"messages_converter,omitempty"`
}

type UpstreamRouteActionRequest struct {
	Reason string `json:"reason,omitempty"`
}

type UpstreamSourceUpdateRequest struct {
	Enabled             *bool             `json:"enabled,omitempty"`
	LowBalanceThreshold *float64          `json:"low_balance_threshold,omitempty"`
	StaticEgressIPs     []string          `json:"static_egress_ips,omitempty"`
	ModelAliases        map[string]string `json:"model_aliases,omitempty"`
}
