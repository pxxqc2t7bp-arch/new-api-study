package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

func planQuotaAuthorityDB(tx *gorm.DB) *gorm.DB {
	return tx.Session(&gorm.Session{
		Logger: tx.Logger.LogMode(gormlogger.Silent),
	})
}

const (
	PlanQuotaDomainStateActive   = "active"
	PlanQuotaDomainStateDisabled = "disabled"
)

type PlanQuotaDomain struct {
	CredentialHash string `gorm:"type:char(64);primaryKey"`
	Generation     int64  `gorm:"bigint;not null;default:0"`
	State          string `gorm:"type:varchar(16);not null"`
	DisabledUntil  int64  `gorm:"bigint;not null;default:0"`
}

func PlanQuotaDomainHash(credential string) (string, bool) {
	if credential == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:]), true
}

func planQuotaDomainCredential(channel *Channel) (string, bool) {
	if channel == nil ||
		channel.ChannelInfo.IsMultiKey ||
		!strings.HasPrefix(channel.GetTag(), "plan:") {
		return "", false
	}
	keys := channel.GetKeys()
	if len(keys) != 1 || keys[0] == "" {
		return "", false
	}
	return keys[0], true
}

func PlanQuotaDomainMembership(channel *Channel) (string, bool) {
	credential, ok := planQuotaDomainCredential(channel)
	if !ok {
		return "", false
	}
	return PlanQuotaDomainHash(credential)
}

type planQuotaDomainBackfillState struct {
	disabled                   bool
	generation                 int64
	disabledUntil              int64
	normalizableChannelIndexes []int
	markerGenerationMode       int
}

type planQuotaBackfillOwnershipState struct {
	owned         bool
	generation    int64
	disabledUntil int64
	marker        bool
	hasGeneration bool
}

var (
	errPlanQuotaDomainInvariant       = errors.New("plan quota domain authority invariant failed")
	errPlanQuotaDomainSnapshotChanged = errors.New("plan quota domain channel snapshot changed")
)

type planQuotaCredentialLock struct {
	credential  string
	hash        string
	allowCreate bool
}

type PlanQuotaDomainDisableRequest struct {
	FailingChannelID   int
	ObservedCredential string
	ObservedTag        string
	Reason             string
	ResetAt            int64
	Generation         int64
}

type PlanQuotaDomainRecoveryRequest struct {
	Source     *Channel
	RecoveryAt int64
	RequireDue bool
}

type PlanQuotaDomainTransitionResult struct {
	Channels      []*Channel
	NewlyDisabled int
	NewlyEnabled  int
	Recovered     bool
}

func InitializePlanQuotaDomains() error {
	if DB == nil {
		return errors.New("plan quota domain initialization requires a database")
	}

	var activeCount int
	var disabledCount int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var discovered []Channel
		if err := tx.
			Select("key", "tag", "channel_info").
			Where("tag LIKE ?", "plan:%").
			Find(&discovered).Error; err != nil {
			return err
		}

		candidateHashes := make(map[string]struct{})
		for index := range discovered {
			credential, member := planQuotaDomainCredential(&discovered[index])
			if !member {
				continue
			}
			hash, _ := PlanQuotaDomainHash(credential)
			candidateHashes[hash] = struct{}{}
		}

		hashes := make([]string, 0, len(candidateHashes))
		for hash := range candidateHashes {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		domains := make(map[string]*PlanQuotaDomain, len(hashes))
		for _, hash := range hashes {
			var domain PlanQuotaDomain
			authorityDB := planQuotaAuthorityDB(tx)
			query := lockForUpdate(authorityDB).
				Where("credential_hash = ?", hash).
				Limit(1).
				Find(&domain)
			queryErr := query.Error
			if queryErr == nil && query.RowsAffected == 0 {
				if createErr := authorityDB.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "credential_hash"}},
					DoNothing: true,
				}).Create(&PlanQuotaDomain{
					CredentialHash: hash,
					State:          PlanQuotaDomainStateActive,
				}).Error; createErr != nil {
					return createErr
				}
				query = lockForUpdate(authorityDB).
					Where("credential_hash = ?", hash).
					Limit(1).
					Find(&domain)
				queryErr = query.Error
			}
			switch {
			case queryErr != nil:
				return queryErr
			case query.RowsAffected != 1:
				return errPlanQuotaDomainInvariant
			case domain.State != PlanQuotaDomainStateActive &&
				domain.State != PlanQuotaDomainStateDisabled:
				return errors.New("plan quota domain has an invalid authority state")
			}
			domains[hash] = &domain
		}

		var channels []Channel
		if err := lockForUpdate(tx).
			Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}}).
			Find(&channels).Error; err != nil {
			return err
		}

		backfill := make(map[string]planQuotaDomainBackfillState, len(domains))
		for index := range channels {
			hash, member := PlanQuotaDomainMembership(&channels[index])
			ownershipCleared, err := clearStalePlanQuotaDomainOwnership(
				&channels[index],
				hash,
				member,
			)
			if err != nil {
				return err
			}
			if ownershipCleared {
				if err := tx.Model(&Channel{}).
					Where("id = ?", channels[index].Id).
					Update("other_info", channels[index].OtherInfo).Error; err != nil {
					return err
				}
			}
			if !member {
				continue
			}
			if _, exists := domains[hash]; !exists {
				return errPlanQuotaDomainSnapshotChanged
			}
			state := backfill[hash]
			ownership, err := planQuotaBackfillOwnership(&channels[index], hash)
			if err != nil {
				return err
			}
			if ownership.owned {
				state.disabled = true
				if ownership.generation > state.generation {
					state.generation = ownership.generation
				}
				if ownership.disabledUntil > state.disabledUntil {
					state.disabledUntil = ownership.disabledUntil
				}
				if ownership.marker {
					mode := 1
					if ownership.hasGeneration {
						mode = 2
					}
					if state.markerGenerationMode != 0 &&
						state.markerGenerationMode != mode {
						return errors.New("plan quota domain has mixed generation metadata")
					}
					state.markerGenerationMode = mode
				}
			}
			if channels[index].Status == common.ChannelStatusEnabled ||
				(channels[index].Status == common.ChannelStatusAutoDisabled &&
					(ownership.owned || ownershipCleared)) {
				state.normalizableChannelIndexes = append(
					state.normalizableChannelIndexes,
					index,
				)
			}
			backfill[hash] = state
		}

		for _, hash := range hashes {
			state := backfill[hash]
			domain := domains[hash]
			if domain.Generation > state.generation {
				state.generation = domain.Generation
			}
			if domain.State == PlanQuotaDomainStateDisabled {
				state.disabled = true
				if domain.DisabledUntil > state.disabledUntil {
					state.disabledUntil = domain.DisabledUntil
				}
			}
			domain.Generation = state.generation
			if state.disabled {
				domain.State = PlanQuotaDomainStateDisabled
				domain.DisabledUntil = state.disabledUntil
				disabledCount++
			} else {
				domain.State = PlanQuotaDomainStateActive
				domain.DisabledUntil = 0
				activeCount++
			}

			if err := planQuotaAuthorityDB(tx).Model(&PlanQuotaDomain{}).
				Where("credential_hash = ?", hash).
				Updates(map[string]any{
					"generation":     domain.Generation,
					"state":          domain.State,
					"disabled_until": domain.DisabledUntil,
				}).Error; err != nil {
				return err
			}
			if !state.disabled {
				continue
			}
			for _, channelIndex := range state.normalizableChannelIndexes {
				channel := &channels[channelIndex]
				channel.Status = common.ChannelStatusAutoDisabled
				info := channel.GetOtherInfo()
				info["quota_domain_id"] = hash
				info["quota_generation"] = strconv.FormatInt(domain.Generation, 10)
				info["quota_type"] = "plan"
				if domain.DisabledUntil > 0 {
					info["disabled_until"] = domain.DisabledUntil
				} else {
					delete(info, "disabled_until")
					delete(info, "quota_reset_at")
				}
				channel.SetOtherInfo(info)
				if err := tx.Model(&Channel{}).
					Where("id = ?", channel.Id).
					Updates(map[string]any{
						"status":     channel.Status,
						"other_info": channel.OtherInfo,
					}).Error; err != nil {
					return err
				}
				if err := tx.Model(&Ability{}).
					Where("channel_id = ?", channel.Id).
					Select("enabled").
					Update("enabled", false).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	common.SysLog(fmt.Sprintf(
		"initialized Plan quota domains: active=%d disabled=%d",
		activeCount,
		disabledCount,
	))
	return nil
}

func planQuotaBackfillOwnership(
	channel *Channel,
	expectedHash string,
) (planQuotaBackfillOwnershipState, error) {
	var ownership planQuotaBackfillOwnershipState
	info := channel.GetOtherInfo()
	rawMarker, hasMarker := info["quota_domain_id"]
	rawType, hasType := info["quota_type"]
	rawLegacyDomain, hasLegacyDomain := info["quota_domain"]
	rawGeneration, hasGeneration := info["quota_generation"]
	rawDeadline, hasDeadline := info["disabled_until"]
	hasOwnershipMetadata := hasMarker || hasType || hasLegacyDomain || hasGeneration
	if !hasOwnershipMetadata {
		return ownership, nil
	}

	quotaType, typeValid := rawType.(string)
	if !typeValid || quotaType != "plan" {
		return ownership, errors.New("plan quota domain has malformed ownership metadata")
	}

	ownership.hasGeneration = hasGeneration
	if hasGeneration {
		value, ok := rawGeneration.(string)
		if !ok || value == "" {
			return ownership, errors.New("plan quota domain has malformed generation metadata")
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return ownership, errors.New("plan quota domain has malformed generation metadata")
		}
		ownership.generation = parsed
	}
	if hasDeadline {
		var valid bool
		ownership.disabledUntil, valid = planQuotaMetadataInt64(rawDeadline)
		if !valid || ownership.disabledUntil < 0 {
			return ownership, errors.New("plan quota domain has malformed deadline metadata")
		}
	}

	if hasMarker {
		ownership.marker = true
		marker, ok := rawMarker.(string)
		if !ok || marker != expectedHash {
			return ownership, errors.New("plan quota domain has conflicting ownership metadata")
		}
		if hasLegacyDomain {
			legacyDomain, valid := rawLegacyDomain.(string)
			if !valid || legacyDomain != channel.GetTag() {
				return ownership, errors.New("plan quota domain has conflicting ownership metadata")
			}
		}
		ownership.owned = true
		return ownership, nil
	}

	legacyDomain, legacyValid := rawLegacyDomain.(string)
	if channel.Status != common.ChannelStatusAutoDisabled ||
		!legacyValid ||
		legacyDomain == "" ||
		legacyDomain != channel.GetTag() {
		return ownership, errors.New("plan quota domain has malformed legacy ownership metadata")
	}
	ownership.owned = true
	return ownership, nil
}

func planQuotaMetadataInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if typed < math.MinInt64 || typed > math.MaxInt64 || math.Trunc(typed) != typed {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

func planQuotaCredentialForChannel(channel *Channel, allowCreate bool) (planQuotaCredentialLock, bool) {
	credential, selected := planQuotaDomainCredential(channel)
	if !selected {
		return planQuotaCredentialLock{}, false
	}
	hash, ok := PlanQuotaDomainMembership(channel)
	if !ok {
		return planQuotaCredentialLock{}, false
	}
	return planQuotaCredentialLock{
		credential:  credential,
		hash:        hash,
		allowCreate: allowCreate,
	}, true
}

func lockPlanQuotaDomains(
	tx *gorm.DB,
	requested []planQuotaCredentialLock,
) (map[string]*PlanQuotaDomain, error) {
	byHash := make(map[string]planQuotaCredentialLock, len(requested))
	for _, item := range requested {
		if item.hash == "" {
			continue
		}
		existing, found := byHash[item.hash]
		if found {
			existing.allowCreate = existing.allowCreate && item.allowCreate
			if existing.credential == "" {
				existing.credential = item.credential
			}
			byHash[item.hash] = existing
			continue
		}
		byHash[item.hash] = item
	}
	hashes := make([]string, 0, len(byHash))
	for hash := range byHash {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)

	locked := make(map[string]*PlanQuotaDomain, len(hashes))
	for _, hash := range hashes {
		request := byHash[hash]
		var domain PlanQuotaDomain
		authorityDB := planQuotaAuthorityDB(tx)
		query := lockForUpdate(authorityDB).
			Where("credential_hash = ?", hash).
			Limit(1).
			Find(&domain)
		err := query.Error
		if err == nil && query.RowsAffected == 0 {
			if !request.allowCreate {
				return nil, errPlanQuotaDomainInvariant
			}
			hasMember, memberErr := planQuotaDomainHasMember(tx, request.credential)
			if memberErr != nil {
				return nil, memberErr
			}
			if hasMember {
				return nil, errPlanQuotaDomainInvariant
			}
			if createErr := authorityDB.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "credential_hash"}},
				DoNothing: true,
			}).Create(&PlanQuotaDomain{
				CredentialHash: hash,
				State:          PlanQuotaDomainStateActive,
			}).Error; createErr != nil {
				return nil, createErr
			}
			lockedQuery := lockForUpdate(authorityDB).
				Where("credential_hash = ?", hash).
				Limit(1).
				Find(&domain)
			if lockedQuery.Error != nil {
				return nil, lockedQuery.Error
			}
			if lockedQuery.RowsAffected != 1 {
				return nil, errPlanQuotaDomainInvariant
			}
		} else if err != nil {
			return nil, err
		}
		if domain.State != PlanQuotaDomainStateActive &&
			domain.State != PlanQuotaDomainStateDisabled {
			return nil, errPlanQuotaDomainInvariant
		}
		locked[hash] = &domain
	}
	return locked, nil
}

func planQuotaDomainHasMember(tx *gorm.DB, credential string) (bool, error) {
	if credential == "" {
		return false, nil
	}
	var channels []Channel
	if err := tx.Where("tag LIKE ?", "plan:%").Find(&channels).Error; err != nil {
		return false, err
	}
	for index := range channels {
		selected, member := planQuotaDomainCredential(&channels[index])
		if member && selected == credential {
			return true, nil
		}
	}
	return false, nil
}

func applyPlanQuotaDomainState(
	channel *Channel,
	domain *PlanQuotaDomain,
	replaceOwnership bool,
) {
	if channel == nil || domain == nil || domain.State != PlanQuotaDomainStateDisabled {
		return
	}
	if channel.Status == common.ChannelStatusManuallyDisabled {
		return
	}
	info := channel.GetOtherInfo()
	if channel.Status == common.ChannelStatusAutoDisabled && !replaceOwnership {
		marker, hasMarker := info["quota_domain_id"].(string)
		quotaType, typeValid := info["quota_type"].(string)
		legacyDomain, legacyValid := info["quota_domain"].(string)
		sameOwner := hasMarker &&
			marker == domain.CredentialHash &&
			typeValid &&
			quotaType == "plan"
		legacyOwner := !hasMarker &&
			typeValid &&
			quotaType == "plan" &&
			legacyValid &&
			legacyDomain == channel.GetTag()
		if !sameOwner && !legacyOwner {
			return
		}
	}
	channel.Status = common.ChannelStatusAutoDisabled
	info["quota_domain"] = channel.GetTag()
	info["quota_domain_id"] = domain.CredentialHash
	info["quota_generation"] = strconv.FormatInt(domain.Generation, 10)
	info["quota_type"] = "plan"
	if domain.DisabledUntil > 0 {
		info["disabled_until"] = domain.DisabledUntil
	} else {
		delete(info, "disabled_until")
		delete(info, "quota_reset_at")
	}
	channel.SetOtherInfo(info)
}

func clearPlanQuotaDomainOwnership(channel *Channel, hash string, tag string) bool {
	if channel == nil {
		return false
	}
	info := channel.GetOtherInfo()
	marker, hasMarker := info["quota_domain_id"]
	markerValue, markerValid := marker.(string)
	quotaType, typeValid := info["quota_type"].(string)
	legacyDomain, legacyValid := info["quota_domain"].(string)
	owned := hasMarker && markerValid && markerValue == hash
	if !hasMarker {
		owned = typeValid &&
			quotaType == "plan" &&
			legacyValid &&
			legacyDomain == tag
	}
	if !owned {
		return false
	}
	delete(info, "disabled_until")
	delete(info, "quota_reset_at")
	delete(info, "quota_domain")
	delete(info, "quota_domain_id")
	delete(info, "quota_generation")
	delete(info, "quota_type")
	channel.SetOtherInfo(info)
	return true
}

func clearStalePlanQuotaDomainOwnership(
	channel *Channel,
	currentHash string,
	currentMember bool,
) (bool, error) {
	if channel == nil {
		return false, nil
	}
	info := channel.GetOtherInfo()
	rawMarker, hasMarker := info["quota_domain_id"]
	if !hasMarker {
		return false, nil
	}
	marker, markerValid := rawMarker.(string)
	quotaType, typeValid := info["quota_type"].(string)
	if currentMember && markerValid && marker == currentHash {
		return false, nil
	}
	if !markerValid || len(marker) != sha256.Size*2 || quotaType != "plan" || !typeValid {
		if currentMember {
			return false, errors.New("plan quota domain has conflicting ownership metadata")
		}
		return false, nil
	}
	if _, err := hex.DecodeString(marker); err != nil {
		if currentMember {
			return false, errors.New("plan quota domain has conflicting ownership metadata")
		}
		return false, nil
	}
	if rawGeneration, exists := info["quota_generation"]; exists {
		generation, ok := rawGeneration.(string)
		if !ok {
			return false, errors.New("plan quota domain has malformed generation metadata")
		}
		parsed, err := strconv.ParseInt(generation, 10, 64)
		if err != nil || parsed < 0 {
			return false, errors.New("plan quota domain has malformed generation metadata")
		}
	}
	if rawDeadline, exists := info["disabled_until"]; exists {
		deadline, valid := planQuotaMetadataInt64(rawDeadline)
		if !valid || deadline < 0 {
			return false, errors.New("plan quota domain has malformed deadline metadata")
		}
	}
	if rawDomain, exists := info["quota_domain"]; exists {
		domain, valid := rawDomain.(string)
		if !valid || domain == "" {
			return false, errors.New("plan quota domain has malformed ownership metadata")
		}
	}
	if !clearPlanQuotaDomainOwnership(channel, marker, "") {
		return false, errPlanQuotaDomainInvariant
	}
	return true, nil
}

func insertChannelsWithPlanQuotaDomains(tx *gorm.DB, channels []*Channel) error {
	requests := make([]planQuotaCredentialLock, 0, len(channels))
	for _, channel := range channels {
		if request, ok := planQuotaCredentialForChannel(channel, true); ok {
			requests = append(requests, request)
		}
	}
	domains, err := lockPlanQuotaDomains(tx, requests)
	if err != nil {
		return err
	}
	for _, channel := range channels {
		if hash, member := PlanQuotaDomainMembership(channel); member {
			applyPlanQuotaDomainState(channel, domains[hash], false)
		}
	}
	for _, channel := range channels {
		if err := tx.Create(channel).Error; err != nil {
			return err
		}
		if err := channel.AddAbilities(tx); err != nil {
			return err
		}
	}
	return nil
}

// InsertChannelWithAbilities inserts a channel through the domain authority
// protocol inside a caller-owned transaction.
func InsertChannelWithAbilities(tx *gorm.DB, channel *Channel) error {
	if tx == nil || channel == nil {
		return errors.New("channel insert transaction is missing")
	}
	return insertChannelsWithPlanQuotaDomains(tx, []*Channel{channel})
}

func updateChannelWithPlanQuotaDomains(channel *Channel, expected *Channel) (*Channel, error) {
	if channel == nil || expected == nil || channel.Id == 0 || expected.Id != channel.Id {
		return nil, errors.New("channel update snapshot is missing")
	}

	prospective := *expected
	if channel.Key != "" {
		prospective.Key = channel.Key
	}
	if channel.Tag != nil {
		prospective.Tag = channel.Tag
	}
	if !reflect.DeepEqual(channel.ChannelInfo, ChannelInfo{}) {
		prospective.ChannelInfo = channel.ChannelInfo
	}

	requests := make([]planQuotaCredentialLock, 0, 2)
	oldRequest, oldMember := planQuotaCredentialForChannel(expected, false)
	if oldMember {
		requests = append(requests, oldRequest)
	}
	newRequest, newMember := planQuotaCredentialForChannel(&prospective, true)
	if newMember {
		if oldMember && oldRequest.hash == newRequest.hash {
			newRequest.allowCreate = false
		}
		requests = append(requests, newRequest)
	}

	var updated Channel
	err := DB.Transaction(func(tx *gorm.DB) error {
		domains, err := lockPlanQuotaDomains(tx, requests)
		if err != nil {
			return err
		}

		var current Channel
		if err := lockForUpdate(tx).Where("id = ?", expected.Id).First(&current).Error; err != nil {
			return err
		}
		if !reflect.DeepEqual(current, *expected) {
			return errPlanQuotaDomainSnapshotChanged
		}
		update := *channel
		if reflect.DeepEqual(channel.ChannelInfo, expected.ChannelInfo) {
			update.ChannelInfo = ChannelInfo{}
		}
		if err := tx.Model(&Channel{}).
			Where("id = ?", current.Id).
			Updates(&update).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", current.Id).First(&updated).Error; err != nil {
			return err
		}
		actualHash, actualMember := PlanQuotaDomainMembership(&updated)
		if actualMember != newMember || (actualMember && actualHash != newRequest.hash) {
			return errPlanQuotaDomainSnapshotChanged
		}
		ownershipCleared := oldMember &&
			(!actualMember || actualHash != oldRequest.hash) &&
			clearPlanQuotaDomainOwnership(&updated, oldRequest.hash, expected.GetTag())
		if actualMember {
			applyPlanQuotaDomainState(&updated, domains[actualHash], ownershipCleared)
			if err := tx.Model(&Channel{}).Where("id = ?", updated.Id).Updates(map[string]any{
				"status":     updated.Status,
				"other_info": updated.OtherInfo,
			}).Error; err != nil {
				return err
			}
		} else if ownershipCleared {
			if err := tx.Model(&Channel{}).
				Where("id = ?", updated.Id).
				Update("other_info", updated.OtherInfo).Error; err != nil {
				return err
			}
		}
		return updated.UpdateAbilities(tx)
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

func mutateChannelSnapshotsWithPlanQuotaDomains(
	expectedChannels []*Channel,
	rejectDisabledEnable bool,
	mutate func(*Channel),
) ([]*Channel, error) {
	if len(expectedChannels) == 0 {
		return nil, nil
	}
	type channelMutation struct {
		expected *Channel
		desired  *Channel
	}
	mutations := make([]channelMutation, 0, len(expectedChannels))
	for _, expected := range expectedChannels {
		if expected == nil || expected.Id <= 0 {
			return nil, errors.New("channel mutation snapshot is missing")
		}
		desired := *expected
		mutate(&desired)
		mutations = append(mutations, channelMutation{expected: expected, desired: &desired})
	}
	sort.Slice(mutations, func(i, j int) bool {
		return mutations[i].expected.Id < mutations[j].expected.Id
	})
	for index := 1; index < len(mutations); index++ {
		if mutations[index-1].expected.Id == mutations[index].expected.Id {
			return nil, errors.New("channel mutation snapshot is duplicated")
		}
	}

	requests := make([]planQuotaCredentialLock, 0, len(mutations)*2)
	for _, mutation := range mutations {
		oldRequest, oldMember := planQuotaCredentialForChannel(mutation.expected, false)
		if oldMember {
			requests = append(requests, oldRequest)
		}
		newRequest, newMember := planQuotaCredentialForChannel(mutation.desired, true)
		if newMember {
			if oldMember && oldRequest.hash == newRequest.hash {
				newRequest.allowCreate = false
			}
			requests = append(requests, newRequest)
		}
	}

	updated := make([]*Channel, 0, len(mutations))
	channelIDs := make([]int, 0, len(mutations))
	for _, mutation := range mutations {
		channelIDs = append(channelIDs, mutation.expected.Id)
	}
	observeChannelStatusPublication(channelStatusPublicationBeforeWrite)
	_, err := withChannelStatusesLocks(channelIDs, func() (bool, error) {
		err := DB.Transaction(func(tx *gorm.DB) error {
			domains, err := lockPlanQuotaDomains(tx, requests)
			if err != nil {
				return err
			}
			for _, mutation := range mutations {
				oldHash, oldMember := PlanQuotaDomainMembership(mutation.expected)
				hash, member := PlanQuotaDomainMembership(mutation.desired)
				ownershipCleared := oldMember &&
					(!member || hash != oldHash) &&
					clearPlanQuotaDomainOwnership(
						mutation.desired,
						oldHash,
						mutation.expected.GetTag(),
					)
				if member {
					if rejectDisabledEnable &&
						mutation.desired.Status == common.ChannelStatusEnabled &&
						domains[hash].State == PlanQuotaDomainStateDisabled {
						return errPlanQuotaDomainInvariant
					}
					applyPlanQuotaDomainState(
						mutation.desired,
						domains[hash],
						ownershipCleared,
					)
				}
			}
			for _, mutation := range mutations {
				var current Channel
				if err := lockForUpdate(tx).
					Where("id = ?", mutation.expected.Id).
					First(&current).Error; err != nil {
					return err
				}
				if !reflect.DeepEqual(current, *mutation.expected) {
					return errPlanQuotaDomainSnapshotChanged
				}
			}
			for _, mutation := range mutations {
				if err := tx.Model(&Channel{}).
					Where("id = ?", mutation.desired.Id).
					Select("*").
					Omit("id").
					Updates(mutation.desired).Error; err != nil {
					return err
				}
				if err := mutation.desired.UpdateAbilities(tx); err != nil {
					return err
				}
				snapshot := *mutation.desired
				updated = append(updated, &snapshot)
			}
			return nil
		})
		if err == nil {
			observeChannelStatusPublication(channelStatusPublicationAfterCommit)
			CacheUpdateChannels(updated)
		}
		return err == nil, err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// UpdateChannelCredentialIfUnchanged rotates a credential only while the
// caller's complete channel snapshot remains current.
func UpdateChannelCredentialIfUnchanged(
	expected *Channel,
	credential string,
) (*Channel, bool, error) {
	updated, err := mutateChannelSnapshotsWithPlanQuotaDomains(
		[]*Channel{expected},
		false,
		func(channel *Channel) {
			channel.Key = credential
		},
	)
	if errors.Is(err, errPlanQuotaDomainSnapshotChanged) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(updated) != 1 {
		return nil, false, nil
	}
	return updated[0], true, nil
}

func nextPlanQuotaGeneration(current int64, requested int64) int64 {
	next := time.Now().UnixNano()
	if next <= current {
		next = current + 1
	}
	if requested > next {
		next = requested
	}
	return next
}

func DisablePlanQuotaDomain(
	request PlanQuotaDomainDisableRequest,
) (PlanQuotaDomainTransitionResult, error) {
	var result PlanQuotaDomainTransitionResult
	hash, ok := PlanQuotaDomainHash(request.ObservedCredential)
	if !ok {
		return result, errors.New("plan quota domain credential is empty")
	}
	if request.FailingChannelID <= 0 || !strings.HasPrefix(request.ObservedTag, "plan:") {
		return result, errors.New("plan quota domain disable identity is invalid")
	}

	err := commitAndPublishChannelStatus(
		func() error {
			return DB.Transaction(func(tx *gorm.DB) error {
				domains, err := lockPlanQuotaDomains(tx, []planQuotaCredentialLock{{
					credential:  request.ObservedCredential,
					hash:        hash,
					allowCreate: true,
				}})
				if err != nil {
					return err
				}
				domain := domains[hash]
				domain.Generation = nextPlanQuotaGeneration(domain.Generation, request.Generation)
				domain.State = PlanQuotaDomainStateDisabled
				if request.ResetAt > 0 {
					domain.DisabledUntil = request.ResetAt + 60
				} else {
					domain.DisabledUntil = 0
				}

				members, err := findPlanQuotaDomainMembers(tx, request.ObservedCredential)
				if err != nil {
					return err
				}
				lockedMembers, err := lockPlanQuotaMemberSnapshots(tx, members)
				if err != nil {
					return err
				}
				if err := planQuotaAuthorityDB(tx).Model(&PlanQuotaDomain{}).
					Where("credential_hash = ?", hash).
					Updates(map[string]any{
						"generation":     domain.Generation,
						"state":          domain.State,
						"disabled_until": domain.DisabledUntil,
					}).Error; err != nil {
					return err
				}

				for index := range lockedMembers {
					channel := &lockedMembers[index]
					if channel.Status != common.ChannelStatusEnabled {
						owner, ownerOK := channel.GetOtherInfo()["quota_domain_id"].(string)
						if channel.Status != common.ChannelStatusAutoDisabled ||
							!ownerOK ||
							owner != hash {
							continue
						}
					}
					wasEnabled := channel.Status == common.ChannelStatusEnabled
					if wasEnabled {
						result.NewlyDisabled++
					}
					channel.Status = common.ChannelStatusAutoDisabled
					info := channel.GetOtherInfo()
					if wasEnabled {
						info["status_reason"] = request.Reason
						info["status_time"] = common.GetTimestamp()
					}
					info["quota_domain"] = channel.GetTag()
					info["quota_domain_id"] = hash
					info["quota_generation"] = strconv.FormatInt(domain.Generation, 10)
					info["quota_type"] = "plan"
					if request.ResetAt > 0 {
						info["quota_reset_at"] = request.ResetAt
						info["disabled_until"] = domain.DisabledUntil
					} else {
						delete(info, "quota_reset_at")
						delete(info, "disabled_until")
					}
					channel.SetOtherInfo(info)
					if err := tx.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
						"status":     channel.Status,
						"other_info": channel.OtherInfo,
					}).Error; err != nil {
						return err
					}
					if err := tx.Model(&Ability{}).Where("channel_id = ?", channel.Id).
						Select("enabled").Update("enabled", false).Error; err != nil {
						return err
					}
					snapshot := *channel
					result.Channels = append(result.Channels, &snapshot)
				}
				return nil
			})
		},
		func() {
			publishPlanQuotaTransition(result.Channels)
		},
	)
	if err != nil {
		return PlanQuotaDomainTransitionResult{}, err
	}
	return result, nil
}

func RecoverPlanQuotaDomain(
	request PlanQuotaDomainRecoveryRequest,
) (PlanQuotaDomainTransitionResult, error) {
	var result PlanQuotaDomainTransitionResult
	if request.Source == nil || request.Source.Id <= 0 {
		return result, errors.New("plan quota domain recovery snapshot is missing")
	}
	hash, member := PlanQuotaDomainMembership(request.Source)
	if !member {
		return result, errPlanQuotaDomainInvariant
	}
	credential, _ := planQuotaDomainCredential(request.Source)

	err := commitAndPublishChannelStatus(
		func() error {
			return DB.Transaction(func(tx *gorm.DB) error {
				domains, err := lockPlanQuotaDomains(tx, []planQuotaCredentialLock{{
					credential: credential,
					hash:       hash,
				}})
				if err != nil {
					return err
				}
				domain := domains[hash]
				sourceGeneration, sourceDeadline, valid := planQuotaRecoverySnapshot(request.Source, hash)
				if domain.State != PlanQuotaDomainStateDisabled || !valid {
					return nil
				}
				switch request.Source.Status {
				case common.ChannelStatusAutoDisabled:
					if domain.Generation != sourceGeneration ||
						domain.DisabledUntil != sourceDeadline ||
						(request.RequireDue && domain.DisabledUntil > request.RecoveryAt) {
						return nil
					}
				case common.ChannelStatusManuallyDisabled:
					if request.RequireDue {
						return nil
					}
				default:
					return nil
				}

				members, err := findPlanQuotaDomainMembers(tx, credential)
				if err != nil {
					return err
				}
				routes, err := lockPlanQuotaRecoveryRoutes(tx, members)
				if err != nil {
					return err
				}
				lockedMembers, err := lockPlanQuotaMemberSnapshots(tx, members)
				if err != nil {
					return err
				}
				sourceMatched := false
				for index := range lockedMembers {
					if lockedMembers[index].Id == request.Source.Id {
						sourceMatched = reflect.DeepEqual(lockedMembers[index], *request.Source)
						break
					}
				}
				if !sourceMatched {
					return nil
				}

				for index := range lockedMembers {
					channel := &lockedMembers[index]
					if channel.Status != common.ChannelStatusAutoDisabled {
						continue
					}
					generation, deadline, owned := planQuotaRecoverySnapshot(channel, hash)
					if !owned {
						continue
					}
					if generation != domain.Generation || deadline != domain.DisabledUntil {
						return nil
					}
				}

				domain.Generation = nextPlanQuotaGeneration(domain.Generation, 0)
				domain.State = PlanQuotaDomainStateActive
				domain.DisabledUntil = 0
				result.Recovered = true
				if err := planQuotaAuthorityDB(tx).Model(&PlanQuotaDomain{}).
					Where("credential_hash = ?", hash).
					Updates(map[string]any{
						"generation":     domain.Generation,
						"state":          domain.State,
						"disabled_until": domain.DisabledUntil,
					}).Error; err != nil {
					return err
				}

				for index := range lockedMembers {
					channel := &lockedMembers[index]
					_, _, owned := planQuotaRecoverySnapshot(channel, hash)
					if !owned {
						continue
					}
					wasAutoDisabled := channel.Status == common.ChannelStatusAutoDisabled
					info := channel.GetOtherInfo()
					delete(info, "disabled_until")
					delete(info, "quota_reset_at")
					delete(info, "quota_domain")
					delete(info, "quota_domain_id")
					delete(info, "quota_generation")
					delete(info, "quota_type")
					if planQuotaRouteAllowsRecovery(channel, routes[channel.Id], request.RecoveryAt) &&
						wasAutoDisabled {
						channel.Status = common.ChannelStatusEnabled
						info["status_reason"] = ""
						info["status_time"] = common.GetTimestamp()
						result.NewlyEnabled++
					}
					channel.SetOtherInfo(info)
					if err := tx.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
						"status":     channel.Status,
						"other_info": channel.OtherInfo,
					}).Error; err != nil {
						return err
					}
					if wasAutoDisabled {
						if err := tx.Model(&Ability{}).Where("channel_id = ?", channel.Id).
							Select("enabled").Update("enabled", channel.Status == common.ChannelStatusEnabled).Error; err != nil {
							return err
						}
					}
					snapshot := *channel
					result.Channels = append(result.Channels, &snapshot)
				}
				return nil
			})
		},
		func() {
			publishPlanQuotaTransition(result.Channels)
		},
	)
	if err != nil {
		return PlanQuotaDomainTransitionResult{}, err
	}
	return result, nil
}

func findPlanQuotaDomainMembers(tx *gorm.DB, credential string) ([]Channel, error) {
	var candidates []Channel
	if err := tx.Where("tag LIKE ?", "plan:%").Find(&candidates).Error; err != nil {
		return nil, err
	}
	members := make([]Channel, 0, len(candidates))
	for index := range candidates {
		selected, member := planQuotaDomainCredential(&candidates[index])
		if member && selected == credential {
			members = append(members, candidates[index])
		}
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].Id < members[j].Id
	})
	return members, nil
}

func lockPlanQuotaMemberSnapshots(tx *gorm.DB, expected []Channel) ([]Channel, error) {
	locked := make([]Channel, len(expected))
	for index := range expected {
		if err := lockForUpdate(tx).
			Where("id = ?", expected[index].Id).
			First(&locked[index]).Error; err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(locked[index], expected[index]) {
			return nil, errPlanQuotaDomainSnapshotChanged
		}
	}
	return locked, nil
}

func planQuotaRecoverySnapshot(channel *Channel, hash string) (int64, int64, bool) {
	channelHash, member := PlanQuotaDomainMembership(channel)
	if !member ||
		channelHash != hash {
		return 0, 0, false
	}
	info := channel.GetOtherInfo()
	deadline := int64(0)
	deadlineValid := true
	if rawDeadline, exists := info["disabled_until"]; exists {
		deadline, deadlineValid = planQuotaMetadataInt64(rawDeadline)
	}
	if marker, exists := info["quota_domain_id"]; exists {
		markerValue, markerValid := marker.(string)
		generationValue, generationValid := info["quota_generation"].(string)
		generation, generationErr := strconv.ParseInt(generationValue, 10, 64)
		return generation, deadline, markerValid &&
			markerValue == hash &&
			generationValid &&
			generationErr == nil &&
			generation >= 0 &&
			deadlineValid
	}
	quotaType, typeValid := info["quota_type"].(string)
	quotaDomain, domainValid := info["quota_domain"].(string)
	return 0, deadline, typeValid &&
		quotaType == "plan" &&
		domainValid &&
		quotaDomain == channel.GetTag() &&
		deadlineValid
}

func lockPlanQuotaRecoveryRoutes(
	tx *gorm.DB,
	members []Channel,
) (map[int]*UpstreamManagedRoute, error) {
	channelIDs := make([]int, 0, len(members))
	for index := range members {
		channelIDs = append(channelIDs, members[index].Id)
	}
	if len(channelIDs) == 0 {
		return map[int]*UpstreamManagedRoute{}, nil
	}
	var routes []UpstreamManagedRoute
	if err := lockForUpdate(tx).
		Where("channel_id IN ?", channelIDs).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}}).
		Find(&routes).Error; err != nil {
		return nil, err
	}
	byChannel := make(map[int]*UpstreamManagedRoute, len(routes))
	for index := range routes {
		route := routes[index]
		byChannel[route.ChannelID] = &route
	}
	return byChannel, nil
}

func planQuotaRouteAllowsRecovery(
	channel *Channel,
	route *UpstreamManagedRoute,
	recoveryAt int64,
) bool {
	if route == nil {
		return !strings.HasPrefix(channel.GetTag(), "managed:") &&
			!strings.HasPrefix(channel.GetTag(), "plan:managed:")
	}
	return route.State == UpstreamRouteStateActive &&
		route.Rank > 0 &&
		!route.Detached &&
		route.ManualPauseUntil <= recoveryAt
}

func publishPlanQuotaTransition(channels []*Channel) {
	if len(channels) == 0 {
		return
	}
	updates := make([]ChannelStatusCacheUpdate, 0, len(channels))
	for _, channel := range channels {
		updates = append(updates, ChannelStatusCacheUpdate{Snapshot: channel})
	}
	CacheUpdateChannelStatusSnapshots(updates)
}

func planQuotaDomainAllowsStatus(
	tx *gorm.DB,
	channel *Channel,
	status int,
) (bool, error) {
	request, member := planQuotaCredentialForChannel(channel, false)
	if !member {
		return true, nil
	}
	domains, err := lockPlanQuotaDomains(tx, []planQuotaCredentialLock{request})
	if err != nil {
		return false, err
	}
	return status != common.ChannelStatusEnabled ||
		domains[request.hash].State == PlanQuotaDomainStateActive, nil
}
