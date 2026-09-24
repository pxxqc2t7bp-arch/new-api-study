package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

var (
	planQuotaResetPattern = regexp.MustCompile(`(?i)reset at\s+(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4} [A-Z]+)`)
	planQuotaTokensLeft   = regexp.MustCompile(`(?i)\byou have\s+[0-9][0-9,]*(?:\.[0-9]+)?\s+weighted tokens? left\b`)
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	disableChannel(channelError, reason, nil)
}

type planQuotaDisableContext struct {
	resetAt     int64
	observedTag string
}

// DisableChannelForAPIError applies the structured Plan quota path when the
// upstream response has all required quota evidence.
func DisableChannelForAPIError(channelError types.ChannelError, observedTag string, err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	resetAt, planQuota := ClassifyPlanQuotaError(err)
	if !planQuota {
		return false
	}
	disableChannel(channelError, err.ErrorWithStatusCode(), &planQuotaDisableContext{
		resetAt:     resetAt,
		observedTag: observedTag,
	})
	return true
}

func disableChannel(channelError types.ChannelError, reason string, planQuota *planQuotaDisableContext) {
	common.SysLog(fmt.Sprintf("通道「%s」（#%d）发生错误，准备禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(fmt.Sprintf("通道「%s」（#%d）未启用自动禁用功能，跳过禁用操作", channelError.ChannelName, channelError.ChannelId))
		return
	}

	structuredPlanQuota := planQuota != nil && strings.HasPrefix(planQuota.observedTag, "plan:")
	if structuredPlanQuota && !channelError.IsMultiKey {
		disablePlanQuotaDomainWithCredential(
			&model.Channel{Id: channelError.ChannelId},
			channelError.UsingKey,
			planQuota.observedTag,
			reason,
			planQuota.resetAt,
		)
		return
	}

	var success bool
	if structuredPlanQuota {
		channel, err := model.GetChannelById(channelError.ChannelId, true)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to load multi-key Plan quota channel: channel_id=%d error=%v", channelError.ChannelId, err))
			return
		}
		success, err = model.UpdateMultiKeyChannelStatusIfUnchanged(
			channel,
			planQuota.observedTag,
			channelError.UsingKey,
			common.ChannelStatusAutoDisabled,
			reason,
			model.MultiKeyChannelStatusUpdateOptions{
				PlanQuotaResetAt:       planQuota.resetAt,
				ClearPlanQuotaDeadline: planQuota.resetAt == 0,
			},
		)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to disable multi-key Plan quota channel: channel_id=%d error=%v", channelError.ChannelId, err))
			return
		}
	} else {
		if handled, _, err := RecordManagedChannelFailure(channelError, reason); handled {
			if err != nil {
				common.SysError(fmt.Sprintf("failed to record managed channel failure: channel_id=%d error=%v", channelError.ChannelId, err))
			}
			return
		}
		success = model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
	}

	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelError.ChannelName, channelError.ChannelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, reason)
		NotifyRootUser(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), subject, content)
	}
}

func ParsePlanQuotaReset(reason string) (int64, bool) {
	lowerReason := strings.ToLower(reason)
	if !strings.Contains(lowerReason, "exceeded the 5-hour usage quota") &&
		!strings.Contains(lowerReason, "exceeded the weekly usage quota") &&
		!strings.Contains(lowerReason, "exceeded the monthly usage quota") {
		return 0, false
	}
	match := planQuotaResetPattern.FindStringSubmatch(reason)
	if len(match) != 2 {
		return 0, true
	}
	resetAt, err := time.Parse("2006-01-02 15:04:05 -0700 MST", match[1])
	if err != nil {
		return 0, true
	}
	return resetAt.Unix(), true
}

// ClassifyPlanQuotaError requires the upstream status, semantics, and message
// evidence that identify a Plan account quota response.
func ClassifyPlanQuotaError(err *types.NewAPIError) (int64, bool) {
	if err == nil || err.GetOriginalStatusCode() != 429 {
		return 0, false
	}
	message := err.Error()
	accountQuotaExceeded := isAccountQuotaExceededSemantic(err.GetErrorCode())
	switch openAIError := err.RelayError.(type) {
	case types.OpenAIError:
		accountQuotaExceeded = accountQuotaExceeded ||
			isAccountQuotaExceededSemantic(openAIError.Code) ||
			isAccountQuotaExceededSemantic(openAIError.Type)
		if openAIError.Message != "" {
			message = openAIError.Message
		}
	case *types.OpenAIError:
		if openAIError != nil {
			accountQuotaExceeded = accountQuotaExceeded ||
				isAccountQuotaExceededSemantic(openAIError.Code) ||
				isAccountQuotaExceededSemantic(openAIError.Type)
			if openAIError.Message != "" {
				message = openAIError.Message
			}
		}
	}
	if !accountQuotaExceeded {
		return 0, false
	}
	if resetAt, matched := ParsePlanQuotaReset(message); matched {
		return resetAt, true
	}
	return 0, planQuotaTokensLeft.MatchString(message)
}

func isAccountQuotaExceededSemantic(value any) bool {
	normalized := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
	normalized = strings.NewReplacer("_", "", "-", "").Replace(normalized)
	return normalized == "accountquotaexceeded"
}

func isNonMultiKeyPlanChannel(channel *model.Channel) bool {
	return channel != nil &&
		!channel.ChannelInfo.IsMultiKey &&
		strings.HasPrefix(channel.GetTag(), "plan:")
}

func planQuotaDomainID(channelID int, credential string) string {
	if credential == "" {
		return fmt.Sprintf("channel:%d", channelID)
	}
	hash, _ := model.PlanQuotaDomainHash(credential)
	return hash
}

// PlanQuotaRecoveryDomainKey returns the shared recovery owner for a marked or
// validated legacy Plan quota failure.
func PlanQuotaRecoveryDomainKey(channel *model.Channel) (string, bool) {
	if channel == nil || channel.Status != common.ChannelStatusAutoDisabled {
		return "", false
	}

	info := channel.GetOtherInfo()
	if value, exists := info["quota_domain_id"]; exists {
		domainID, ok := value.(string)
		if !ok || domainID == "" {
			return "", false
		}
		return "marker:" + domainID, true
	}

	quotaType, quotaTypeValid := info["quota_type"].(string)
	quotaDomain, quotaDomainValid := info["quota_domain"].(string)
	if !quotaTypeValid || quotaType != "plan" ||
		!quotaDomainValid || quotaDomain == "" ||
		quotaDomain != channel.GetTag() {
		return "", false
	}
	return "legacy:" + quotaDomain, true
}

func disablePlanQuotaDomain(failingChannel *model.Channel, reason string, resetAt int64) {
	observedCredential := ""
	observedTag := ""
	if failingChannel != nil {
		keys := failingChannel.GetKeys()
		if len(keys) == 1 {
			observedCredential = keys[0]
		}
		observedTag = failingChannel.GetTag()
	}
	disablePlanQuotaDomainWithCredential(failingChannel, observedCredential, observedTag, reason, resetAt)
}

func disablePlanQuotaDomainWithCredential(
	failingChannel *model.Channel,
	observedCredential string,
	observedTag string,
	reason string,
	resetAt int64,
) {
	if failingChannel == nil {
		common.SysError("failed to disable Plan quota domain: channel is nil")
		return
	}
	failingChannelID := failingChannel.Id
	if observedCredential == "" {
		disableEmptyCredentialPlanQuotaSource(
			failingChannelID,
			observedTag,
			reason,
			resetAt,
		)
		return
	}

	result, err := model.DisablePlanQuotaDomain(model.PlanQuotaDomainDisableRequest{
		FailingChannelID:   failingChannelID,
		ObservedCredential: observedCredential,
		ObservedTag:        observedTag,
		Reason:             reason,
		ResetAt:            resetAt,
	})
	if err != nil {
		common.SysError(fmt.Sprintf(
			"failed to disable Plan quota domain: channel_id=%d error=%v",
			failingChannelID,
			err,
		))
		return
	}
	if result.NewlyDisabled > 0 {
		subject := fmt.Sprintf("Plan 配额域「%s」已临时禁用", observedTag)
		content := fmt.Sprintf("%s，共禁用 %d 个协议渠道", reason, result.NewlyDisabled)
		NotifyRootUser("channel_plan_quota_"+observedTag, subject, content)
	}
}

func disableEmptyCredentialPlanQuotaSource(
	channelID int,
	observedTag string,
	reason string,
	resetAt int64,
) {
	current, err := model.GetChannelById(channelID, true)
	if err != nil {
		common.SysError(fmt.Sprintf(
			"failed to load empty-credential Plan quota source: channel_id=%d error=%v",
			channelID,
			err,
		))
		return
	}
	if current.ChannelInfo.IsMultiKey ||
		current.Key != "" ||
		current.GetTag() != observedTag {
		return
	}
	domainID := planQuotaDomainID(channelID, "")
	if current.Status != common.ChannelStatusEnabled {
		owner, owned := current.GetOtherInfo()["quota_domain_id"].(string)
		if current.Status != common.ChannelStatusAutoDisabled ||
			!owned ||
			owner != domainID {
			return
		}
	}
	desired := *current
	info := desired.GetOtherInfo()
	info["status_reason"] = reason
	info["status_time"] = common.GetTimestamp()
	info["quota_domain"] = current.GetTag()
	info["quota_domain_id"] = domainID
	generation := time.Now().UnixNano()
	if previous, ok := info["quota_generation"].(string); ok {
		if parsed, parseErr := strconv.ParseInt(previous, 10, 64); parseErr == nil &&
			generation <= parsed {
			generation = parsed + 1
		}
	}
	info["quota_generation"] = strconv.FormatInt(generation, 10)
	info["quota_type"] = "plan"
	if resetAt > 0 {
		info["quota_reset_at"] = resetAt
		info["disabled_until"] = resetAt + 60
	} else {
		delete(info, "quota_reset_at")
		delete(info, "disabled_until")
	}
	desired.SetOtherInfo(info)
	_, err = model.UpdateSingleKeyChannelStatusIfUnchanged(
		current.Id,
		current.Key,
		current.GetTag(),
		current.Status,
		current.OtherInfo,
		common.ChannelStatusAutoDisabled,
		desired.OtherInfo,
	)
	if err != nil {
		common.SysError(fmt.Sprintf(
			"failed to disable empty-credential Plan quota source: channel_id=%d error=%v",
			channelID,
			err,
		))
	}
}

func EnableChannel(channelId int, usingKey string, channelName string) {
	channel, _ := model.CacheGetChannel(channelId)
	if isNonMultiKeyPlanChannel(channel) && enablePlanQuotaDomain(channel) {
		return
	}
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

// NotifyManagedChannelRecovered sends the same recovery notification as a
// direct channel enable after orchestration has persisted the final state.
func NotifyManagedChannelRecovered(channel *model.Channel) {
	notifyManagedChannelRecovered(channel, NotifyRootUser)
}

func notifyManagedChannelRecovered(
	channel *model.Channel,
	notifyRootUser func(string, string, string),
) {
	if channel == nil || channel.Id <= 0 {
		return
	}
	subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
	content := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
	notifyRootUser(
		formatNotifyType(channel.Id, common.ChannelStatusEnabled),
		subject,
		content,
	)
}

// EnableChannelForHealthCheck recovers only the state observed before the
// probe. Manual and internal callers that intentionally act on current state
// should continue using EnableChannel.
func EnableChannelForHealthCheck(channel *model.Channel, usingKey string) int {
	if channel == nil {
		return 0
	}
	recoveryAt := time.Now().Unix()
	if channel.ChannelInfo.IsMultiKey {
		if channel.Status != common.ChannelStatusEnabled &&
			channel.Status != common.ChannelStatusAutoDisabled {
			return 0
		}
		changed, err := model.RecoverMultiKeyChannelStatusIfUnchanged(
			channel,
			channel.GetTag(),
			usingKey,
			common.ChannelStatusEnabled,
			"",
			model.MultiKeyChannelStatusUpdateOptions{
				ClearPlanQuotaDeadline: channel.Status == common.ChannelStatusAutoDisabled,
			},
			recoveryAt,
		)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to recover multi-key channel from health-check snapshot: channel_id=%d error=%v", channel.Id, err))
			return 0
		}
		if !changed {
			return 0
		}
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
		NotifyRootUser(formatNotifyType(channel.Id, common.ChannelStatusEnabled), subject, content)
		return 1
	}
	if channel.Status != common.ChannelStatusAutoDisabled ||
		channel.GetDisabledUntil() > recoveryAt {
		return 0
	}
	if isNonMultiKeyPlanChannel(channel) {
		if _, owned := PlanQuotaRecoveryDomainKey(channel); owned {
			return enablePlanQuotaDomainForHealthCheck(channel, recoveryAt)
		}
	}

	changed, err := enableSingleKeyChannelSnapshot(channel, false, &recoveryAt)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to recover channel from health-check snapshot: channel_id=%d error=%v", channel.Id, err))
		return 0
	}
	if changed {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
		NotifyRootUser(formatNotifyType(channel.Id, common.ChannelStatusEnabled), subject, content)
		return 1
	}
	return 0
}

func enablePlanQuotaDomainForHealthCheck(
	recoveringSnapshot *model.Channel,
	recoveryAt int64,
) int {
	if !planQuotaSnapshotMatchesCredentialMarker(recoveringSnapshot) {
		changed, err := enableSingleKeyChannelSnapshot(recoveringSnapshot, true, &recoveryAt)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to recover rotated Plan quota source: channel_id=%d error=%v", recoveringSnapshot.Id, err))
			return 0
		}
		if changed {
			return 1
		}
		return 0
	}

	result, err := model.RecoverPlanQuotaDomain(model.PlanQuotaDomainRecoveryRequest{
		Source:     recoveringSnapshot,
		RecoveryAt: recoveryAt,
		RequireDue: true,
	})
	if err != nil {
		common.SysError(fmt.Sprintf("failed to recover Plan quota domain: channel_id=%d error=%v", recoveringSnapshot.Id, err))
		return 0
	}
	enabled := result.NewlyEnabled
	if enabled == 0 {
		return 0
	}
	tag := recoveringSnapshot.GetTag()
	NotifyRootUser("channel_plan_quota_recovered_"+tag,
		fmt.Sprintf("Plan 配额域「%s」已恢复", tag),
		fmt.Sprintf("已恢复 %d 个协议渠道", enabled))
	return enabled
}

func planQuotaSnapshotMatchesCredentialMarker(channel *model.Channel) bool {
	domainID, marked := channel.GetOtherInfo()["quota_domain_id"].(string)
	if !marked {
		return true
	}
	hash, ok := model.PlanQuotaDomainHash(channel.Key)
	return ok && hash == domainID
}

func enableSingleKeyChannelSnapshot(
	channel *model.Channel,
	clearPlanQuota bool,
	recoveryAt *int64,
) (bool, error) {
	if channel == nil || channel.ChannelInfo.IsMultiKey ||
		channel.Status != common.ChannelStatusAutoDisabled {
		return false, nil
	}

	expectedOtherInfo := channel.OtherInfo
	desired := *channel
	metadata := desired.GetOtherInfo()
	metadata["status_reason"] = ""
	metadata["status_time"] = common.GetTimestamp()
	if clearPlanQuota {
		delete(metadata, "disabled_until")
		delete(metadata, "quota_reset_at")
		delete(metadata, "quota_domain")
		delete(metadata, "quota_domain_id")
		delete(metadata, "quota_generation")
		delete(metadata, "quota_type")
	}
	desired.SetOtherInfo(metadata)

	if recoveryAt != nil {
		return model.RecoverSingleKeyChannelStatusIfUnchanged(
			channel,
			common.ChannelStatusEnabled,
			desired.OtherInfo,
			*recoveryAt,
		)
	}
	return model.UpdateSingleKeyChannelStatusIfUnchanged(
		channel.Id,
		channel.Key,
		channel.GetTag(),
		channel.Status,
		expectedOtherInfo,
		common.ChannelStatusEnabled,
		desired.OtherInfo,
	)
}

func enablePlanQuotaDomain(recoveringChannel *model.Channel) bool {
	if recoveringChannel == nil {
		common.SysError("failed to enable Plan quota domain: channel is nil")
		return true
	}
	current, err := model.GetChannelById(recoveringChannel.Id, true)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to load Plan quota domain: channel_id=%d error=%v", recoveringChannel.Id, err))
		return true
	}

	tag := current.GetTag()
	_, owned := PlanQuotaRecoveryDomainKey(current)
	if !owned {
		return false
	}

	enabled := 0
	if !planQuotaSnapshotMatchesCredentialMarker(current) {
		changed, err := enableSingleKeyChannelSnapshot(current, true, nil)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to recover rotated Plan quota source: channel_id=%d error=%v", current.Id, err))
			return true
		}
		if changed {
			enabled = 1
		}
	} else {
		result, err := model.RecoverPlanQuotaDomain(model.PlanQuotaDomainRecoveryRequest{
			Source: current,
		})
		if err != nil {
			common.SysError(fmt.Sprintf("failed to recover Plan quota domain: channel_id=%d error=%v", current.Id, err))
			return true
		}
		enabled = result.NewlyEnabled
	}
	if enabled > 0 {
		NotifyRootUser("channel_plan_quota_recovered_"+tag,
			fmt.Sprintf("Plan 配额域「%s」已恢复", tag),
			fmt.Sprintf("已恢复 %d 个协议渠道", enabled))
	}
	return true
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if _, quotaLimited := ClassifyPlanQuotaError(err); quotaLimited {
		return true
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
