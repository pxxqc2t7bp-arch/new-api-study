package model

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// These snapshots intentionally contain no provider key or request body.
const (
	AppExecutionModelKindTaskBacked     = "task_backed"
	AppExecutionModelKindNativeResponse = "native_response"
)

type AppExecutionModel struct {
	PublicModel      string        `json:"public_model"`
	ActualModel      string        `json:"actual_model"`
	ExecutionKind    string        `json:"execution_kind" app_legacy_optional:"true"`
	PluginKey        string        `json:"plugin_key"`
	PluginVersion    string        `json:"plugin_version"`
	PluginSHA256     string        `json:"plugin_sha256"`
	Protocol         string        `json:"protocol"`
	ChannelID        int           `json:"channel_id"`
	ChannelType      int           `json:"channel_type" app_legacy_optional:"true"`
	APIType          int           `json:"api_type" app_legacy_optional:"true"`
	BaseURL          string        `json:"base_url" app_legacy_optional:"true"`
	ConnectionDigest string        `json:"connection_digest" app_legacy_optional:"true"`
	CredentialDigest string        `json:"credential_digest" app_legacy_optional:"true"`
	CredentialIndex  int           `json:"credential_index" app_legacy_optional:"true"`
	RequestProfile   string        `json:"request_profile" app_legacy_optional:"true"`
	Group            string        `json:"group"`
	GroupRatio       float64       `json:"group_ratio"`
	Price            PricingValues `json:"price"`
	BillingBasis     string        `json:"billing_basis"`
	ExprVersion      int           `json:"expr_version"`
	UsageSchemaJSON  string        `json:"usage_schema_json"`
	QuotaPerUnit     float64       `json:"quota_per_unit"`
}

func (candidate *AppExecutionModel) UnmarshalJSON(raw []byte) error {
	type wireModel AppExecutionModel
	var decoded wireModel
	var fields map[string]json.RawMessage
	if common.Unmarshal(raw, &decoded) != nil || common.Unmarshal(raw, &fields) != nil {
		return errors.New("invalid_execution_model")
	}
	_, hasKind := fields["execution_kind"]
	_, hasChannelType := fields["channel_type"]
	_, hasAPIType := fields["api_type"]
	_, hasBaseURL := fields["base_url"]
	_, hasConnection := fields["connection_digest"]
	_, hasCredential := fields["credential_digest"]
	_, hasCredentialIndex := fields["credential_index"]
	_, hasProfile := fields["request_profile"]
	if !hasKind && !hasChannelType && !hasAPIType && !hasBaseURL && !hasConnection &&
		!hasCredential && !hasCredentialIndex && !hasProfile &&
		decoded.PluginKey == "doubao" && decoded.PluginVersion == "1.2.0" &&
		decoded.PluginSHA256 != "" && strings.HasPrefix(decoded.ActualModel, "doubao-seedance-") &&
		(decoded.Protocol == "openai_video" || decoded.Protocol == "openai_responses") {
		decoded.ExecutionKind = AppExecutionModelKindTaskBacked
	}
	*candidate = AppExecutionModel(decoded)
	return nil
}

func (candidate AppExecutionModel) IsTaskBacked() bool {
	return candidate.ExecutionKind == AppExecutionModelKindTaskBacked
}

type AppRegisteredAssetInput struct {
	AuthorizationSHA256 string    `json:"authorization_sha256"`
	AssetURI            string    `json:"asset_uri"`
	JSONPointer         string    `json:"json_pointer"`
	MediaType           string    `json:"media_type"`
	PublicModel         string    `json:"public_model"`
	Protocol            string    `json:"protocol"`
	OwnerSubject        string    `json:"owner_subject"`
	ProjectID           string    `json:"project_id"`
	Purpose             string    `json:"purpose"`
	RoutingAccount      string    `json:"routing_account"`
	Status              string    `json:"status"`
	ExpiresAt           time.Time `json:"expires_at"`
}

type AppExecutionGrantBinding struct {
	GrantID                  string                    `json:"grant_id"`
	ScopeHash                string                    `json:"scope_hash"`
	RequestHash              string                    `json:"request_hash"`
	Issuer                   string                    `json:"issuer"`
	AppKey                   string                    `json:"app_key"`
	InstallationID           string                    `json:"installation_id"`
	Generation               string                    `json:"generation"`
	AppSessionID             string                    `json:"app_session_id"`
	Subject                  string                    `json:"subject"`
	UserID                   int                       `json:"user_id"`
	IssuingCredentialID      string                    `json:"issuing_credential_id"`
	IssuingCredentialVersion string                    `json:"issuing_credential_version"`
	RunID                    string                    `json:"run_id"`
	ExecutionRequestID       string                    `json:"execution_request_id"`
	Operation                string                    `json:"operation"`
	RegisteredAssetInputs    []AppRegisteredAssetInput `json:"registered_asset_inputs"`
	AssetSnapshotSHA256      string                    `json:"asset_snapshot_sha256"`
	SnapshotSHA256           string                    `json:"snapshot_sha256"`
	ExpiresAt                int64                     `json:"expires_at"`
}

type AppExecutionGrant struct {
	GrantID             string `gorm:"primaryKey;size:64" json:"-"`
	ScopeHash           string `gorm:"size:64;not null;uniqueIndex" json:"-"`
	RequestHash         string `gorm:"size:64;not null" json:"-"`
	AppKey              string `gorm:"size:128;not null" json:"-"`
	InstallationID      string `gorm:"size:64;not null;index" json:"-"`
	AppSessionID        string `gorm:"size:64;not null" json:"-"`
	Subject             string `gorm:"size:64;not null" json:"-"`
	UserID              int    `gorm:"not null;index" json:"-"`
	RunID               string `gorm:"size:64;not null" json:"-"`
	ExecutionRequestID  string `gorm:"size:64;not null" json:"-"`
	Operation           string `gorm:"size:32;not null" json:"-"`
	PolicyVersion       int64  `gorm:"not null" json:"-"`
	PolicyDigest        string `gorm:"size:64;not null" json:"-"`
	PolicyJSON          string `gorm:"type:text;not null" json:"-"`
	ModelsJSON          string `gorm:"type:text;not null" json:"-"`
	PassthroughJSON     string `gorm:"type:text;not null" json:"-"`
	PriceVersion        string `gorm:"size:64;not null" json:"-"`
	FundingSource       string `gorm:"size:32;not null" json:"-"`
	PayerRef            string `gorm:"size:128;not null" json:"-"`
	FundingRef          string `gorm:"size:128;not null" json:"-"`
	FundingSnapshotJSON string `gorm:"type:text;not null" json:"-"`
	BindingJSON         string `gorm:"type:text;not null" json:"-"`
	ResponseJSON        string `gorm:"type:text;not null" json:"-"`
	TokenHash           string `gorm:"size:64;not null;uniqueIndex" json:"-"`
	TokenSalt           string `gorm:"size:64;not null" json:"-"`
	CreatedAt           int64  `gorm:"not null" json:"-"`
	ExpiresAt           int64  `gorm:"not null" json:"-"`
}

type AppResponseResult struct {
	ExecutionID         int64            `gorm:"primaryKey;autoIncrement:false"`
	Execution           AppTaskExecution `gorm:"foreignKey:ExecutionID;references:ID;constraint:OnUpdate:RESTRICT,OnDelete:RESTRICT" json:"-"`
	HTTPStatus          int              `gorm:"not null"`
	SafeHeadersJSON     string           `gorm:"type:text;not null"`
	Body                []byte           `gorm:"not null"`
	BodySHA256          string           `gorm:"size:64;not null"`
	ResponseID          string           `gorm:"size:255;not null"`
	ProviderModel       string           `gorm:"size:128;not null"`
	ProviderStatus      string           `gorm:"size:32;not null"`
	RawUsageJSON        string           `gorm:"type:text;not null"`
	NormalizedUsageJSON string           `gorm:"type:text;not null"`
	CapturedAt          int64            `gorm:"not null"`
}

func MigrateAppExecutionTables(db *gorm.DB) error {
	return db.AutoMigrate(&AppExecutionPolicyVersion{}, &AppPassthroughRuleVersion{},
		&AppExecutionGrant{}, &AppTaskExecution{}, &AppResponseResult{},
		&AppTaskReconcile{}, &AppTaskSettlement{}, &AppTaskOutbox{},
		&AppTaskCancelReplay{})
}
