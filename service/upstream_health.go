package service

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"gorm.io/gorm"
)

var upstreamRecoveryBackoff = []time.Duration{
	0,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	4 * time.Hour,
}

type managedRouteAdminCacheEntry struct {
	info      map[string]any
	expiresAt int64
}

var managedRouteAdminCache sync.Map
var managedModelExclusionMutex sync.Mutex

const managedModelExclusionsOption = "upstream_orchestration.model_exclusions"

func GetManagedRouteAdminInfo(channelID int) map[string]any {
	now := common.GetTimestamp()
	if cached, ok := managedRouteAdminCache.Load(channelID); ok {
		entry := cached.(managedRouteAdminCacheEntry)
		if entry.expiresAt > now {
			return entry.info
		}
	}
	var route model.UpstreamManagedRoute
	if err := model.DB.
		Where("channel_id = ? AND detached = ?", channelID, false).
		First(&route).Error; err != nil {
		return nil
	}
	var source model.UpstreamSource
	if err := model.DB.Where("id = ?", route.SourceID).First(&source).Error; err != nil {
		return nil
	}
	var group model.UpstreamGroup
	if err := model.DB.
		Where("source_id = ? AND external_id = ?", route.SourceID, route.ExternalGroupID).
		First(&group).Error; err != nil {
		return nil
	}
	info := map[string]any{
		"source":               source.Key,
		"group":                group.Name,
		"external_group_id":    route.ExternalGroupID,
		"protocol":             route.Protocol,
		"state":                route.State,
		"effective_multiplier": route.EffectiveMultiplier,
		"selected_endpoint":    source.SelectedEndpoint,
		"health_status":        group.HealthStatus,
		"health_sample_age":    max(int64(0), now-group.ObservedAt),
	}
	if group.Availability != nil {
		info["availability"] = *group.Availability
	}
	managedRouteAdminCache.Store(channelID, managedRouteAdminCacheEntry{info: info, expiresAt: now + 60})
	return info
}

func invalidateManagedRouteAdminInfo(channelID int) {
	managedRouteAdminCache.Delete(channelID)
}

func IsManagedChannel(channelID int) bool {
	if channelID <= 0 || !operation_setting.GetUpstreamOrchestrationSetting().Enabled {
		return false
	}
	_, err := model.GetUpstreamManagedRouteByChannelID(channelID)
	return err == nil
}

func ShouldRecordManagedRouteFailure(err *types.NewAPIError) bool {
	if err == nil || types.IsSkipRetryError(err) {
		return false
	}
	if IsManagedModelUnsupported(err) {
		return true
	}
	if types.IsChannelError(err) {
		return true
	}
	switch err.StatusCode {
	case 401, 403, 429:
		return true
	default:
		return err.StatusCode >= 500 && err.StatusCode <= 599
	}
}

func IsManagedModelUnsupported(err *types.NewAPIError) bool {
	if err == nil || err.StatusCode != 404 {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "model") &&
		strings.Contains(message, "not supported by any configured account")
}

func IsolateManagedRouteModel(channelID int, modelName string, reason string) (bool, error) {
	return isolateManagedRouteModel(
		channelID,
		modelName,
		reason,
		model.PublishOptionValue,
	)
}

func isolateManagedRouteModel(
	channelID int,
	modelName string,
	reason string,
	publishOptionValue func(string, string) error,
) (bool, error) {
	modelName = strings.TrimSpace(modelName)
	if channelID <= 0 || modelName == "" {
		return false, nil
	}
	managedModelExclusionMutex.Lock()
	defer managedModelExclusionMutex.Unlock()

	route, err := model.GetUpstreamManagedRouteByChannelID(channelID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	isolated, optionValue, err := model.IsolateManagedRouteModel(
		route,
		modelName,
		reason,
		common.GetTimestamp(),
		managedModelExclusionsOption,
	)
	if err != nil {
		return false, err
	}
	if !isolated {
		return false, nil
	}
	publishErr := publishOptionValue(managedModelExclusionsOption, optionValue)
	model.InitChannelCache()
	invalidateManagedRouteAdminInfo(channelID)
	return true, publishErr
}

func RecordManagedChannelFailure(channelError types.ChannelError, reason string) (bool, bool, error) {
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	if !setting.Enabled {
		return false, false, nil
	}
	route, quarantined, err := model.RecordUpstreamRouteFailure(
		channelError.ChannelId,
		common.GetTimestamp(),
		int64(setting.FailureWindowMinutes*60),
		setting.FailureThreshold,
		common.LocalLogPreview(reason),
	)
	if err != nil {
		return false, false, err
	}
	if route == nil {
		return false, false, nil
	}
	if quarantined {
		invalidateManagedRouteAdminInfo(channelError.ChannelId)
		if model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason) {
			if err := NotifyRootBark(
				fmt.Sprintf("%s_managed_%d", dto.NotifyTypeChannelUpdate, channelError.ChannelId),
				fmt.Sprintf("受管通道「%s」（#%d）已隔离", channelError.ChannelName, channelError.ChannelId),
				fmt.Sprintf("%d 分钟内连续错误达到 %d 次，已停止生产流量。原因：%s", setting.FailureWindowMinutes, setting.FailureThreshold, common.LocalLogPreview(reason)),
			); err != nil {
				common.SysLog("upstream Bark notification skipped: " + err.Error())
			}
		}
	}
	return true, quarantined, nil
}

func RecordManagedChannelSuccess(channelID int, elapsed time.Duration) {
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	if !setting.Enabled || channelID <= 0 {
		return
	}
	route, err := model.RecordUpstreamRouteSuccess(channelID, common.GetTimestamp(), elapsed.Milliseconds())
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			common.SysLog(fmt.Sprintf("record managed upstream success failed: channel_id=%d error=%v", channelID, err))
		}
		return
	}
	if route == nil {
		return
	}
	invalidateManagedRouteAdminInfo(channelID)
	enqueueStaleManagedRouteProbes(route, common.GetTimestamp(), setting.ProbeFreshnessMinutes*60)
}

func enqueueStaleManagedRouteProbes(current *model.UpstreamManagedRoute, now int64, freshnessSeconds int) {
	if current == nil || freshnessSeconds <= 0 {
		return
	}
	var routes []model.UpstreamManagedRoute
	if err := model.DB.Where(
		"platform = ? AND protocol = ? AND detached = ? AND channel_id <> ? AND state IN ? AND last_success_at < ?",
		current.Platform,
		current.Protocol,
		false,
		current.ChannelID,
		[]string{model.UpstreamRouteStateActive, model.UpstreamRouteStateQuarantined},
		now-int64(freshnessSeconds),
	).Order("rank asc").Limit(operation_setting.GetUpstreamOrchestrationSetting().CandidateLimit).Find(&routes).Error; err != nil {
		common.SysLog("load stale managed routes failed: " + err.Error())
		return
	}
	for _, route := range routes {
		if route.NextProbeAt > now {
			continue
		}
		if err := model.DB.Model(&model.UpstreamManagedRoute{}).
			Where("id = ? AND next_probe_at <= ?", route.ID, now).
			Updates(map[string]any{"next_probe_at": now, "updated_at": now}).Error; err != nil {
			common.SysLog(fmt.Sprintf("schedule managed route probe failed: route_id=%d error=%v", route.ID, err))
		}
	}
}

func PauseManagedRoute(routeID int64, reason string) error {
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	now := common.GetTimestamp()
	var route model.UpstreamManagedRoute
	if err := model.DB.First(&route, routeID).Error; err != nil {
		return err
	}
	if route.Detached {
		return errors.New("managed route is detached")
	}
	until := now + int64(setting.ManualPauseHours*3600)
	if err := model.DB.Model(&route).Updates(map[string]any{
		"state":              model.UpstreamRouteStatePaused,
		"manual_pause_until": until,
		"last_reason":        strings.TrimSpace(reason),
		"updated_at":         now,
	}).Error; err != nil {
		return err
	}
	model.UpdateChannelStatus(route.ChannelID, "", common.ChannelStatusManuallyDisabled, "upstream orchestration manual pause")
	invalidateManagedRouteAdminInfo(route.ChannelID)
	return nil
}

func ResumeManagedRoute(routeID int64) error {
	now := common.GetTimestamp()
	var route model.UpstreamManagedRoute
	if err := model.DB.First(&route, routeID).Error; err != nil {
		return err
	}
	if route.Detached {
		return errors.New("managed route is detached")
	}
	if err := model.DB.Model(&route).Updates(map[string]any{
		"state":              model.UpstreamRouteStateShadow,
		"manual_pause_until": int64(0),
		"next_probe_at":      now,
		"last_reason":        "",
		"updated_at":         now,
	}).Error; err != nil {
		return err
	}
	invalidateManagedRouteAdminInfo(route.ChannelID)
	return nil
}

func DetachManagedRoute(routeID int64) error {
	now := common.GetTimestamp()
	result := model.DB.Model(&model.UpstreamManagedRoute{}).Where("id = ?", routeID).Updates(map[string]any{
		"state":      model.UpstreamRouteStateDetached,
		"detached":   true,
		"updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	var route model.UpstreamManagedRoute
	if model.DB.First(&route, routeID).Error == nil {
		invalidateManagedRouteAdminInfo(route.ChannelID)
	}
	return nil
}

type ManagedRouteProbeResult struct {
	Applied bool
	State   string
}

func MarkManagedRouteProbeResult(
	route *model.UpstreamManagedRoute,
	succeeded bool,
	latencyMS int64,
	reason string,
) (ManagedRouteProbeResult, error) {
	result := ManagedRouteProbeResult{}
	if route == nil || route.ID == 0 {
		return result, errors.New("managed route probe snapshot is missing")
	}
	now := common.GetTimestamp()
	if route.Detached ||
		route.State == model.UpstreamRouteStateLongRed ||
		route.State == model.UpstreamRouteStatePaused {
		return result, nil
	}
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	desired := *route
	if succeeded {
		nextState := model.UpstreamRouteStateActive
		successes := route.ConsecutiveSuccesses + 1
		if route.State == model.UpstreamRouteStateShadow &&
			(!setting.Enabled || successes < setting.ShadowSuccessesRequired) {
			nextState = model.UpstreamRouteStateShadow
		}
		desired.State = nextState
		desired.ConsecutiveFailures = 0
		desired.ConsecutiveSuccesses = successes
		desired.FailureWindowStart = 0
		desired.RecoveryAttempts = 0
		desired.NextProbeAt = nextProbeAfterSuccess(nextState, now)
		desired.LastProbeAt = now
		desired.LastSuccessAt = now
		desired.LastLatencyMS = latencyMS
		desired.LastReason = ""
		desired.UpdatedAt = now
		applied, _, err := model.UpdateManagedRouteProbeResultIfUnchanged(
			route,
			&desired,
			false,
			"",
			now,
		)
		if err != nil {
			return result, err
		}
		if applied {
			invalidateManagedRouteAdminInfo(route.ChannelID)
			result.Applied = true
			result.State = desired.State
		}
		return result, nil
	}

	desired.ConsecutiveSuccesses = 0
	desired.LastProbeAt = now
	desired.LastFailureAt = now
	desired.LastReason = common.LocalLogPreview(reason)
	desired.UpdatedAt = now
	quarantined := false
	if route.State == model.UpstreamRouteStateActive {
		windowSeconds := int64(setting.FailureWindowMinutes * 60)
		if desired.FailureWindowStart == 0 || now-desired.FailureWindowStart > windowSeconds {
			desired.FailureWindowStart = now
			desired.ConsecutiveFailures = 1
		} else {
			desired.ConsecutiveFailures++
		}
		desired.NextProbeAt = now + 60
		if desired.ConsecutiveFailures >= setting.FailureThreshold {
			desired.State = model.UpstreamRouteStateQuarantined
			desired.RecoveryAttempts = 0
			desired.NextProbeAt = now
			quarantined = true
		}
	} else {
		desired.RecoveryAttempts++
		desired.NextProbeAt = 0
		if desired.RecoveryAttempts < len(upstreamRecoveryBackoff) {
			desired.NextProbeAt = now + int64(upstreamRecoveryBackoff[desired.RecoveryAttempts].Seconds())
		}
		desired.State = model.UpstreamRouteStateQuarantined
		if route.State == model.UpstreamRouteStateShadow {
			desired.State = model.UpstreamRouteStateShadow
		}
	}
	applied, channelDisabled, err := model.UpdateManagedRouteProbeResultIfUnchanged(
		route,
		&desired,
		quarantined,
		reason,
		now,
	)
	if err != nil || !applied {
		return result, err
	}
	invalidateManagedRouteAdminInfo(route.ChannelID)
	result.Applied = true
	result.State = desired.State
	if channelDisabled {
		if err := NotifyRootBark(
			fmt.Sprintf("%s_managed_%d", dto.NotifyTypeChannelUpdate, route.ChannelID),
			fmt.Sprintf("受管通道「%s」（#%d）已隔离", managedRouteChannelName(route.ChannelID), route.ChannelID),
			fmt.Sprintf("%d 分钟内连续错误达到 %d 次，已停止生产流量。原因：%s", setting.FailureWindowMinutes, setting.FailureThreshold, common.LocalLogPreview(reason)),
		); err != nil {
			common.SysLog("upstream Bark notification skipped: " + err.Error())
		}
	}
	return result, nil
}

func nextProbeAfterSuccess(state string, now int64) int64 {
	if state == model.UpstreamRouteStateShadow {
		return now + 60
	}
	return 0
}

func managedRouteChannelName(channelID int) string {
	channel, err := model.GetChannelById(channelID, false)
	if err != nil || channel == nil {
		return fmt.Sprintf("#%d", channelID)
	}
	return channel.Name
}
