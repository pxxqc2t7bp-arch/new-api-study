package service

import (
	"crypto/sha256"
	"encoding/hex"
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

const planQuotaDisableMaxAttempts = 3

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
	if err == nil || err.StatusCode != 429 {
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
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
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
	currentFailingChannel, err := model.GetChannelById(failingChannel.Id, true)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to load failing Plan quota channel: channel_id=%d error=%v", failingChannel.Id, err))
		return
	}
	failingChannel = currentFailingChannel

	channels := []*model.Channel{failingChannel}
	if observedCredential != "" {
		allChannels, err := model.GetAllChannels(0, 0, true, false)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to load Plan quota credential domain: channel_id=%d error=%v", failingChannel.Id, err))
			return
		}
		channels = allChannels
	}
	domainID := planQuotaDomainID(failingChannel.Id, observedCredential)
	generation := strconv.FormatInt(time.Now().UnixNano(), 10)

	disabled := 0
	for _, channel := range channels {
		newlyDisabled, err := disablePlanQuotaChannel(
			channel,
			failingChannel.Id,
			observedCredential,
			observedTag,
			domainID,
			generation,
			reason,
			resetAt,
		)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to disable Plan quota channel: channel_id=%d error=%v", channel.Id, err))
			continue
		}
		if newlyDisabled {
			disabled++
		}
	}
	if disabled > 0 {
		subject := fmt.Sprintf("Plan 配额域「%s」已临时禁用", observedTag)
		content := fmt.Sprintf("%s，共禁用 %d 个协议渠道", reason, disabled)
		NotifyRootUser("channel_plan_quota_"+observedTag, subject, content)
	}
}

func disablePlanQuotaChannel(
	channel *model.Channel,
	failingChannelID int,
	observedCredential string,
	observedTag string,
	domainID string,
	generation string,
	reason string,
	resetAt int64,
) (bool, error) {
	if channel == nil {
		return false, nil
	}
	channelID := channel.Id
	expectedKey := channel.Key
	expectedTag := channel.GetTag()

	for attempt := range planQuotaDisableMaxAttempts {
		if channel.Id != channelID ||
			channel.Key != expectedKey ||
			channel.GetTag() != expectedTag ||
			channel.ChannelInfo.IsMultiKey {
			return false, nil
		}
		keys := channel.GetKeys()
		if observedCredential == "" {
			if channel.Id != failingChannelID ||
				channel.Key != observedCredential ||
				channel.GetTag() != observedTag {
				return false, nil
			}
		} else if !strings.HasPrefix(channel.GetTag(), "plan:") ||
			len(keys) != 1 ||
			keys[0] != observedCredential {
			return false, nil
		}
		if channel.Status != common.ChannelStatusEnabled {
			candidateDomainID, ok := channel.GetOtherInfo()["quota_domain_id"].(string)
			if channel.Status != common.ChannelStatusAutoDisabled ||
				!ok ||
				candidateDomainID != domainID {
				return false, nil
			}
		}

		expectedStatus := channel.Status
		expectedOtherInfo := channel.OtherInfo
		desiredChannel := *channel
		metadata := desiredChannel.GetOtherInfo()
		if expectedStatus != common.ChannelStatusAutoDisabled {
			metadata["status_reason"] = reason
			metadata["status_time"] = common.GetTimestamp()
		}
		metadata["quota_domain"] = channel.GetTag()
		metadata["quota_domain_id"] = domainID
		metadata["quota_generation"] = generation
		metadata["quota_type"] = "plan"
		if resetAt > 0 {
			metadata["quota_reset_at"] = resetAt
			metadata["disabled_until"] = resetAt + 60
		} else {
			delete(metadata, "quota_reset_at")
			delete(metadata, "disabled_until")
		}
		desiredChannel.SetOtherInfo(metadata)

		changed, err := model.UpdateSingleKeyChannelStatusIfUnchanged(
			channel.Id,
			channel.Key,
			channel.GetTag(),
			expectedStatus,
			expectedOtherInfo,
			common.ChannelStatusAutoDisabled,
			desiredChannel.OtherInfo,
		)
		if err != nil || changed {
			return changed && expectedStatus == common.ChannelStatusEnabled, err
		}
		if attempt+1 == planQuotaDisableMaxAttempts {
			return false, nil
		}
		channel, err = model.GetChannelById(channelID, true)
		if err != nil {
			return false, err
		}
	}
	return false, nil
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

// EnableChannelForHealthCheck recovers only the state observed before the
// probe. Manual and internal callers that intentionally act on current state
// should continue using EnableChannel.
func EnableChannelForHealthCheck(channel *model.Channel, usingKey string) int {
	if channel == nil || channel.Status != common.ChannelStatusAutoDisabled {
		return 0
	}
	if channel.GetDisabledUntil() > time.Now().Unix() {
		return 0
	}
	if channel.ChannelInfo.IsMultiKey {
		if !model.UpdateChannelStatus(channel.Id, usingKey, common.ChannelStatusEnabled, "") {
			return 0
		}
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channel.Name, channel.Id)
		NotifyRootUser(formatNotifyType(channel.Id, common.ChannelStatusEnabled), subject, content)
		return 1
	}
	if isNonMultiKeyPlanChannel(channel) {
		if recoveryKey, owned := PlanQuotaRecoveryDomainKey(channel); owned {
			return enablePlanQuotaDomainForHealthCheck(channel, recoveryKey)
		}
	}

	changed, err := enableSingleKeyChannelSnapshot(channel, false)
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

func enablePlanQuotaDomainForHealthCheck(recoveringSnapshot *model.Channel, recoveryKey string) int {
	changed, err := enableSingleKeyChannelSnapshot(recoveringSnapshot, true)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to recover Plan quota source: channel_id=%d error=%v", recoveringSnapshot.Id, err))
		return 0
	}
	if !changed {
		return 0
	}

	enabled := 1
	if planQuotaSnapshotMatchesCredentialMarker(recoveringSnapshot) {
		channels, err := model.GetAllChannels(0, 0, true, false)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to load Plan quota peers: channel_id=%d error=%v", recoveringSnapshot.Id, err))
		} else {
			now := time.Now().Unix()
			for _, channel := range channels {
				if channel.Id == recoveringSnapshot.Id {
					continue
				}
				candidateKey, candidateOwned := PlanQuotaRecoveryDomainKey(channel)
				if !candidateOwned ||
					candidateKey != recoveryKey ||
					channel.GetDisabledUntil() > now ||
					!planQuotaPeerGenerationMatches(recoveringSnapshot, channel) {
					continue
				}
				changed, err := enableSingleKeyChannelSnapshot(channel, true)
				if err != nil {
					common.SysError(fmt.Sprintf("failed to recover Plan quota channel: channel_id=%d error=%v", channel.Id, err))
					continue
				}
				if changed {
					enabled++
				}
			}
		}
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
	keys := channel.GetKeys()
	return len(keys) == 1 && planQuotaDomainID(channel.Id, keys[0]) == domainID
}

func planQuotaPeerGenerationMatches(source *model.Channel, peer *model.Channel) bool {
	sourceGeneration, sourceExists := source.GetOtherInfo()["quota_generation"]
	peerGeneration, peerExists := peer.GetOtherInfo()["quota_generation"]
	if !sourceExists && !peerExists {
		return true
	}
	sourceValue, sourceValid := sourceGeneration.(string)
	peerValue, peerValid := peerGeneration.(string)
	return sourceValid && peerValid &&
		sourceValue != "" &&
		sourceValue == peerValue
}

func enableSingleKeyChannelSnapshot(channel *model.Channel, clearPlanQuota bool) (bool, error) {
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
	channels, err := model.GetAllChannels(0, 0, true, false)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to load Plan quota domain: channel_id=%d error=%v", recoveringChannel.Id, err))
		return true
	}

	var current *model.Channel
	for _, channel := range channels {
		if channel.Id == recoveringChannel.Id {
			current = channel
			break
		}
	}
	if current == nil {
		common.SysError(fmt.Sprintf("failed to enable Plan quota domain: channel_id=%d not found", recoveringChannel.Id))
		return true
	}

	tag := current.GetTag()
	recoveryKey, owned := PlanQuotaRecoveryDomainKey(current)
	if !owned {
		return false
	}

	recoverOnlyCurrent := !planQuotaSnapshotMatchesCredentialMarker(current)

	enabled := 0
	for _, channel := range channels {
		if recoverOnlyCurrent {
			if channel.Id != current.Id {
				continue
			}
		} else {
			candidateKey, candidateOwned := PlanQuotaRecoveryDomainKey(channel)
			if !candidateOwned || candidateKey != recoveryKey {
				continue
			}
		}
		if channel.Status != common.ChannelStatusAutoDisabled {
			continue
		}

		changed, err := enableSingleKeyChannelSnapshot(channel, true)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to recover Plan quota channel: channel_id=%d error=%v", channel.Id, err))
			continue
		}
		if !changed {
			continue
		}
		enabled++
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
