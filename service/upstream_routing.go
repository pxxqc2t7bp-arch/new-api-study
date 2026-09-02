package service

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	rootdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"gorm.io/gorm"
)

type UpstreamReconcileSummary struct {
	SourcesChecked    int `json:"sources_checked"`
	GroupsChecked     int `json:"groups_checked"`
	EnrollmentQueued  int `json:"enrollment_queued"`
	RoutesActivated   int `json:"routes_activated"`
	RoutesQuarantined int `json:"routes_quarantined"`
	RoutesLongRed     int `json:"routes_long_red"`
	PrioritiesUpdated int `json:"priorities_updated"`
}

type upstreamRouteCandidate struct {
	source model.UpstreamSource
	group  model.UpstreamGroup
	models []string
}

func ReconcileManagedUpstreams(now time.Time) (UpstreamReconcileSummary, error) {
	var summary UpstreamReconcileSummary
	setting := operation_setting.GetUpstreamOrchestrationSetting()
	if !setting.Enabled {
		return summary, nil
	}
	sources, err := model.ListUpstreamSources()
	if err != nil {
		return summary, err
	}
	groups, err := model.ListUpstreamGroups()
	if err != nil {
		return summary, err
	}
	routes, err := model.ListUpstreamManagedRoutes()
	if err != nil {
		return summary, err
	}
	summary.SourcesChecked = len(sources)
	summary.GroupsChecked = len(groups)
	var routeChanges []string

	sourceByID := make(map[int64]model.UpstreamSource, len(sources))
	for index := range sources {
		source := sources[index]
		if source.Balance != nil {
			low := *source.Balance < source.LowBalanceThreshold
			if low && !source.LowBalanceAlerted {
				message := fmt.Sprintf("%s 余额 $%.2f，低于阈值 $%.2f", source.Name, *source.Balance, source.LowBalanceThreshold)
				if err := NotifyRootBark(
					fmt.Sprintf("channel_update_upstream_balance_%s", source.Key),
					"New API 上游余额告警",
					message,
				); err != nil {
					common.SysLog("upstream Bark notification skipped: " + err.Error())
				}
			}
			if low != source.LowBalanceAlerted {
				_ = model.DB.Model(&model.UpstreamSource{}).Where("id = ?", source.ID).
					Updates(map[string]any{"low_balance_alerted": low, "updated_at": now.Unix()}).Error
				source.LowBalanceAlerted = low
				sources[index] = source
			}
		}
		sourceByID[source.ID] = source
	}
	groupByIdentity := make(map[string]model.UpstreamGroup, len(groups))
	for _, group := range groups {
		groupByIdentity[upstreamGroupIdentity(group.SourceID, group.ExternalID)] = group
	}

	for i := range routes {
		route := &routes[i]
		if route.Detached {
			continue
		}
		source, sourceExists := sourceByID[route.SourceID]
		group, groupExists := groupByIdentity[upstreamGroupIdentity(route.SourceID, route.ExternalGroupID)]
		if !sourceExists || !groupExists {
			continue
		}
		state, reason := desiredManagedRouteState(*route, source, group, now, setting)
		if state == route.State {
			continue
		}
		updates := map[string]any{
			"state":       state,
			"last_reason": reason,
			"updated_at":  now.Unix(),
		}
		if state == model.UpstreamRouteStateQuarantined {
			if route.RedSince == 0 {
				updates["red_since"] = now.Unix()
			}
			updates["recovery_attempts"] = 0
			updates["next_probe_at"] = now.Unix()
			summary.RoutesQuarantined++
		}
		if state == model.UpstreamRouteStateLongRed {
			updates["next_probe_at"] = int64(0)
			summary.RoutesLongRed++
		}
		if state == model.UpstreamRouteStateActive {
			updates["red_since"] = int64(0)
			updates["recovery_attempts"] = 0
			updates["next_probe_at"] = int64(0)
			updates["consecutive_failures"] = 0
			updates["failure_window_start"] = int64(0)
			summary.RoutesActivated++
		}
		if err := model.DB.Model(route).Updates(updates).Error; err != nil {
			return summary, err
		}
		routeChanges = append(routeChanges, fmt.Sprintf(
			"#%d %s/%s: %s -> %s",
			route.ChannelID,
			source.Key,
			group.Name,
			route.State,
			state,
		))
		if state == model.UpstreamRouteStateActive {
			model.UpdateChannelStatus(route.ChannelID, "", common.ChannelStatusEnabled, "")
		} else {
			model.UpdateChannelStatus(route.ChannelID, "", common.ChannelStatusAutoDisabled, reason)
		}
		route.State = state
	}

	candidates, err := buildUpstreamRouteCandidates(sources, groups, now, setting)
	if err != nil {
		return summary, err
	}
	if setting.AutoEnroll {
		queued, queueErr := enqueueMissingUpstreamEnrollments(candidates, routes, setting)
		if queueErr != nil {
			return summary, queueErr
		}
		summary.EnrollmentQueued = queued
	}
	updated, err := rankManagedRoutes(now, sources, groups)
	if err != nil {
		return summary, err
	}
	summary.PrioritiesUpdated = updated
	if updated > 0 {
		model.InitChannelCache()
	}
	if len(routeChanges) > 0 {
		if err := NotifyRootBark(
			"channel_update_upstream_reconcile",
			"New API 上游线路状态变化",
			strings.Join(routeChanges, "\n"),
		); err != nil {
			common.SysLog("upstream Bark notification skipped: " + err.Error())
		}
	}
	return summary, nil
}

func desiredManagedRouteState(
	route model.UpstreamManagedRoute,
	source model.UpstreamSource,
	group model.UpstreamGroup,
	now time.Time,
	setting *operation_setting.UpstreamOrchestrationSetting,
) (string, string) {
	if route.Detached {
		return model.UpstreamRouteStateDetached, "detached"
	}
	if route.ManualPauseUntil > now.Unix() {
		return model.UpstreamRouteStatePaused, "manual pause"
	}
	if route.State == model.UpstreamRouteStatePaused {
		route.State = model.UpstreamRouteStateShadow
	}
	if source.LastSnapshotAt == 0 ||
		now.Unix()-source.LastSnapshotAt > int64((setting.SyncIntervalHours+1)*3600) {
		return route.State, route.LastReason
	}
	if group.ObservedAt == 0 ||
		now.Unix()-group.ObservedAt > int64((setting.SyncIntervalHours+1)*3600) {
		return route.State, route.LastReason
	}
	if !source.Enabled {
		return model.UpstreamRouteStateQuarantined, "source disabled"
	}
	if source.Balance != nil && *source.Balance <= 0 {
		return model.UpstreamRouteStateQuarantined, "source balance exhausted"
	}
	switch group.HealthStatus {
	case model.UpstreamHealthFailed, model.UpstreamHealthError:
		redSince := group.RedSince
		if redSince == 0 {
			redSince = now.Unix()
		}
		if now.Unix()-redSince >= int64(setting.RedLongTermHours*3600) {
			return model.UpstreamRouteStateLongRed, "upstream monitor red for 24 hours"
		}
		return model.UpstreamRouteStateQuarantined, "upstream monitor red"
	case model.UpstreamHealthOperational, model.UpstreamHealthDegraded:
		if route.State != model.UpstreamRouteStateShadow {
			return model.UpstreamRouteStateActive, ""
		}
	}
	return route.State, route.LastReason
}

func buildUpstreamRouteCandidates(
	sources []model.UpstreamSource,
	groups []model.UpstreamGroup,
	now time.Time,
	setting *operation_setting.UpstreamOrchestrationSetting,
) ([]upstreamRouteCandidate, error) {
	sourceByID := make(map[int64]model.UpstreamSource, len(sources))
	for _, source := range sources {
		sourceByID[source.ID] = source
	}
	modelVendors, err := loadManagedModelVendors()
	if err != nil {
		return nil, err
	}
	var candidates []upstreamRouteCandidate
	for _, group := range groups {
		source, ok := sourceByID[group.SourceID]
		if !ok || !source.Enabled || source.SelectedEndpoint == "" {
			continue
		}
		if source.LastSnapshotAt == 0 || now.Unix()-source.LastSnapshotAt > int64((setting.SyncIntervalHours+1)*3600) {
			continue
		}
		if group.ObservedAt == 0 || now.Unix()-group.ObservedAt > int64((setting.SyncIntervalHours+1)*3600) {
			continue
		}
		if source.Balance != nil && *source.Balance <= 0 {
			continue
		}
		if group.EffectiveMultiplier <= 0 || group.EffectiveMultiplier > setting.MaxUpstreamMultiplier {
			continue
		}
		if group.HealthStatus == model.UpstreamHealthFailed || group.HealthStatus == model.UpstreamHealthError {
			continue
		}
		var upstreamModels []string
		if err := common.UnmarshalJsonStr(group.Models, &upstreamModels); err != nil {
			continue
		}
		eligible := make([]string, 0, len(upstreamModels))
		for _, upstreamModel := range upstreamModels {
			canonical := canonicalManagedModelName(upstreamModel, setting.ModelAliases)
			if !managedModelMatchesPlatform(canonical, group.Platform, modelVendors) || !hasConfiguredModelPrice(canonical) {
				continue
			}
			eligible = append(eligible, canonical)
		}
		eligible = uniqueSortedStrings(eligible)
		if len(eligible) == 0 {
			continue
		}
		candidates = append(candidates, upstreamRouteCandidate{
			source: source,
			group:  group,
			models: eligible,
		})
	}
	return selectUpstreamCandidateGroups(candidates, setting.CandidateLimit), nil
}

func selectUpstreamCandidateGroups(candidates []upstreamRouteCandidate, limit int) []upstreamRouteCandidate {
	if limit <= 0 {
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return lessUpstreamCandidate(candidates[i], candidates[j])
	})
	selected := make(map[string]upstreamRouteCandidate)
	modelCandidates := make(map[string][]upstreamRouteCandidate)
	for _, candidate := range candidates {
		for _, modelName := range candidate.models {
			key := candidate.group.Platform + "\x00" + modelName
			modelCandidates[key] = append(modelCandidates[key], candidate)
		}
	}
	for _, perModel := range modelCandidates {
		seenSources := make(map[int64]struct{})
		chosen := make([]upstreamRouteCandidate, 0, limit)
		for _, candidate := range perModel {
			if _, exists := seenSources[candidate.source.ID]; exists {
				continue
			}
			seenSources[candidate.source.ID] = struct{}{}
			chosen = append(chosen, candidate)
			if len(chosen) == limit {
				break
			}
		}
		for _, candidate := range perModel {
			if len(chosen) == limit {
				break
			}
			if slices.ContainsFunc(chosen, func(item upstreamRouteCandidate) bool {
				return item.group.SourceID == candidate.group.SourceID && item.group.ExternalID == candidate.group.ExternalID
			}) {
				continue
			}
			chosen = append(chosen, candidate)
		}
		for _, candidate := range chosen {
			selected[upstreamGroupIdentity(candidate.group.SourceID, candidate.group.ExternalID)] = candidate
		}
	}
	result := make([]upstreamRouteCandidate, 0, len(selected))
	for _, candidate := range selected {
		result = append(result, candidate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return lessUpstreamCandidate(result[i], result[j])
	})
	return result
}

func lessUpstreamCandidate(left upstreamRouteCandidate, right upstreamRouteCandidate) bool {
	if left.group.EffectiveMultiplier != right.group.EffectiveMultiplier {
		return left.group.EffectiveMultiplier < right.group.EffectiveMultiplier
	}
	leftAvailability := float64(-1)
	rightAvailability := float64(-1)
	if left.group.Availability != nil {
		leftAvailability = *left.group.Availability
	}
	if right.group.Availability != nil {
		rightAvailability = *right.group.Availability
	}
	if leftAvailability != rightAvailability {
		return leftAvailability > rightAvailability
	}
	leftLatency := int64(math.MaxInt64)
	rightLatency := int64(math.MaxInt64)
	if left.group.LatencyMS != nil {
		leftLatency = *left.group.LatencyMS
	}
	if right.group.LatencyMS != nil {
		rightLatency = *right.group.LatencyMS
	}
	if leftLatency != rightLatency {
		return leftLatency < rightLatency
	}
	if left.source.Key != right.source.Key {
		return left.source.Key < right.source.Key
	}
	return left.group.ExternalID < right.group.ExternalID
}

func enqueueMissingUpstreamEnrollments(
	candidates []upstreamRouteCandidate,
	routes []model.UpstreamManagedRoute,
	setting *operation_setting.UpstreamOrchestrationSetting,
) (int, error) {
	existing := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		existing[upstreamGroupIdentity(route.SourceID, route.ExternalGroupID)] = struct{}{}
	}
	queued := 0
	for _, candidate := range candidates {
		identity := upstreamGroupIdentity(candidate.source.ID, candidate.group.ExternalID)
		if _, ok := existing[identity]; ok {
			continue
		}
		pending, err := hasPendingEnrollment(candidate.source.Key, candidate.group.ExternalID)
		if err != nil {
			return queued, err
		}
		if pending {
			continue
		}
		payload := rootdto.UpstreamEnrollmentCommand{
			SourceKey:       candidate.source.Key,
			ExternalGroupID: candidate.group.ExternalID,
			GroupName:       candidate.group.Name,
			Platform:        candidate.group.Platform,
			APIBaseURL:      candidate.source.SelectedEndpoint,
			Models:          candidate.models,
			KeyName:         managedUpstreamKeyName(candidate.source.Key, candidate.group.ExternalID),
			IPWhitelist:     append([]string(nil), setting.StaticEgressIPs[candidate.source.Key]...),
		}
		if _, err := model.CreateUpstreamSyncCommand("", upstreamSyncCommandEnroll, candidate.source.Key, payload); err != nil {
			return queued, err
		}
		existing[identity] = struct{}{}
		queued++
	}
	return queued, nil
}

func hasPendingEnrollment(sourceKey string, externalGroupID string) (bool, error) {
	var commands []model.UpstreamSyncCommand
	if err := model.DB.Where(
		"type = ? AND source_key = ? AND status IN ?",
		upstreamSyncCommandEnroll,
		sourceKey,
		[]string{model.UpstreamSyncCommandPending, model.UpstreamSyncCommandRunning},
	).Find(&commands).Error; err != nil {
		return false, err
	}
	for _, command := range commands {
		var payload rootdto.UpstreamEnrollmentCommand
		if common.UnmarshalJsonStr(command.Payload, &payload) == nil && payload.ExternalGroupID == externalGroupID {
			return true, nil
		}
	}
	return false, nil
}

func rankManagedRoutes(now time.Time, sources []model.UpstreamSource, groups []model.UpstreamGroup) (int, error) {
	sourceByID := make(map[int64]model.UpstreamSource, len(sources))
	for _, source := range sources {
		sourceByID[source.ID] = source
	}
	groupByIdentity := make(map[string]model.UpstreamGroup, len(groups))
	for _, group := range groups {
		groupByIdentity[upstreamGroupIdentity(group.SourceID, group.ExternalID)] = group
	}
	routes, err := model.ListUpstreamManagedRoutes()
	if err != nil {
		return 0, err
	}
	sort.SliceStable(routes, func(i, j int) bool {
		leftGroup := groupByIdentity[upstreamGroupIdentity(routes[i].SourceID, routes[i].ExternalGroupID)]
		rightGroup := groupByIdentity[upstreamGroupIdentity(routes[j].SourceID, routes[j].ExternalGroupID)]
		return lessUpstreamCandidate(
			upstreamRouteCandidate{source: sourceByID[routes[i].SourceID], group: leftGroup},
			upstreamRouteCandidate{source: sourceByID[routes[j].SourceID], group: rightGroup},
		)
	})
	rankByProtocol := map[string]int{}
	updated := 0
	for index := range routes {
		route := &routes[index]
		if route.Detached || route.State != model.UpstreamRouteStateActive {
			continue
		}
		group, ok := groupByIdentity[upstreamGroupIdentity(route.SourceID, route.ExternalGroupID)]
		if !ok {
			continue
		}
		rankByProtocol[route.Protocol]++
		rank := rankByProtocol[route.Protocol]
		priority := int64(1000 - rank)
		selectedEndpoint := sourceByID[route.SourceID].SelectedEndpoint
		result := model.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.UpstreamManagedRoute{}).Where("id = ?", route.ID).Updates(map[string]any{
				"rank":                 rank,
				"effective_multiplier": group.EffectiveMultiplier,
				"updated_at":           now.Unix(),
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.Channel{}).Where("id = ?", route.ChannelID).Updates(map[string]any{
				"priority": priority,
				"base_url": selectedEndpoint,
			}).Error; err != nil {
				return err
			}
			return tx.Model(&model.Ability{}).Where("channel_id = ?", route.ChannelID).Updates(map[string]any{
				"priority": priority,
			}).Error
		})
		if result != nil {
			return updated, result
		}
		updated++
	}
	return updated, nil
}

func loadManagedModelVendors() (map[string]string, error) {
	var rows []struct {
		ModelName  string
		VendorName string
	}
	err := model.DB.Table("models").
		Select("models.model_name as model_name, vendors.name as vendor_name").
		Joins("JOIN vendors ON vendors.id = models.vendor_id").
		Where("models.status = ?", 1).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(rows))
	for _, row := range rows {
		result[row.ModelName] = strings.ToLower(strings.TrimSpace(row.VendorName))
	}
	var groups []model.UpstreamGroup
	if err := model.DB.Select("platform", "models").Find(&groups).Error; err != nil {
		return nil, err
	}
	for _, group := range groups {
		var modelNames []string
		if common.UnmarshalJsonStr(group.Models, &modelNames) != nil {
			continue
		}
		vendor := strings.ToLower(strings.TrimSpace(group.Platform))
		if vendor == "grok" {
			vendor = "xai"
		}
		for _, modelName := range modelNames {
			if _, exists := result[modelName]; !exists {
				result[modelName] = vendor
			}
		}
	}
	return result, nil
}

func managedModelMatchesPlatform(modelName string, platform string, vendors map[string]string) bool {
	vendor, ok := vendors[modelName]
	if !ok {
		return false
	}
	platform = strings.ToLower(strings.TrimSpace(platform))
	switch platform {
	case "openai":
		return strings.Contains(vendor, "openai") || strings.Contains(vendor, "chatgpt")
	case "anthropic":
		return strings.Contains(vendor, "anthropic") || strings.Contains(vendor, "claude")
	case "grok":
		return strings.Contains(vendor, "xai") || strings.Contains(vendor, "grok")
	default:
		return false
	}
}

func hasConfiguredModelPrice(modelName string) bool {
	if expression, ok := billing_setting.GetBillingExpr(modelName); ok && strings.TrimSpace(expression) != "" {
		return true
	}
	if _, ok := ratio_setting.GetModelPrice(modelName, false); ok {
		return true
	}
	_, ok, _ := ratio_setting.GetModelRatio(modelName)
	return ok
}

func canonicalManagedModelName(modelName string, aliases map[string]string) string {
	modelName = strings.TrimSpace(modelName)
	if alias, ok := aliases[modelName]; ok {
		return strings.TrimSpace(alias)
	}
	return modelName
}

func managedUpstreamKeyName(sourceKey string, externalGroupID string) string {
	value := strings.NewReplacer(" ", "-", "/", "-", ":", "-").Replace(externalGroupID)
	return truncateRunes(fmt.Sprintf("newapi-managed-%s-%s", sourceKey, value), 64)
}

func upstreamGroupIdentity(sourceID int64, externalGroupID string) string {
	return fmt.Sprintf("%d\x00%s", sourceID, strings.TrimSpace(externalGroupID))
}
