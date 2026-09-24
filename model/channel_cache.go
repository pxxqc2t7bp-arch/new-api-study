package model

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

var group2model2channels map[string]map[string][]int // enabled channel
var channelsIDM map[int]*Channel                     // all channels include disabled
// channel2advancedCustomConfig caches parsed Advanced Custom (type 58) configs so
// path-aware selection avoids re-parsing JSON per request. Refreshed on full sync.
var channel2advancedCustomConfig map[int]*kitdto.AdvancedCustomConfig
var channelSyncLock sync.RWMutex

func InitChannelCache() {
	if !common.MemoryCacheEnabled {
		InvalidatePricingCache()
		rebuildTaskAliasView()
		return
	}
	channelSyncLock.Lock()
	newChannelId2channel := make(map[int]*Channel)
	newChannel2advancedCustomConfig := make(map[int]*kitdto.AdvancedCustomConfig)
	var channels []*Channel
	DB.Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
				newChannel2advancedCustomConfig[channel.Id] = config
			}
		}
	}
	var abilities []*Ability
	DB.Find(&abilities)
	groups := make(map[string]bool)
	for _, ability := range abilities {
		groups[ability.Group] = true
	}
	newGroup2model2channels := make(map[string]map[string][]int)
	for group := range groups {
		newGroup2model2channels[group] = make(map[string][]int)
	}
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled {
			continue // skip disabled channels
		}
		groups := strings.Split(channel.Group, ",")
		for _, group := range groups {
			models := strings.Split(channel.Models, ",")
			for _, model := range models {
				if _, ok := newGroup2model2channels[group][model]; !ok {
					newGroup2model2channels[group][model] = make([]int, 0)
				}
				newGroup2model2channels[group][model] = append(newGroup2model2channels[group][model], channel.Id)
			}
		}
	}

	// sort by priority
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return newChannelId2channel[channels[i]].GetPriority() > newChannelId2channel[channels[j]].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	group2model2channels = newGroup2model2channels
	//channelsIDM = newChannelId2channel
	for i, channel := range newChannelId2channel {
		if channel.ChannelInfo.IsMultiKey {
			channel.Keys = channel.GetKeys()
			if channel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
				if oldChannel, ok := channelsIDM[i]; ok {
					// 存在旧的渠道，如果是多key且轮询，保留轮询索引信息
					if oldChannel.ChannelInfo.IsMultiKey && oldChannel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
						channel.ChannelInfo.MultiKeyPollingIndex = oldChannel.ChannelInfo.MultiKeyPollingIndex
					}
				}
			}
		}
	}
	channelsIDM = newChannelId2channel
	channel2advancedCustomConfig = newChannel2advancedCustomConfig
	channelSyncLock.Unlock()
	// Lock ordering: InvalidatePricingCache acquires updatePricingLock, and
	// GetPricing (holding updatePricingLock) nests channelSyncLock.RLock via
	// loadPricingAdvancedCustomConfigs. channelSyncLock MUST be released before
	// invalidating the pricing cache, otherwise the reversed order deadlocks.
	InvalidatePricingCache()
	rebuildTaskAliasView()
	common.SysLog("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog("syncing channels from database")
		InitChannelCache()
	}
}

func GetRandomSatisfiedChannel(
	group string,
	model string,
	retry int,
	filters []dto.ChannelFilter,
) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannel(group, model, retry, filters)
	}
	priorities, err := ListSatisfiedChannelPriorities(group, model, filters)
	if err != nil || retry >= len(priorities) {
		return nil, err
	}
	return GetRandomSatisfiedChannelAtPriority(group, model, priorities[retry], filters)
}

func cachedSatisfiedChannelIDs(group string, model string, filters []dto.ChannelFilter) []int {
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	channels, _ := filterCandidateIDs(group2model2channels[group][model], model, filters)
	if len(channels) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(model)
		channels, _ = filterCandidateIDs(group2model2channels[group][normalizedModel], model, filters)
	}
	return append([]int(nil), channels...)
}

func GetRandomSatisfiedChannelAtPriority(
	group string,
	model string,
	priority int64,
	filters []dto.ChannelFilter,
) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelAtPriority(group, model, priority, filters)
	}
	channels := cachedSatisfiedChannelIDs(group, model, filters)
	if len(channels) == 0 {
		return nil, nil
	}

	var sumWeight = 0
	var targetChannels []*Channel
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	for _, channelId := range channels {
		if channel, ok := channelsIDM[channelId]; ok {
			if channel.GetPriority() == priority {
				sumWeight += channel.GetWeight()
				targetChannels = append(targetChannels, channel)
			}
		} else {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelId)
		}
	}

	if len(targetChannels) == 0 {
		return nil, nil
	}

	// smoothing factor and adjustment
	smoothingFactor := 1
	smoothingAdjustment := 0

	if sumWeight == 0 {
		// when all channels have weight 0, set sumWeight to the number of channels and set smoothing adjustment to 100
		// each channel's effective weight = 100
		sumWeight = len(targetChannels) * 100
		smoothingAdjustment = 100
	} else if sumWeight/len(targetChannels) < 10 {
		// when the average weight is less than 10, set smoothing factor to 100
		smoothingFactor = 100
	}

	// Calculate the total weight of all channels up to endIdx
	totalWeight := sumWeight * smoothingFactor

	// Generate a random value in the range [0, totalWeight)
	randomWeight := rand.Intn(totalWeight)

	// Find a channel based on its weight
	for _, channel := range targetChannels {
		randomWeight -= channel.GetWeight()*smoothingFactor + smoothingAdjustment
		if randomWeight < 0 {
			return channel, nil
		}
	}
	// return null if no channel is not found
	return nil, errors.New("channel not found")
}

func ListSatisfiedChannelIDsAtPriority(
	group string,
	model string,
	priority int64,
	filters []dto.ChannelFilter,
) ([]int, error) {
	if !common.MemoryCacheEnabled {
		return ListChannelIDsAtPriority(group, model, priority, filters)
	}
	channels := cachedSatisfiedChannelIDs(group, model, filters)
	result := make([]int, 0, len(channels))
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	for _, channelID := range channels {
		channel, ok := channelsIDM[channelID]
		if !ok {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelID)
		}
		if channel.GetPriority() == priority {
			result = append(result, channelID)
		}
	}
	return result, nil
}

func ListSatisfiedChannelPriorities(
	group string,
	model string,
	filters []dto.ChannelFilter,
) ([]int64, error) {
	if !common.MemoryCacheEnabled {
		return ListChannelPriorities(group, model, filters)
	}
	channels := cachedSatisfiedChannelIDs(group, model, filters)
	priorities := make(map[int64]struct{})
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	for _, channelID := range channels {
		channel, ok := channelsIDM[channelID]
		if !ok {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelID)
		}
		priorities[channel.GetPriority()] = struct{}{}
	}
	result := make([]int64, 0, len(priorities))
	for priority := range priorities {
		result = append(result, priority)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] > result[j] })
	return result, nil
}

// CountSatisfiedChannelPriorities returns the number of distinct priority
// levels that can serve a model and its request constraints. A retry consumes one level,
// so channels at the same priority remain a load-balanced pool.
func CountSatisfiedChannelPriorities(group string, model string, filters []dto.ChannelFilter) (int, error) {
	priorities, err := ListSatisfiedChannelPriorities(group, model, filters)
	return len(priorities), err
}
func CacheGetChannel(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	return c, nil
}

func CacheGetChannelInfo(id int) (*ChannelInfo, error) {
	if !common.MemoryCacheEnabled {
		channel, err := GetChannelById(id, true)
		if err != nil {
			return nil, err
		}
		return &channel.ChannelInfo, nil
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	return &c.ChannelInfo, nil
}

func CacheUpdateChannelStatus(id int, status int) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	if channel, ok := channelsIDM[id]; ok {
		channel.Status = status
		syncChannelRoutingIndexLocked(channel)
	}
}

func syncChannelRoutingIndexLocked(channel *Channel) {
	for group, model2channels := range group2model2channels {
		for model, channels := range model2channels {
			filtered := channels[:0]
			for _, channelID := range channels {
				if channelID != channel.Id {
					filtered = append(filtered, channelID)
				}
			}
			group2model2channels[group][model] = filtered
		}
	}
	if channel.Status != common.ChannelStatusEnabled {
		return
	}

	if group2model2channels == nil {
		group2model2channels = make(map[string]map[string][]int)
	}
	addedRoutes := make(map[string]struct{})
	for _, group := range strings.Split(channel.Group, ",") {
		if group2model2channels[group] == nil {
			group2model2channels[group] = make(map[string][]int)
		}
		for _, model := range strings.Split(channel.Models, ",") {
			routeKey := group + "\x00" + model
			if _, exists := addedRoutes[routeKey]; exists {
				continue
			}
			addedRoutes[routeKey] = struct{}{}
			channels := append(group2model2channels[group][model], channel.Id)
			sort.SliceStable(channels, func(i, j int) bool {
				return channelsIDM[channels[i]].GetPriority() > channelsIDM[channels[j]].GetPriority()
			})
			group2model2channels[group][model] = channels
		}
	}
}

func cacheUpdateChannelLocked(channel *Channel) {
	if channelsIDM == nil {
		channelsIDM = make(map[int]*Channel)
	}
	routingChanged := true
	if oldChannel, ok := channelsIDM[channel.Id]; ok {
		logger.LogDebug(nil, "CacheUpdateChannel before: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, oldChannel.ChannelInfo.MultiKeyPollingIndex)
		routingChanged = oldChannel.Status != channel.Status ||
			oldChannel.Group != channel.Group ||
			oldChannel.Models != channel.Models ||
			oldChannel.GetPriority() != channel.GetPriority()
	}
	channelsIDM[channel.Id] = channel
	if routingChanged {
		syncChannelRoutingIndexLocked(channel)
	}
	if channel2advancedCustomConfig == nil {
		channel2advancedCustomConfig = make(map[int]*kitdto.AdvancedCustomConfig)
	}
	delete(channel2advancedCustomConfig, channel.Id)
	if channel.Type == constant.ChannelTypeAdvancedCustom {
		if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
			channel2advancedCustomConfig[channel.Id] = config
		}
	}
	logger.LogDebug(nil, "CacheUpdateChannel after: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, channel.ChannelInfo.MultiKeyPollingIndex)
}

type ChannelStatusCacheUpdate struct {
	Snapshot          *Channel
	UpdateChannelInfo bool
}

// CacheUpdateChannelStatusSnapshots publishes only status-owned fields from
// committed snapshots, preserving newer cache state outside that ownership.
func CacheUpdateChannelStatusSnapshots(updates []ChannelStatusCacheUpdate) {
	if !common.MemoryCacheEnabled {
		return
	}

	updated := false
	channelSyncLock.Lock()
	for _, update := range updates {
		if update.Snapshot == nil {
			continue
		}

		snapshot := update.Snapshot
		published := *snapshot
		cached, exists := channelsIDM[snapshot.Id]
		if exists && cached != nil {
			published = *cached
		}
		published.Status = snapshot.Status
		published.OtherInfo = snapshot.OtherInfo
		if update.UpdateChannelInfo {
			pollingIndex := published.ChannelInfo.MultiKeyPollingIndex
			published.ChannelInfo = snapshot.ChannelInfo
			if exists && cached != nil {
				published.ChannelInfo.MultiKeyPollingIndex = pollingIndex
			}
		}
		if (!exists || cached == nil) && published.ChannelInfo.IsMultiKey {
			published.Keys = published.GetKeys()
		}
		cacheUpdateChannelLocked(&published)
		updated = true
	}
	channelSyncLock.Unlock()
	if updated {
		InvalidatePricingCache()
	}
}

type ManagedChannelCacheUpdate struct {
	Snapshot            *Channel
	UpdateRoutingConfig bool
	UpdateStatusReason  bool
}

// CacheUpdateManagedChannelSnapshots publishes only reconciliation-owned
// fields while preserving newer cache state from independent writers.
func CacheUpdateManagedChannelSnapshots(updates []ManagedChannelCacheUpdate) {
	if !common.MemoryCacheEnabled {
		return
	}

	updated := false
	channelSyncLock.Lock()
	for _, update := range updates {
		if update.Snapshot == nil {
			continue
		}

		snapshot := update.Snapshot
		published := *snapshot
		if cached, exists := channelsIDM[snapshot.Id]; exists && cached != nil {
			published = *cached
		}
		published.Status = snapshot.Status
		if update.UpdateRoutingConfig {
			published.Priority = snapshot.Priority
			published.BaseURL = snapshot.BaseURL
			published.Models = snapshot.Models
		}
		if update.UpdateStatusReason {
			publishedInfo := published.GetOtherInfo()
			snapshotInfo := snapshot.GetOtherInfo()
			for _, key := range []string{"status_reason", "status_time"} {
				if value, exists := snapshotInfo[key]; exists {
					publishedInfo[key] = value
				} else {
					delete(publishedInfo, key)
				}
			}
			published.SetOtherInfo(publishedInfo)
		}
		cacheUpdateChannelLocked(&published)
		updated = true
	}
	channelSyncLock.Unlock()
	if updated {
		InvalidatePricingCache()
	}
}

func CacheUpdateChannels(channels []*Channel) {
	if !common.MemoryCacheEnabled {
		return
	}

	updated := false
	channelSyncLock.Lock()
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		cacheUpdateChannelLocked(channel)
		updated = true
	}
	// Lock ordering: do NOT hold channelSyncLock while calling
	// InvalidatePricingCache. GetPricing acquires updatePricingLock first and then
	// channelSyncLock.RLock (via loadPricingAdvancedCustomConfigs); acquiring
	// updatePricingLock while holding channelSyncLock would be an AB-BA deadlock.
	channelSyncLock.Unlock()
	if updated {
		InvalidatePricingCache()
	}
}

func CacheUpdateChannel(channel *Channel) {
	CacheUpdateChannels([]*Channel{channel})
}

func CacheDeleteChannels(channelIDs []int) {
	if !common.MemoryCacheEnabled || len(channelIDs) == 0 {
		return
	}

	deleted := make(map[int]struct{}, len(channelIDs))
	for _, channelID := range channelIDs {
		deleted[channelID] = struct{}{}
	}

	channelSyncLock.Lock()
	for channelID := range deleted {
		delete(channelsIDM, channelID)
		delete(channel2advancedCustomConfig, channelID)
	}
	for group, model2channels := range group2model2channels {
		for model, channelIDs := range model2channels {
			kept := channelIDs[:0]
			for _, channelID := range channelIDs {
				if _, remove := deleted[channelID]; !remove {
					kept = append(kept, channelID)
				}
			}
			group2model2channels[group][model] = kept
		}
	}
	channelSyncLock.Unlock()

	InvalidatePricingCache()
	rebuildTaskAliasView()
}
