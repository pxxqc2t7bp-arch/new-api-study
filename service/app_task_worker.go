package service

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

type AppTaskPollingAdaptor interface {
	Init(*relaycommon.RelayInfo)
	TaskPluginIdentity() (key, version, sourceSHA256 string)
	FetchTaskWithContext(context.Context, string, string, *model.Task, string) (*http.Response, error)
	ParseTaskResultWithContext(context.Context, *model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error)
	ValidateAppTaskUsage(map[string]any) (map[string]any, error)
	AppTaskArtifactsWithContext(context.Context, *model.Task) ([]types.TaskArtifact, map[string]string, error)
}

// Assigned once in main before starting the scheduler. Service does not import
// relay adaptors, which depend on this package.
var AppTaskAdaptorFactory func(pluginKey string) AppTaskPollingAdaptor

var appDoubaoTaskID = regexp.MustCompile(`^cgt-[A-Za-z0-9_-]{1,251}$`)

func ValidAppProviderTaskID(id string) bool { return appDoubaoTaskID.MatchString(id) }

func RunAppTaskReconcileOnce(ctx context.Context, db *gorm.DB, now time.Time) (processed bool, resultErr error) {
	started := time.Now()
	owner, err := model.NewAppPluginOpaqueID()
	if err != nil {
		return false, err
	}
	execution, lease, won, err := model.ClaimAppTaskReconcile(ctx, db, owner, now, time.Minute)
	if err != nil || !won {
		return false, err
	}
	if execution.ExecutionKind != model.AppExecutionKindTask {
		return false, errors.New("invalid_execution_kind")
	}
	errorCode := "storage_error"
	defer func() {
		if resultErr == nil {
			return
		}
		// Persist only a fixed error code, even when the query context expired.
		// The lease epoch still fences this cleanup against a replacement worker.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err := model.RunAppPluginTransaction(db.WithContext(cleanup), func(tx *gorm.DB) error {
			return model.RecordAppTaskObservationTx(tx, lease, "", errorCode, now.Add(time.Since(started)))
		})
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if !execution.ProviderAccepted || execution.ProviderTaskID == "" {
		errorCode = "provider_identity_unresolved"
		return true, errors.New(errorCode)
	}
	var task model.Task
	if err := db.WithContext(ctx).Where("task_id = ? AND user_id = ? AND execution_mode = ?",
		execution.TaskID, execution.UserID, "app_managed").First(&task).Error; err != nil {
		return true, err
	}
	if task.TaskID != execution.TaskID || task.ChannelId != execution.ChannelID ||
		task.PrivateData.UpstreamTaskID != execution.ProviderTaskID || task.PrivateData.Key != "" || task.PrivateData.TokenId != 0 {
		errorCode = "invalid_task_projection"
		return true, errors.New(errorCode)
	}
	// A funding retry consumes the already frozen terminal evidence. It must
	// not query, reprice, or depend on the current plugin/service identity.
	if execution.ProviderEvidenceHash != "" {
		err := model.RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
			_, err := model.FinalizeAppTaskTx(tx, lease, model.AppTaskTerminal{
				ProviderState: execution.ProviderState, FinalQuota: execution.FinalQuota,
				UsageJSON: execution.UsageJSON, ResultJSON: execution.ResultJSON, ArtifactsJSON: execution.ArtifactsJSON,
				ArtifactURLs: task.PrivateData.AppArtifactURLs,
			}, now.Add(time.Since(started)))
			return err
		})
		return true, err
	}
	if execution.PluginKey != "doubao" || execution.PluginVersion != "1.2.0" ||
		!appDoubaoTaskID.MatchString(execution.ProviderTaskID) || AppTaskAdaptorFactory == nil {
		errorCode = "task_adaptor_unavailable"
		return true, errors.New(errorCode)
	}
	adaptor := AppTaskAdaptorFactory(execution.PluginKey)
	if adaptor == nil {
		errorCode = "task_adaptor_unavailable"
		return true, errors.New(errorCode)
	}
	key, version, digest := adaptor.TaskPluginIdentity()
	if key != execution.PluginKey || version != execution.PluginVersion || digest != execution.PluginSHA256 {
		errorCode = "plugin_version_mismatch"
		return true, errors.New(errorCode)
	}
	var snapshot model.AppExecutionModel
	if common.UnmarshalJsonStr(execution.PriceSnapshotJSON, &snapshot) != nil ||
		snapshot.PublicModel != execution.PublicModel || snapshot.ActualModel != execution.ActualModel ||
		snapshot.ChannelID != execution.ChannelID || snapshot.PluginKey != key ||
		snapshot.PluginVersion != version || snapshot.PluginSHA256 != digest || snapshot.Group != execution.ActualGroup {
		errorCode = "invalid_price_snapshot"
		return true, errors.New(errorCode)
	}
	var channel model.Channel
	if err := db.WithContext(ctx).Where("id = ?", execution.ChannelID).First(&channel).Error; err != nil {
		errorCode = "channel_unavailable"
		return true, errors.New(errorCode)
	}
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(channel.Type)
	}
	credential := channel.Key
	if execution.ConnectionDigest != "" || execution.CredentialDigest != "" {
		options := ConfiguredAppPluginAuthOptions()
		connection, err := appTaskConnectionDigest(options.DerivationKey, channel)
		if err != nil || connection != execution.ConnectionDigest {
			errorCode = "host_connection_changed"
			return true, errors.New(errorCode)
		}
		if channel.ChannelInfo.IsMultiKey {
			keys := channel.GetKeys()
			if execution.CredentialIndex < 0 || execution.CredentialIndex >= len(keys) {
				errorCode = "host_credential_changed"
				return true, errors.New(errorCode)
			}
			credential = keys[execution.CredentialIndex]
		}
		if len(options.DerivationKey) < 32 ||
			appTaskCredentialDigest(options.DerivationKey, credential) != execution.CredentialDigest {
			errorCode = "host_credential_changed"
			return true, errors.New(errorCode)
		}
	}
	info := &relaycommon.RelayInfo{UserId: execution.UserID, UsingGroup: execution.ActualGroup,
		OriginModelName: execution.PublicModel}
	info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: execution.ChannelID, ChannelBaseUrl: baseURL,
		UpstreamModelName: execution.ActualModel, ChannelSetting: channel.GetSetting()}
	info.ApiKey = credential
	adaptor.Init(info)
	queryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	response, err := adaptor.FetchTaskWithContext(queryCtx, baseURL, credential, &task, channel.GetSetting().Proxy)
	if err != nil || response == nil {
		errorCode = "provider_query_failed"
		return true, errors.New(errorCode)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		errorCode = "provider_query_failed"
		return true, errors.New(errorCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		errorCode = "provider_result_invalid"
		return true, errors.New(errorCode)
	}
	var raw struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
	}
	if common.Unmarshal(body, &raw) != nil || raw.ID != execution.ProviderTaskID ||
		(raw.Model != "" && raw.Model != execution.ActualModel) {
		errorCode = "provider_result_invalid"
		return true, errors.New(errorCode)
	}
	parsed, err := adaptor.ParseTaskResultWithContext(queryCtx, &task, response, body)
	if err != nil || parsed == nil {
		errorCode = "provider_result_invalid"
		return true, errors.New(errorCode)
	}
	state := ""
	switch raw.Status {
	case "pending", "queued":
		state = "queued"
	case "processing", "running":
		state = "running"
	case "succeeded":
		state = "succeeded"
	case "failed", "expired":
		state = "failed"
	case "cancelled":
		state = "cancelled"
	default:
		errorCode = "provider_result_invalid"
		return true, errors.New(errorCode)
	}
	if state == "queued" || state == "running" {
		return true, model.RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
			return model.RecordAppTaskObservationTx(tx, lease, state, "", now.Add(time.Since(started)))
		})
	}
	var inputs AppTaskBillingInputs
	if decodeAppExecutionJSON([]byte(execution.RequestFactsJSON), &inputs) != nil {
		errorCode = "invalid_request_snapshot"
		return true, errors.New(errorCode)
	}
	if inputs.Usage == nil {
		inputs.Usage = map[string]any{}
	}
	maps.Copy(inputs.Usage, parsed.UsageFacts)
	inputs.Usage, err = adaptor.ValidateAppTaskUsage(inputs.Usage)
	if err != nil {
		errorCode = "provider_usage_invalid"
		return true, errors.New(errorCode)
	}
	quote := AppTaskPriceQuote{}
	artifacts := []types.TaskArtifact{}
	urls := map[string]string{}
	var providerError any
	if state == "succeeded" {
		quote, err = ComputeAppTaskPrice(snapshot, inputs, execution.PricedAt)
		if err != nil || quote.Quota < 0 {
			errorCode = "pricing_unavailable"
			return true, errors.New(errorCode)
		}
		task.Status, task.Data = model.TaskStatusSuccess, body
		artifacts, urls, err = adaptor.AppTaskArtifactsWithContext(queryCtx, &task)
		if err != nil {
			errorCode = "artifact_unavailable"
			return true, errors.New(errorCode)
		}
		if artifacts == nil {
			artifacts = []types.TaskArtifact{}
		}
	} else {
		providerError = map[string]string{"code": "provider_task_" + state}
	}
	result := map[string]any{"error": providerError, "billing": quote}
	if quote.Clamp != nil {
		result["admin_info"] = map[string]any{"quota_saturation": quote.Clamp.AuditMap()}
	}
	usageJSON, err := common.Marshal(inputs.Usage)
	if err != nil {
		return true, err
	}
	artifactsJSON, err := common.Marshal(artifacts)
	if err != nil {
		return true, err
	}
	resultJSON, err := common.Marshal(result)
	if err != nil {
		return true, err
	}
	return true, model.RunAppPluginTransaction(db.WithContext(ctx), func(tx *gorm.DB) error {
		_, err := model.FinalizeAppTaskTx(tx, lease, model.AppTaskTerminal{ProviderState: state, FinalQuota: int64(quote.Quota),
			UsageJSON: string(usageJSON), ArtifactsJSON: string(artifactsJSON), ResultJSON: string(resultJSON), ArtifactURLs: urls,
		}, now.Add(time.Since(started)))
		return err
	})
}
