package model

import (
	"errors"
	"reflect"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	UpstreamProtocolOpenAI    = "openai"
	UpstreamProtocolAnthropic = "anthropic"

	UpstreamRouteStateShadow      = "shadow"
	UpstreamRouteStateActive      = "active"
	UpstreamRouteStateQuarantined = "quarantined"
	UpstreamRouteStateLongRed     = "long_red"
	UpstreamRouteStatePaused      = "manual_pause"
	UpstreamRouteStateDetached    = "detached"
	UpstreamRouteStateRetained    = "retained"
)

var errStaleManagedRouteIsolation = errors.New("managed route isolation snapshot changed")

type UpstreamManagedRoute struct {
	ID                   int64   `json:"id" gorm:"primaryKey"`
	SourceID             int64   `json:"source_id" gorm:"not null;uniqueIndex:idx_upstream_route_identity,priority:1;index"`
	ExternalGroupID      string  `json:"external_group_id" gorm:"type:varchar(128);not null;uniqueIndex:idx_upstream_route_identity,priority:2"`
	Platform             string  `json:"platform" gorm:"type:varchar(32);not null;uniqueIndex:idx_upstream_route_identity,priority:3;index"`
	Protocol             string  `json:"protocol" gorm:"type:varchar(32);not null;uniqueIndex:idx_upstream_route_identity,priority:4;index"`
	ChannelID            int     `json:"channel_id" gorm:"not null;uniqueIndex"`
	ExternalKeyID        string  `json:"external_key_id,omitempty" gorm:"type:varchar(128)"`
	KeyFingerprint       string  `json:"key_fingerprint,omitempty" gorm:"type:char(64);index"`
	State                string  `json:"state" gorm:"type:varchar(32);not null;index"`
	Rank                 int     `json:"rank" gorm:"not null"`
	EffectiveMultiplier  float64 `json:"effective_multiplier" gorm:"not null;index"`
	ConsecutiveFailures  int     `json:"consecutive_failures" gorm:"not null"`
	ConsecutiveSuccesses int     `json:"consecutive_successes" gorm:"not null"`
	FailureWindowStart   int64   `json:"failure_window_start,omitempty" gorm:"bigint"`
	LastFailureAt        int64   `json:"last_failure_at,omitempty" gorm:"bigint"`
	LastSuccessAt        int64   `json:"last_success_at,omitempty" gorm:"bigint;index"`
	LastProbeAt          int64   `json:"last_probe_at,omitempty" gorm:"bigint"`
	LastLatencyMS        int64   `json:"last_latency_ms,omitempty" gorm:"bigint"`
	RecoveryAttempts     int     `json:"recovery_attempts" gorm:"not null"`
	NextProbeAt          int64   `json:"next_probe_at,omitempty" gorm:"bigint;index"`
	RedSince             int64   `json:"red_since,omitempty" gorm:"bigint;index"`
	ManualPauseUntil     int64   `json:"manual_pause_until,omitempty" gorm:"bigint;index"`
	Detached             bool    `json:"detached" gorm:"not null;index"`
	LastReason           string  `json:"last_reason,omitempty" gorm:"type:text"`
	CreatedAt            int64   `json:"created_at" gorm:"bigint;index"`
	UpdatedAt            int64   `json:"updated_at" gorm:"bigint;index"`
}

func (route *UpstreamManagedRoute) BeforeCreate(_ *gorm.DB) error {
	now := common.GetTimestamp()
	route.ExternalGroupID = strings.TrimSpace(route.ExternalGroupID)
	route.Platform = strings.ToLower(strings.TrimSpace(route.Platform))
	route.Protocol = strings.ToLower(strings.TrimSpace(route.Protocol))
	if route.State == "" {
		route.State = UpstreamRouteStateShadow
	}
	if route.CreatedAt == 0 {
		route.CreatedAt = now
	}
	if route.UpdatedAt == 0 {
		route.UpdatedAt = now
	}
	return nil
}

func UpsertUpstreamManagedRoute(route *UpstreamManagedRoute) error {
	if route == nil {
		return nil
	}
	route.UpdatedAt = common.GetTimestamp()
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "source_id"},
			{Name: "external_group_id"},
			{Name: "platform"},
			{Name: "protocol"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"channel_id",
			"external_key_id",
			"key_fingerprint",
			"state",
			"rank",
			"effective_multiplier",
			"consecutive_failures",
			"consecutive_successes",
			"failure_window_start",
			"last_failure_at",
			"last_success_at",
			"last_probe_at",
			"last_latency_ms",
			"recovery_attempts",
			"next_probe_at",
			"red_since",
			"manual_pause_until",
			"detached",
			"last_reason",
			"updated_at",
		}),
	}).Create(route).Error
}

func GetUpstreamManagedRouteByChannelID(channelID int) (*UpstreamManagedRoute, error) {
	var route UpstreamManagedRoute
	if err := DB.Where("channel_id = ?", channelID).First(&route).Error; err != nil {
		return nil, err
	}
	return &route, nil
}

func ListUpstreamManagedRoutes() ([]UpstreamManagedRoute, error) {
	var routes []UpstreamManagedRoute
	err := DB.Order("rank asc, id asc").Find(&routes).Error
	return routes, err
}

// UpdateManagedRouteProbeResultIfUnchanged applies a probe result only while
// the complete route snapshot observed before the write is still current.
func UpdateManagedRouteProbeResultIfUnchanged(
	expected *UpstreamManagedRoute,
	desired *UpstreamManagedRoute,
	disableChannel bool,
	statusReason string,
	statusTime int64,
) (bool, bool, error) {
	if expected == nil ||
		expected.ID == 0 ||
		desired == nil ||
		desired.ID != expected.ID ||
		desired.SourceID != expected.SourceID ||
		desired.ExternalGroupID != expected.ExternalGroupID ||
		desired.Platform != expected.Platform ||
		desired.Protocol != expected.Protocol ||
		desired.ChannelID != expected.ChannelID {
		return false, false, errors.New("managed route probe snapshot is missing or inconsistent")
	}

	update := func() (bool, bool, error) {
		applied := false
		channelDisabled := false
		var updatedChannel Channel
		err := DB.Transaction(func(tx *gorm.DB) error {
			var current UpstreamManagedRoute
			err := lockForUpdate(tx).Where("id = ?", expected.ID).First(&current).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, *expected) {
				return nil
			}

			if err := tx.Model(&UpstreamManagedRoute{}).
				Where("id = ?", current.ID).
				Updates(map[string]any{
					"state":                 desired.State,
					"consecutive_failures":  desired.ConsecutiveFailures,
					"consecutive_successes": desired.ConsecutiveSuccesses,
					"failure_window_start":  desired.FailureWindowStart,
					"last_failure_at":       desired.LastFailureAt,
					"last_success_at":       desired.LastSuccessAt,
					"last_probe_at":         desired.LastProbeAt,
					"last_latency_ms":       desired.LastLatencyMS,
					"recovery_attempts":     desired.RecoveryAttempts,
					"next_probe_at":         desired.NextProbeAt,
					"last_reason":           desired.LastReason,
					"updated_at":            desired.UpdatedAt,
				}).Error; err != nil {
				return err
			}

			if disableChannel {
				var currentChannel Channel
				if err := lockForUpdate(tx).
					Where("id = ?", current.ChannelID).
					First(&currentChannel).Error; err != nil {
					return err
				}
				updatedChannel = currentChannel
				if updatedChannel.Status == common.ChannelStatusEnabled {
					updatedChannel.Status = common.ChannelStatusAutoDisabled
					channelDisabled = true
				}
				info := updatedChannel.GetOtherInfo()
				info["status_reason"] = statusReason
				info["status_time"] = statusTime
				updatedChannel.SetOtherInfo(info)
				if err := tx.Model(&Channel{}).
					Where("id = ?", updatedChannel.Id).
					Updates(map[string]any{
						"status":     updatedChannel.Status,
						"other_info": updatedChannel.OtherInfo,
					}).Error; err != nil {
					return err
				}
				if err := tx.Model(&Ability{}).
					Where("channel_id = ?", updatedChannel.Id).
					Select("enabled").
					Update("enabled", false).Error; err != nil {
					return err
				}
			}

			applied = true
			return nil
		})
		if err != nil {
			return false, false, err
		}
		if applied && disableChannel {
			CacheUpdateManagedChannelSnapshots([]ManagedChannelCacheUpdate{{
				Snapshot:           &updatedChannel,
				UpdateStatusReason: true,
			}})
		}
		return applied, channelDisabled, nil
	}
	if disableChannel {
		channelDisabled := false
		applied, err := withChannelStatusLocks(expected.ChannelID, func() (bool, error) {
			var applied bool
			var err error
			applied, channelDisabled, err = update()
			return applied, err
		})
		return applied, channelDisabled, err
	}
	return update()
}

// IsolateManagedRouteModel removes one model from an attached managed route
// while locking the managed decision rows in canonical order.
func IsolateManagedRouteModel(
	expectedRoute *UpstreamManagedRoute,
	modelName string,
	reason string,
	now int64,
	exclusionOptionKey string,
) (bool, string, error) {
	modelName = strings.TrimSpace(modelName)
	exclusionOptionKey = strings.TrimSpace(exclusionOptionKey)
	if expectedRoute == nil ||
		expectedRoute.ID == 0 ||
		expectedRoute.SourceID == 0 ||
		expectedRoute.ChannelID == 0 ||
		modelName == "" ||
		exclusionOptionKey == "" {
		return false, "", nil
	}
	if err := validateOptionValue(exclusionOptionKey, "{}"); err != nil {
		return false, "", err
	}

	// Lock order: option protocol, channel status locks, then database rows in
	// option/source/group/route/channel order. Do not call an exported option
	// writer while this mutex is held.
	optionPersistencePublishMutex.Lock()
	defer optionPersistencePublishMutex.Unlock()

	optionValue := ""
	isolated, err := withChannelStatusLocks(expectedRoute.ChannelID, func() (bool, error) {
		isolated := false
		err := DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoNothing: true,
			}).Create(&Option{Key: exclusionOptionKey, Value: "{}"}).Error; err != nil {
				return err
			}
			var option Option
			if err := lockForUpdate(tx).
				Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: exclusionOptionKey}).
				First(&option).Error; err != nil {
				return err
			}

			var source UpstreamSource
			if err := lockForUpdate(tx).
				Where("id = ?", expectedRoute.SourceID).
				First(&source).Error; err != nil {
				return err
			}

			var group UpstreamGroup
			if err := lockForUpdate(tx).
				Where("source_id = ? AND external_id = ?", source.ID, expectedRoute.ExternalGroupID).
				First(&group).Error; err != nil {
				return err
			}

			var route UpstreamManagedRoute
			if err := lockForUpdate(tx).
				Where("id = ?", expectedRoute.ID).
				First(&route).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return errStaleManagedRouteIsolation
				}
				return err
			}
			if route.SourceID != source.ID ||
				route.ExternalGroupID != group.ExternalID ||
				route.Platform != group.Platform ||
				route.Detached ||
				!reflect.DeepEqual(route, *expectedRoute) {
				return errStaleManagedRouteIsolation
			}

			var channel Channel
			if err := lockForUpdate(tx).
				Where("id = ?", route.ChannelID).
				First(&channel).Error; err != nil {
				return err
			}

			channelModels, channelRemoved := removeManagedRouteModel(channel.Models, modelName)
			var groupModels []string
			if err := common.UnmarshalJsonStr(group.Models, &groupModels); err != nil {
				return err
			}
			groupModels, groupRemoved := removeManagedRouteModel(strings.Join(groupModels, ","), modelName)

			exclusions := make(map[string][]string)
			if strings.TrimSpace(option.Value) != "" {
				if err := common.UnmarshalJsonStr(option.Value, &exclusions); err != nil {
					return err
				}
			}
			exclusionKey := strings.ToLower(strings.TrimSpace(source.Key)) + ":" +
				strings.TrimSpace(route.ExternalGroupID)
			exclusionAdded := true
			for _, excluded := range exclusions[exclusionKey] {
				if strings.TrimSpace(excluded) == modelName {
					exclusionAdded = false
					break
				}
			}
			if !exclusionAdded && !channelRemoved && !groupRemoved {
				optionValue = option.Value
				isolated = true
				return nil
			}

			if channelRemoved {
				channel.Models = strings.Join(channelModels, ",")
				if err := tx.Model(&Channel{}).
					Where("id = ?", channel.Id).
					Update("models", channel.Models).Error; err != nil {
					return err
				}
				if err := channel.UpdateAbilities(tx); err != nil {
					return err
				}
			}

			if groupRemoved {
				encodedModels, err := common.Marshal(groupModels)
				if err != nil {
					return err
				}
				if err := tx.Model(&UpstreamGroup{}).
					Where("id = ?", group.ID).
					Updates(map[string]any{
						"models":     string(encodedModels),
						"updated_at": now,
					}).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&UpstreamManagedRoute{}).
				Where("id = ?", route.ID).
				Updates(map[string]any{
					"last_failure_at": now,
					"last_reason":     strings.TrimSpace(reason),
					"updated_at":      now,
				}).Error; err != nil {
				return err
			}

			if exclusionAdded {
				exclusions[exclusionKey] = append(exclusions[exclusionKey], modelName)
				encoded, err := common.Marshal(exclusions)
				if err != nil {
					return err
				}
				option.Value = string(encoded)
				if err := tx.Model(&Option{}).
					Where(clause.Eq{Column: clause.Column{Name: "key"}, Value: exclusionOptionKey}).
					Update("value", option.Value).Error; err != nil {
					return err
				}
			}
			optionValue = option.Value
			isolated = true
			return nil
		})
		return isolated, err
	})
	if errors.Is(err, errStaleManagedRouteIsolation) {
		return false, "", nil
	}
	return isolated, optionValue, err
}

func removeManagedRouteModel(models string, target string) ([]string, bool) {
	items := strings.Split(models, ",")
	result := make([]string, 0, len(items))
	removed := false
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == target {
			removed = true
			continue
		}
		result = append(result, item)
	}
	return result, removed
}

func RecordUpstreamRouteFailure(channelID int, now int64, windowSeconds int64, threshold int, reason string) (*UpstreamManagedRoute, bool, error) {
	var route UpstreamManagedRoute
	quarantine := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("channel_id = ? AND detached = ?", channelID, false).First(&route).Error; err != nil {
			return err
		}
		if route.FailureWindowStart == 0 || now-route.FailureWindowStart > windowSeconds {
			route.FailureWindowStart = now
			route.ConsecutiveFailures = 1
		} else {
			route.ConsecutiveFailures++
		}
		route.LastFailureAt = now
		route.LastReason = strings.TrimSpace(reason)
		if route.ConsecutiveFailures >= threshold {
			route.State = UpstreamRouteStateQuarantined
			route.RecoveryAttempts = 0
			route.NextProbeAt = now
			quarantine = true
		}
		route.UpdatedAt = now
		return tx.Model(&UpstreamManagedRoute{}).Where("id = ?", route.ID).Updates(map[string]any{
			"state":                 route.State,
			"consecutive_failures":  route.ConsecutiveFailures,
			"consecutive_successes": 0,
			"failure_window_start":  route.FailureWindowStart,
			"last_failure_at":       route.LastFailureAt,
			"last_reason":           route.LastReason,
			"recovery_attempts":     route.RecoveryAttempts,
			"next_probe_at":         route.NextProbeAt,
			"updated_at":            route.UpdatedAt,
		}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	return &route, quarantine, err
}

func RecordUpstreamRouteSuccess(channelID int, now int64, latencyMS int64) (*UpstreamManagedRoute, error) {
	var route UpstreamManagedRoute
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("channel_id = ? AND detached = ?", channelID, false).First(&route).Error; err != nil {
			return err
		}
		route.ConsecutiveFailures = 0
		route.ConsecutiveSuccesses++
		route.FailureWindowStart = 0
		route.LastSuccessAt = now
		route.LastLatencyMS = latencyMS
		route.LastReason = ""
		route.UpdatedAt = now
		return tx.Model(&UpstreamManagedRoute{}).Where("id = ?", route.ID).Updates(map[string]any{
			"consecutive_failures":  0,
			"consecutive_successes": route.ConsecutiveSuccesses,
			"failure_window_start":  int64(0),
			"last_success_at":       now,
			"last_latency_ms":       latencyMS,
			"last_reason":           "",
			"updated_at":            now,
		}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &route, err
}
