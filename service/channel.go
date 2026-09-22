package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

var planQuotaResetPattern = regexp.MustCompile(`(?i)reset at\s+(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4} [A-Z]+)`)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	common.SysLog(fmt.Sprintf("通道「%s」（#%d）发生错误，准备禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(fmt.Sprintf("通道「%s」（#%d）未启用自动禁用功能，跳过禁用操作", channelError.ChannelName, channelError.ChannelId))
		return
	}

	if handled, _, err := RecordManagedChannelFailure(channelError, reason); handled {
		if err != nil {
			common.SysError(fmt.Sprintf("failed to record managed channel failure: channel_id=%d error=%v", channelError.ChannelId, err))
		}
		return
	}

	channel, _ := model.CacheGetChannel(channelError.ChannelId)
	resetAt, quotaLimited := ParsePlanQuotaReset(reason)
	if !channelError.IsMultiKey && isNonMultiKeyPlanChannel(channel) && quotaLimited {
		disablePlanQuotaDomain(channel, reason, resetAt)
		return
	}

	success := model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
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

func disablePlanQuotaDomain(failingChannel *model.Channel, reason string, resetAt int64) {
	if failingChannel == nil {
		common.SysError("failed to disable Plan quota domain: channel is nil")
		return
	}
	failingKeys := failingChannel.GetKeys()
	if len(failingKeys) > 1 {
		common.SysError(fmt.Sprintf("failed to disable Plan quota domain: channel_id=%d has multiple credentials", failingChannel.Id))
		return
	}

	credential := ""
	channels := []*model.Channel{failingChannel}
	if len(failingKeys) == 1 {
		credential = failingKeys[0]
		allChannels, err := model.GetAllChannels(0, 0, true, false)
		if err != nil {
			common.SysError(fmt.Sprintf("failed to load Plan quota credential domain: channel_id=%d error=%v", failingChannel.Id, err))
			return
		}
		channels = allChannels
	}
	domainID := planQuotaDomainID(failingChannel.Id, credential)

	disabled := 0
	for _, channel := range channels {
		tag := channel.GetTag()
		keys := channel.GetKeys()
		if credential == "" {
			if channel.Id != failingChannel.Id {
				continue
			}
		} else {
			if !isNonMultiKeyPlanChannel(channel) ||
				len(keys) != 1 ||
				keys[0] != credential {
				continue
			}
		}
		if model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusAutoDisabled, reason) {
			disabled++
		}
		metadata := map[string]interface{}{
			"quota_domain":    tag,
			"quota_domain_id": domainID,
			"quota_type":      "plan",
		}
		if resetAt > 0 {
			metadata["quota_reset_at"] = resetAt
			metadata["disabled_until"] = resetAt + 60
		} else {
			metadata["quota_reset_at"] = nil
			metadata["disabled_until"] = nil
		}
		if err := model.MergeChannelStatusMetadata(channel.Id, metadata); err != nil {
			common.SysError(fmt.Sprintf("failed to persist Plan quota metadata: channel_id=%d error=%v", channel.Id, err))
		}
	}
	if disabled > 0 {
		tag := failingChannel.GetTag()
		subject := fmt.Sprintf("Plan 配额域「%s」已临时禁用", tag)
		content := fmt.Sprintf("%s，共禁用 %d 个协议渠道", reason, disabled)
		NotifyRootUser("channel_plan_quota_"+tag, subject, content)
	}
}

func EnableChannel(channelId int, usingKey string, channelName string) {
	channel, _ := model.CacheGetChannel(channelId)
	if isNonMultiKeyPlanChannel(channel) {
		enablePlanQuotaDomain(channel)
		return
	}
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

func enablePlanQuotaDomain(recoveringChannel *model.Channel) {
	if recoveringChannel == nil {
		common.SysError("failed to enable Plan quota domain: channel is nil")
		return
	}
	channels, err := model.GetAllChannels(0, 0, true, false)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to load Plan quota domain: channel_id=%d error=%v", recoveringChannel.Id, err))
		return
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
		return
	}

	tag := current.GetTag()
	domainValue, hasDomainID := current.GetOtherInfo()["quota_domain_id"]
	domainID, domainIDValid := domainValue.(string)
	if hasDomainID && (!domainIDValid || domainID == "") {
		common.SysError(fmt.Sprintf("failed to enable Plan quota domain: channel_id=%d has invalid quota_domain_id", current.Id))
		return
	}

	enabled := 0
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusAutoDisabled {
			continue
		}
		candidateValue, candidateHasDomainID := channel.GetOtherInfo()["quota_domain_id"]
		if hasDomainID {
			candidateDomainID, ok := candidateValue.(string)
			if !ok || candidateDomainID != domainID {
				continue
			}
		} else if candidateHasDomainID || channel.GetTag() != tag {
			continue
		}
		if !model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusEnabled, "") {
			continue
		}
		enabled++
		if err := model.MergeChannelStatusMetadata(channel.Id, map[string]interface{}{
			"disabled_until":  nil,
			"quota_reset_at":  nil,
			"quota_domain":    nil,
			"quota_domain_id": nil,
			"quota_type":      nil,
		}); err != nil {
			common.SysError(fmt.Sprintf("failed to clear Plan quota metadata: channel_id=%d error=%v", channel.Id, err))
		}
	}
	if enabled > 0 {
		NotifyRootUser("channel_plan_quota_recovered_"+tag,
			fmt.Sprintf("Plan 配额域「%s」已恢复", tag),
			fmt.Sprintf("已恢复 %d 个协议渠道", enabled))
	}
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
	if _, quotaLimited := ParsePlanQuotaReset(err.Error()); quotaLimited {
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
