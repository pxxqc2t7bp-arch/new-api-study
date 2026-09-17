package service

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const upstreamSnapshotSchemaVersion = 1

var upstreamSourceOrigins = map[string]string{
	model.UpstreamSourceKeyLeyi:    "https://leyi12.xyz",
	model.UpstreamSourceKeyHualong: "https://api.hualong.online",
	model.UpstreamSourceKeyEBond:   "https://ebondai.com",
}

var upstreamEndpointHosts = map[string][]string{
	model.UpstreamSourceKeyLeyi: {
		"leyi12.xyz",
		"leyiapi.com",
		"api.leyiapi.com",
		"wlaicc.cc",
	},
	model.UpstreamSourceKeyHualong: {
		"api.hualong.online",
		"api-fast.hualong.online",
	},
	model.UpstreamSourceKeyEBond: {
		"ebondai.com",
		"api.ebondai.com",
		"cf.ebondai.com",
	},
}

type UpstreamSnapshotIngestResult struct {
	SnapshotID string `json:"snapshot_id"`
	Sources    int    `json:"sources"`
	Groups     int    `json:"groups"`
	Metrics    int    `json:"metrics"`
}

func IngestUpstreamSnapshot(device *model.UpstreamSyncDevice, snapshot dto.UpstreamSyncSnapshot) (UpstreamSnapshotIngestResult, error) {
	result := UpstreamSnapshotIngestResult{SnapshotID: snapshot.SnapshotID}
	if device == nil || device.Status != model.UpstreamSyncDeviceActive {
		return result, ErrUpstreamDeviceUnauthorized
	}
	if snapshot.SchemaVersion != upstreamSnapshotSchemaVersion {
		return result, fmt.Errorf("unsupported upstream snapshot schema version: %d", snapshot.SchemaVersion)
	}
	if strings.TrimSpace(snapshot.SnapshotID) == "" || snapshot.DeviceID != device.DeviceID {
		return result, errors.New("invalid upstream snapshot identity")
	}
	now := common.GetTimestamp()
	if snapshot.CapturedAt <= 0 || snapshot.CapturedAt > now+300 || snapshot.CapturedAt < now-86400 {
		return result, errors.New("upstream snapshot timestamp is outside the accepted window")
	}
	if len(snapshot.Sources) == 0 || len(snapshot.Sources) > len(upstreamSourceOrigins) {
		return result, errors.New("upstream snapshot source count is invalid")
	}

	err := model.DB.Transaction(func(tx *gorm.DB) error {
		batch := model.UpstreamSyncBatch{
			SnapshotID:  snapshot.SnapshotID,
			DeviceID:    device.DeviceID,
			CapturedAt:  snapshot.CapturedAt,
			SourceCount: len(snapshot.Sources),
		}
		create := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&batch)
		if create.Error != nil {
			return create.Error
		}
		if create.RowsAffected == 0 {
			return nil
		}

		seenSources := make(map[string]struct{}, len(snapshot.Sources))
		for _, sourceSnapshot := range snapshot.Sources {
			sourceKey := strings.ToLower(strings.TrimSpace(sourceSnapshot.Key))
			expectedOrigin, ok := upstreamSourceOrigins[sourceKey]
			if !ok {
				return fmt.Errorf("unsupported upstream source: %s", sourceSnapshot.Key)
			}
			if _, duplicate := seenSources[sourceKey]; duplicate {
				return fmt.Errorf("duplicate upstream source: %s", sourceKey)
			}
			seenSources[sourceKey] = struct{}{}
			if normalizeOrigin(sourceSnapshot.ConsoleURL) != expectedOrigin {
				return fmt.Errorf("unexpected console origin for source %s", sourceKey)
			}

			endpoints, selectedEndpoint, err := normalizeUpstreamEndpoints(sourceKey, sourceSnapshot.APIBaseURL, sourceSnapshot.EndpointCandidates)
			if err != nil {
				return err
			}
			endpointJSON, err := common.Marshal(endpoints)
			if err != nil {
				return err
			}
			source := model.UpstreamSource{
				Key:                 sourceKey,
				Name:                strings.TrimSpace(sourceSnapshot.Name),
				ConsoleURL:          expectedOrigin,
				AdapterVersion:      strings.TrimSpace(sourceSnapshot.AdapterVersion),
				EndpointCandidates:  string(endpointJSON),
				SelectedEndpoint:    selectedEndpoint,
				Status:              model.NormalizeUpstreamHealth(sourceSnapshot.Status),
				Enabled:             true,
				Balance:             validNonNegativeFloat(sourceSnapshot.Balance),
				LowBalanceThreshold: 5,
				LastSnapshotID:      snapshot.SnapshotID,
				LastSnapshotAt:      snapshot.CapturedAt,
				UpdatedAt:           now,
			}
			if source.Status == model.UpstreamHealthOperational || source.Status == model.UpstreamHealthDegraded {
				source.LastSuccessAt = snapshot.CapturedAt
			}
			if sourceSnapshot.Error != "" {
				source.LastError = strings.TrimSpace(sourceSnapshot.Error)
			}
			if err := upsertUpstreamSourceTx(tx, &source); err != nil {
				return err
			}
			if err := tx.Where("key = ?", sourceKey).First(&source).Error; err != nil {
				return err
			}
			result.Sources++

			for _, groupSnapshot := range sourceSnapshot.Groups {
				if err := ingestUpstreamGroupTx(tx, &source, snapshot, groupSnapshot); err != nil {
					return err
				}
				result.Groups++
				result.Metrics++
			}
			for _, keySnapshot := range sourceSnapshot.Keys {
				if err := ingestUpstreamKeyMetricTx(tx, &source, snapshot, keySnapshot); err != nil {
					return err
				}
				result.Metrics++
			}
		}
		return nil
	})
	return result, err
}

func normalizeOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return ""
	}
	return "https://" + strings.ToLower(parsed.Hostname())
}

func normalizeUpstreamEndpoints(sourceKey string, preferred string, candidates []dto.UpstreamEndpointSnapshot) ([]dto.UpstreamEndpointSnapshot, string, error) {
	allowed := upstreamEndpointHosts[sourceKey]
	normalized := make([]dto.UpstreamEndpointSnapshot, 0, len(candidates)+1)
	seen := make(map[string]struct{}, len(candidates)+1)
	appendEndpoint := func(item dto.UpstreamEndpointSnapshot) error {
		parsed, err := url.Parse(strings.TrimSpace(item.URL))
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || !slices.Contains(allowed, strings.ToLower(parsed.Hostname())) {
			return fmt.Errorf("unexpected API endpoint for source %s", sourceKey)
		}
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawQuery = ""
		parsed.Fragment = ""
		item.URL = strings.TrimRight(parsed.String(), "/")
		if _, exists := seen[item.URL]; exists {
			return nil
		}
		seen[item.URL] = struct{}{}
		item.Name = strings.TrimSpace(item.Name)
		normalized = append(normalized, item)
		return nil
	}
	if strings.TrimSpace(preferred) != "" {
		if err := appendEndpoint(dto.UpstreamEndpointSnapshot{Name: "default", URL: preferred}); err != nil {
			return nil, "", err
		}
	}
	for _, candidate := range candidates {
		if err := appendEndpoint(candidate); err != nil {
			return nil, "", err
		}
	}
	selected := ""
	for _, candidate := range normalized {
		if candidate.Healthy != nil && !*candidate.Healthy {
			continue
		}
		if selected == "" {
			selected = candidate.URL
			continue
		}
		var selectedLatency *int64
		for _, current := range normalized {
			if current.URL == selected {
				selectedLatency = current.LatencyMS
				break
			}
		}
		if candidate.LatencyMS != nil && (selectedLatency == nil || *candidate.LatencyMS < *selectedLatency) {
			selected = candidate.URL
		}
	}
	return normalized, selected, nil
}

func upsertUpstreamSourceTx(tx *gorm.DB, source *model.UpstreamSource) error {
	var existing model.UpstreamSource
	err := tx.Where("key = ?", source.Key).First(&existing).Error
	if err == nil {
		source.ID = existing.ID
		source.CreatedAt = existing.CreatedAt
		source.LowBalanceThreshold = existing.LowBalanceThreshold
		updates := map[string]any{
			"name":             source.Name,
			"console_url":      source.ConsoleURL,
			"adapter_version":  source.AdapterVersion,
			"status":           source.Status,
			"last_snapshot_id": source.LastSnapshotID,
			"last_snapshot_at": source.LastSnapshotAt,
			"last_error":       source.LastError,
			"updated_at":       source.UpdatedAt,
		}
		if source.EndpointCandidates != "[]" {
			updates["endpoint_candidates"] = source.EndpointCandidates
		}
		if source.SelectedEndpoint != "" {
			updates["selected_endpoint"] = source.SelectedEndpoint
		}
		if source.Balance != nil {
			updates["balance"] = source.Balance
		}
		if source.LastSuccessAt > 0 {
			updates["last_success_at"] = source.LastSuccessAt
		}
		return tx.Model(&existing).Updates(updates).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return tx.Create(source).Error
}

func ingestUpstreamGroupTx(tx *gorm.DB, source *model.UpstreamSource, snapshot dto.UpstreamSyncSnapshot, input dto.UpstreamGroupSnapshot) error {
	if strings.TrimSpace(input.ExternalID) == "" || strings.TrimSpace(input.Name) == "" {
		return errors.New("upstream group identity is required")
	}
	platform := strings.ToLower(strings.TrimSpace(input.Platform))
	if platform != "openai" && platform != "anthropic" && platform != "grok" {
		return fmt.Errorf("unsupported upstream group platform: %s", input.Platform)
	}
	if !validMultiplier(input.RateMultiplier) || (input.UserRateMultiplier != nil && !validMultiplier(*input.UserRateMultiplier)) {
		return errors.New("invalid upstream group multiplier")
	}
	effectiveMultiplier := input.RateMultiplier
	if input.UserRateMultiplier != nil {
		effectiveMultiplier = *input.UserRateMultiplier
	}
	modelNames := make([]string, 0, len(input.Models))
	for _, upstreamModel := range input.Models {
		name := strings.TrimSpace(upstreamModel.Name)
		if name != "" {
			modelNames = append(modelNames, name)
		}
	}
	modelNames = uniqueSortedStrings(modelNames)
	modelJSON, err := common.Marshal(modelNames)
	if err != nil {
		return err
	}

	var existing model.UpstreamGroup
	findErr := tx.Where("source_id = ? AND external_id = ?", source.ID, input.ExternalID).First(&existing).Error
	redSince := existing.RedSince
	lastGreenAt := existing.LastGreenAt
	health := model.NormalizeUpstreamHealth(input.HealthStatus)
	switch health {
	case model.UpstreamHealthOperational, model.UpstreamHealthDegraded:
		redSince = 0
		lastGreenAt = snapshot.CapturedAt
	case model.UpstreamHealthFailed, model.UpstreamHealthError:
		if redSince == 0 {
			redSince = snapshot.CapturedAt
		}
	}
	group := model.UpstreamGroup{
		SourceID:            source.ID,
		ExternalID:          strings.TrimSpace(input.ExternalID),
		Name:                strings.TrimSpace(input.Name),
		Platform:            platform,
		SubscriptionType:    strings.TrimSpace(input.SubscriptionType),
		BaseMultiplier:      input.RateMultiplier,
		UserMultiplier:      input.UserRateMultiplier,
		EffectiveMultiplier: effectiveMultiplier,
		PeakRateEnabled:     input.PeakRateEnabled,
		PeakStart:           strings.TrimSpace(input.PeakStart),
		PeakEnd:             strings.TrimSpace(input.PeakEnd),
		PeakMultiplier:      validNonNegativeFloat(input.PeakMultiplier),
		IsExclusive:         input.IsExclusive,
		HealthStatus:        health,
		Availability:        validPercent(input.Availability),
		LatencyMS:           validNonNegativeInt64(input.LatencyMS),
		EndpointPingMS:      validNonNegativeInt64(input.EndpointPingMS),
		Models:              string(modelJSON),
		MonitorExternalID:   strings.TrimSpace(input.MonitorExternalID),
		RedSince:            redSince,
		LastGreenAt:         lastGreenAt,
		ObservedAt:          snapshot.CapturedAt,
		UpdatedAt:           common.GetTimestamp(),
	}
	if findErr == nil {
		group.ID = existing.ID
		group.CreatedAt = existing.CreatedAt
	} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return findErr
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "source_id"}, {Name: "external_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name", "platform", "subscription_type", "base_multiplier",
			"user_multiplier", "effective_multiplier", "peak_rate_enabled",
			"peak_start", "peak_end", "peak_multiplier", "is_exclusive",
			"health_status", "availability", "latency_ms", "endpoint_ping_ms",
			"models", "monitor_external_id", "red_since", "last_green_at",
			"observed_at", "updated_at",
		}),
	}).Create(&group).Error; err != nil {
		return err
	}

	metric := model.UpstreamMetricSnapshot{
		SnapshotID:      snapshot.SnapshotID,
		SourceID:        source.ID,
		ExternalGroupID: group.ExternalID,
		HealthStatus:    group.HealthStatus,
		RateMultiplier:  &group.EffectiveMultiplier,
		Balance:         source.Balance,
		Availability:    group.Availability,
		LatencyMS:       group.LatencyMS,
		EndpointPingMS:  group.EndpointPingMS,
		DataQuality:     model.UpstreamMetricQualityReported,
		ObservedAt:      snapshot.CapturedAt,
	}
	return tx.Create(&metric).Error
}

func ingestUpstreamKeyMetricTx(tx *gorm.DB, source *model.UpstreamSource, snapshot dto.UpstreamSyncSnapshot, input dto.UpstreamKeySnapshot) error {
	if strings.TrimSpace(input.ExternalID) == "" || strings.TrimSpace(input.Fingerprint) == "" {
		return errors.New("upstream key snapshot identity is required")
	}
	quality := input.DataQuality
	if quality != model.UpstreamMetricQualityReported && quality != model.UpstreamMetricQualityEstimated {
		quality = model.UpstreamMetricQualityUnavailable
	}
	metric := model.UpstreamMetricSnapshot{
		SnapshotID:      snapshot.SnapshotID,
		SourceID:        source.ID,
		ExternalGroupID: strings.TrimSpace(input.GroupExternalID),
		Balance:         source.Balance,
		InputTokens:     validNonNegativeInt64(input.InputTokens),
		OutputTokens:    validNonNegativeInt64(input.OutputTokens),
		CachedTokens:    validNonNegativeInt64(input.CachedTokens),
		TotalTokens:     validNonNegativeInt64(input.TotalTokens),
		Usage5H:         validNonNegativeFloat(input.Usage5H),
		Limit5H:         validNonNegativeFloat(input.Limit5H),
		Reset5HAt:       validNonNegativeInt64(input.Reset5HAt),
		Usage1D:         validNonNegativeFloat(input.Usage1D),
		Limit1D:         validNonNegativeFloat(input.Limit1D),
		Reset1DAt:       validNonNegativeInt64(input.Reset1DAt),
		Usage7D:         validNonNegativeFloat(input.Usage7D),
		Limit7D:         validNonNegativeFloat(input.Limit7D),
		Reset7DAt:       validNonNegativeInt64(input.Reset7DAt),
		Usage30D:        validNonNegativeFloat(input.Usage30D),
		Limit30D:        validNonNegativeFloat(input.Limit30D),
		Reset30DAt:      validNonNegativeInt64(input.Reset30DAt),
		DataQuality:     quality,
		ObservedAt:      snapshot.CapturedAt,
	}
	if err := tx.Create(&metric).Error; err != nil {
		return err
	}
	if len(input.Fingerprint) == 64 {
		return tx.Model(&model.UpstreamManagedRoute{}).
			Where("source_id = ? AND key_fingerprint = ?", source.ID, strings.ToLower(input.Fingerprint)).
			Updates(map[string]any{
				"external_key_id": input.ExternalID,
				"updated_at":      common.GetTimestamp(),
			}).Error
	}
	return nil
}

func validMultiplier(value float64) bool {
	return value > 0 && value <= 100 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validNonNegativeFloat(value *float64) *float64 {
	if value == nil || *value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}

func validPercent(value *float64) *float64 {
	if value == nil || *value < 0 || *value > 100 || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil
	}
	return value
}

func validNonNegativeInt64(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	return value
}

func uniqueSortedStrings(values []string) []string {
	slices.Sort(values)
	return slices.Compact(values)
}
