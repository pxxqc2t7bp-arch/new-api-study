package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/samber/lo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Channel struct {
	Id                 int     `json:"id"`
	Type               int     `json:"type" gorm:"default:0"`
	Key                string  `json:"key" gorm:"not null"`
	OpenAIOrganization *string `json:"openai_organization"`
	TestModel          *string `json:"test_model"`
	Status             int     `json:"status" gorm:"default:1"`
	Name               string  `json:"name" gorm:"index"`
	Weight             *uint   `json:"weight" gorm:"default:0"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	TestTime           int64   `json:"test_time" gorm:"bigint"`
	ResponseTime       int     `json:"response_time"` // in milliseconds
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''"`
	Other              string  `json:"other"`
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models"`
	Group              string  `json:"group" gorm:"type:varchar(64);default:'default'"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:text"`
	//MaxInputTokens     *int    `json:"max_input_tokens" gorm:"default:0"`
	StatusCodeMapping *string `json:"status_code_mapping" gorm:"type:varchar(1024);default:''"`
	Priority          *int64  `json:"priority" gorm:"bigint;default:0"`
	AutoBan           *int    `json:"auto_ban" gorm:"default:1"`
	OtherInfo         string  `json:"other_info"`
	Tag               *string `json:"tag" gorm:"index"`
	Setting           *string `json:"setting" gorm:"type:text"` // 渠道额外设置
	ParamOverride     *string `json:"param_override" gorm:"type:text"`
	HeaderOverride    *string `json:"header_override" gorm:"type:text"`
	Remark            *string `json:"remark" gorm:"type:varchar(255)" validate:"max=255"`
	// add after v0.8.5
	ChannelInfo ChannelInfo `json:"channel_info" gorm:"type:json"`

	OtherSettings string `json:"settings" gorm:"column:settings"` // 其他设置，存储azure版本等不需要检索的信息，详见dto.ChannelOtherSettings

	// cache info
	Keys []string `json:"-" gorm:"-"`
}

type ChannelInfo struct {
	IsMultiKey             bool                  `json:"is_multi_key"`                        // 是否多Key模式
	MultiKeySize           int                   `json:"multi_key_size"`                      // 多Key模式下的Key数量
	MultiKeyStatusList     map[int]int           `json:"multi_key_status_list"`               // key状态列表，key index -> status
	MultiKeyDisabledReason map[int]string        `json:"multi_key_disabled_reason,omitempty"` // key禁用原因列表，key index -> reason
	MultiKeyDisabledTime   map[int]int64         `json:"multi_key_disabled_time,omitempty"`   // key禁用时间列表，key index -> time
	MultiKeyPollingIndex   int                   `json:"multi_key_polling_index"`             // 多Key模式下轮询的key索引
	MultiKeyRecoveryIndex  int                   `json:"multi_key_recovery_index,omitempty"`  // 多Key模式下被动恢复探测游标
	MultiKeyMode           constant.MultiKeyMode `json:"multi_key_mode"`
}

type ChannelSortOptions struct {
	SortBy    string
	SortOrder string
	IDSort    bool
}

var channelSortColumns = map[string]string{
	"id":            "id",
	"name":          "name",
	"priority":      "priority",
	"balance":       "balance",
	"response_time": "response_time",
	"test_time":     "test_time",
}

func NewChannelSortOptions(sortBy string, sortOrder string, idSort bool) ChannelSortOptions {
	normalizedSortBy := strings.ToLower(strings.TrimSpace(sortBy))
	normalizedSortOrder := strings.ToLower(strings.TrimSpace(sortOrder))
	if _, ok := channelSortColumns[normalizedSortBy]; !ok {
		normalizedSortBy = ""
		normalizedSortOrder = ""
	} else if normalizedSortOrder != "asc" {
		normalizedSortOrder = "desc"
	}

	return ChannelSortOptions{
		SortBy:    normalizedSortBy,
		SortOrder: normalizedSortOrder,
		IDSort:    idSort,
	}
}

func (options ChannelSortOptions) Apply(query *gorm.DB) *gorm.DB {
	if columnName, ok := channelSortColumns[options.SortBy]; ok {
		return query.Order(clause.OrderByColumn{
			Column: clause.Column{Name: columnName},
			Desc:   options.SortOrder != "asc",
		})
	}
	if options.IDSort {
		return query.Order(clause.OrderByColumn{
			Column: clause.Column{Name: "id"},
			Desc:   true,
		})
	}
	return query.Order(clause.OrderByColumn{
		Column: clause.Column{Name: "priority"},
		Desc:   true,
	})
}

func resolveChannelSortOptions(idSort bool, sortOptions []ChannelSortOptions) ChannelSortOptions {
	if len(sortOptions) == 0 {
		return NewChannelSortOptions("", "", idSort)
	}
	options := sortOptions[0]
	options.IDSort = options.IDSort || idSort
	return options
}

func NormalizeChannelGroupFilter(group string) string {
	group = strings.TrimSpace(group)
	if group == "" || strings.EqualFold(group, "all") || strings.EqualFold(group, "null") {
		return ""
	}
	return group
}

func channelGroupFilterCondition() string {
	if common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		return `CONCAT(',', ` + commonGroupCol + `, ',') LIKE ? ESCAPE '!'`
	}
	return `(',' || ` + commonGroupCol + ` || ',') LIKE ? ESCAPE '!'`
}

func channelGroupFilterPattern(group string) string {
	group = strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	).Replace(group)
	return "%," + group + ",%"
}

func ApplyChannelGroupFilter(query *gorm.DB, group string) *gorm.DB {
	group = NormalizeChannelGroupFilter(group)
	if group == "" {
		return query
	}
	return query.Where(channelGroupFilterCondition(), channelGroupFilterPattern(group))
}

// Value implements driver.Valuer interface
// 必须返回 string 而非 []byte:PG simple protocol 下 []byte 参数按 bytea
// 编码,写 json 列会触发 SQLSTATE 22P02。
func (c ChannelInfo) Value() (driver.Value, error) {
	b, err := common.Marshal(&c)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner interface
func (c *ChannelInfo) Scan(value interface{}) error {
	return common.Unmarshal(jsonScanBytes(value), c)
}

func (channel *Channel) GetKeys() []string {
	if channel.Key == "" {
		return []string{}
	}
	if len(channel.Keys) > 0 {
		return channel.Keys
	}
	trimmed := strings.TrimSpace(channel.Key)
	// If the key starts with '[', try to parse it as a JSON array (e.g., for Vertex AI scenarios)
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := common.Unmarshal([]byte(trimmed), &arr); err == nil {
			res := make([]string, len(arr))
			for i, v := range arr {
				res[i] = string(v)
			}
			return res
		}
	}
	// Otherwise, fall back to splitting by newline
	keys := strings.Split(strings.Trim(channel.Key, "\n"), "\n")
	return keys
}

// HasEnabledKey reports whether any configured key is enabled by the per-key status map.
func (channel *Channel) HasEnabledKey() bool {
	if channel == nil {
		return false
	}
	statusList := channel.ChannelInfo.MultiKeyStatusList
	for index := range channel.GetKeys() {
		status, exists := statusList[index]
		if !exists || status == common.ChannelStatusEnabled {
			return true
		}
	}
	return false
}

func (channel *Channel) GetNextEnabledKey() (string, int, *types.NewAPIError) {
	// If not in multi-key mode, return the original key string directly.
	if !channel.ChannelInfo.IsMultiKey {
		return channel.Key, 0, nil
	}

	// Obtain all keys (split by \n)
	keys := channel.GetKeys()
	if len(keys) == 0 {
		// No keys available, return error, should disable the channel
		return "", 0, types.NewError(errors.New("no keys available"), types.ErrorCodeChannelNoAvailableKey)
	}

	lock := GetChannelPollingLock(channel.Id)
	lock.Lock()
	defer lock.Unlock()

	statusList := channel.ChannelInfo.MultiKeyStatusList
	// helper to get key status, default to enabled when missing
	getStatus := func(idx int) int {
		if statusList == nil {
			return common.ChannelStatusEnabled
		}
		if status, ok := statusList[idx]; ok {
			return status
		}
		return common.ChannelStatusEnabled
	}

	// Collect indexes of enabled keys
	enabledIdx := make([]int, 0, len(keys))
	for i := range keys {
		if getStatus(i) == common.ChannelStatusEnabled {
			enabledIdx = append(enabledIdx, i)
		}
	}
	// If no specific status list or none enabled, return an explicit error so caller can
	// properly handle a channel with no available keys (e.g. mark channel disabled).
	// Returning the first key here caused requests to keep using an already-disabled key.
	if len(enabledIdx) == 0 {
		return "", 0, types.NewError(errors.New("no enabled keys"), types.ErrorCodeChannelNoAvailableKey)
	}

	switch channel.ChannelInfo.MultiKeyMode {
	case constant.MultiKeyModeRandom:
		// Randomly pick one enabled key
		selectedIdx := enabledIdx[rand.Intn(len(enabledIdx))]
		return keys[selectedIdx], selectedIdx, nil
	case constant.MultiKeyModePolling:
		// Use channel-specific lock to ensure thread-safe polling

		channelInfo, err := CacheGetChannelInfo(channel.Id)
		if err != nil {
			return "", 0, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
		}
		defer func() {
			if common.DebugEnabled {
				logger.LogDebug(nil, "channel %d polling index: %d", channel.Id, channel.ChannelInfo.MultiKeyPollingIndex)
			}
			if !common.MemoryCacheEnabled {
				_ = channel.SaveChannelInfo()
			} else {
				// CacheUpdateChannel(channel)
			}
		}()
		// Start from the saved polling index and look for the next enabled key
		start := channelInfo.MultiKeyPollingIndex
		if start < 0 || start >= len(keys) {
			start = 0
		}
		for i := 0; i < len(keys); i++ {
			idx := (start + i) % len(keys)
			if getStatus(idx) == common.ChannelStatusEnabled {
				// update polling index for next call (point to the next position)
				channel.ChannelInfo.MultiKeyPollingIndex = (idx + 1) % len(keys)
				return keys[idx], idx, nil
			}
		}
		// Fallback – should not happen, but return first enabled key
		return keys[enabledIdx[0]], enabledIdx[0], nil
	default:
		// Unknown mode, default to first enabled key (or original key string)
		return keys[enabledIdx[0]], enabledIdx[0], nil
	}
}

func (channel *Channel) SaveChannelInfo() error {
	return DB.Model(channel).Update("channel_info", channel.ChannelInfo).Error
}

func (channel *Channel) GetModels() []string {
	if channel.Models == "" {
		return []string{}
	}
	return strings.Split(strings.Trim(channel.Models, ","), ",")
}

func (channel *Channel) GetGroups() []string {
	if channel.Group == "" {
		return []string{}
	}
	groups := strings.Split(strings.Trim(channel.Group, ","), ",")
	for i, group := range groups {
		groups[i] = strings.TrimSpace(group)
	}
	return groups
}

func (channel *Channel) GetOtherInfo() map[string]interface{} {
	otherInfo := make(map[string]interface{})
	if channel.OtherInfo != "" {
		err := common.Unmarshal([]byte(channel.OtherInfo), &otherInfo)
		if err != nil {
			common.SysLog(fmt.Sprintf("failed to unmarshal other info: channel_id=%d, tag=%s, name=%s, error=%v", channel.Id, channel.GetTag(), channel.Name, err))
		}
	}
	return otherInfo
}

func (channel *Channel) GetDisabledUntil() int64 {
	value, ok := channel.GetOtherInfo()["disabled_until"]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func (channel *Channel) SetOtherInfo(otherInfo map[string]interface{}) {
	otherInfoBytes, err := json.Marshal(otherInfo)
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to marshal other info: channel_id=%d, tag=%s, name=%s, error=%v", channel.Id, channel.GetTag(), channel.Name, err))
		return
	}
	channel.OtherInfo = string(otherInfoBytes)
}

func (channel *Channel) GetTag() string {
	if channel.Tag == nil {
		return ""
	}
	return *channel.Tag
}

func (channel *Channel) SetTag(tag string) {
	channel.Tag = &tag
}

func (channel *Channel) GetAutoBan() bool {
	if channel.AutoBan == nil {
		return false
	}
	return *channel.AutoBan == 1
}

func (channel *Channel) Save() error {
	return DB.Save(channel).Error
}

// saveStatusState persists only the fields owned by the channel status flow.
// Keeping this allowlist here prevents a stale channel snapshot from
// overwriting credentials, accounting counters, or channel configuration.
func (channel *Channel) saveStatusState() error {
	if channel.Id == 0 {
		return errors.New("channel ID is 0")
	}
	updates := map[string]any{
		"status":     channel.Status,
		"other_info": channel.OtherInfo,
	}
	if channel.ChannelInfo.IsMultiKey {
		updates["channel_info"] = channel.ChannelInfo
	}
	return DB.Model(&Channel{}).Where("id = ?", channel.Id).Updates(updates).Error
}

func GetAllChannels(startIdx int, num int, selectAll bool, idSort bool, sortOptions ...ChannelSortOptions) ([]*Channel, error) {
	var channels []*Channel
	var err error
	order := resolveChannelSortOptions(idSort, sortOptions)
	if selectAll {
		err = order.Apply(DB).Find(&channels).Error
	} else {
		err = order.Apply(DB).Limit(num).Offset(startIdx).Omit("key").Find(&channels).Error
	}
	return channels, err
}

func GetChannelsByTag(tag string, idSort bool, selectAll bool, sortOptions ...ChannelSortOptions) ([]*Channel, error) {
	var channels []*Channel
	order := resolveChannelSortOptions(idSort, sortOptions)
	query := order.Apply(DB.Where("tag = ?", tag))
	if !selectAll {
		query = query.Omit("key")
	}
	err := query.Find(&channels).Error
	return channels, err
}

func SearchChannels(keyword string, group string, model string, idSort bool, sortOptions ...ChannelSortOptions) ([]*Channel, error) {
	var channels []*Channel
	modelsCol := "`models`"

	// 如果是 PostgreSQL，使用双引号
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		modelsCol = `"models"`
	}

	baseURLCol := "`base_url`"
	// 如果是 PostgreSQL，使用双引号
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		baseURLCol = `"base_url"`
	}

	order := resolveChannelSortOptions(idSort, sortOptions)

	// 构造基础查询
	baseQuery := DB.Model(&Channel{}).Omit("key")

	// 构造WHERE子句
	whereClause := "(id = ? OR name LIKE ? OR " + commonKeyCol + " = ? OR " + baseURLCol + " LIKE ?) AND " + modelsCol + " LIKE ?"
	args := []any{common.String2Int(keyword), "%" + keyword + "%", keyword, "%" + keyword + "%", "%" + model + "%"}
	baseQuery = ApplyChannelGroupFilter(baseQuery.Where(whereClause, args...), group)

	// 执行查询
	err := order.Apply(baseQuery).Find(&channels).Error
	if err != nil {
		return nil, err
	}
	return channels, nil
}

// GetChannelById loads a channel directly from the database, bypassing the
// in-memory channel cache.
//
// WARNING: do NOT call this on request hot paths (middleware, distribution,
// relay submit/retry, polling). Every call is a synchronous DB query and will
// not see cache-only state. Use CacheGetChannel instead: it serves from the
// in-memory cache and falls back to this function automatically when
// MemoryCacheEnabled is false. Direct use is appropriate only where fresh DB
// state is required, e.g. admin CRUD, channel testing, or cache (re)building.
func GetChannelById(id int, selectAll bool) (*Channel, error) {
	channel := &Channel{Id: id}
	var err error = nil
	if selectAll {
		err = DB.First(channel, "id = ?", id).Error
	} else {
		err = DB.Omit("key").First(channel, "id = ?", id).Error
	}
	if err != nil {
		return nil, err
	}
	return channel, nil
}

func BatchInsertChannels(channels []Channel) error {
	if len(channels) == 0 {
		return nil
	}
	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	for _, chunk := range lo.Chunk(channels, 50) {
		if err := tx.Create(&chunk).Error; err != nil {
			tx.Rollback()
			return err
		}
		for _, channel_ := range chunk {
			if err := channel_.AddAbilities(tx); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit().Error
}

func BatchDeleteChannels(ids []int) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// 使用事务 分批删除channel表和abilities表
	tx := DB.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	var deletedCount int64
	for _, chunk := range lo.Chunk(ids, 200) {
		result := tx.Where("id in (?)", chunk).Delete(&Channel{})
		if result.Error != nil {
			tx.Rollback()
			return 0, result.Error
		}
		deletedCount += result.RowsAffected
		if err := tx.Where("channel_id in (?)", chunk).Delete(&Ability{}).Error; err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	return deletedCount, nil
}

func (channel *Channel) GetPriority() int64 {
	if channel.Priority == nil {
		return 0
	}
	return *channel.Priority
}

func (channel *Channel) GetWeight() int {
	if channel.Weight == nil {
		return 0
	}
	return int(*channel.Weight)
}

func (channel *Channel) GetBaseURL() string {
	if channel.BaseURL == nil {
		return ""
	}
	url := *channel.BaseURL
	if url == "" {
		url = constant.GetChannelBaseURL(channel.Type)
	}
	return url
}

func (channel *Channel) GetModelMapping() string {
	if channel.ModelMapping == nil {
		return ""
	}
	return *channel.ModelMapping
}

func (channel *Channel) GetStatusCodeMapping() string {
	if channel.StatusCodeMapping == nil {
		return ""
	}
	return *channel.StatusCodeMapping
}

func (channel *Channel) Insert() error {
	var err error
	err = DB.Create(channel).Error
	if err != nil {
		return err
	}
	err = channel.AddAbilities(nil)
	return err
}

func (channel *Channel) Update() error {
	// If this is a multi-key channel, recalculate MultiKeySize based on the current key list to avoid inconsistency after editing keys
	if channel.ChannelInfo.IsMultiKey {
		var keyStr string
		if channel.Key != "" {
			keyStr = channel.Key
		} else {
			// If key is not provided, read the existing key from the database
			if existing, err := GetChannelById(channel.Id, true); err == nil {
				keyStr = existing.Key
			}
		}
		// Parse the key list (supports newline separation or JSON array)
		keys := []string{}
		if keyStr != "" {
			trimmed := strings.TrimSpace(keyStr)
			if strings.HasPrefix(trimmed, "[") {
				var arr []json.RawMessage
				if err := common.Unmarshal([]byte(trimmed), &arr); err == nil {
					keys = make([]string, len(arr))
					for i, v := range arr {
						keys[i] = string(v)
					}
				}
			}
			if len(keys) == 0 { // fallback to newline split
				keys = strings.Split(strings.Trim(keyStr, "\n"), "\n")
			}
		}
		channel.ChannelInfo.MultiKeySize = len(keys)
		// Clean up status data that exceeds the new key count to prevent index out of range
		if channel.ChannelInfo.MultiKeyStatusList != nil {
			for idx := range channel.ChannelInfo.MultiKeyStatusList {
				if idx >= channel.ChannelInfo.MultiKeySize {
					delete(channel.ChannelInfo.MultiKeyStatusList, idx)
				}
			}
		}
	}
	var err error
	err = DB.Model(channel).Updates(channel).Error
	if err != nil {
		return err
	}
	DB.Model(channel).First(channel, "id = ?", channel.Id)
	err = channel.UpdateAbilities(nil)
	return err
}

func (channel *Channel) UpdateResponseTime(responseTime int64) {
	err := DB.Model(channel).Select("response_time", "test_time").Updates(Channel{
		TestTime:     common.GetTimestamp(),
		ResponseTime: int(responseTime),
	}).Error
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to update response time: channel_id=%d, error=%v", channel.Id, err))
	}
}

func (channel *Channel) UpdateBalance(balance float64) {
	err := DB.Model(channel).Select("balance_updated_time", "balance").Updates(Channel{
		BalanceUpdatedTime: common.GetTimestamp(),
		Balance:            balance,
	}).Error
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to update balance: channel_id=%d, error=%v", channel.Id, err))
	}
}

func (channel *Channel) Delete() error {
	var err error
	err = DB.Delete(channel).Error
	if err != nil {
		return err
	}
	err = channel.DeleteAbilities()
	return err
}

var channelStatusLock sync.Mutex

// channelPollingLocks stores locks for each channel.id to ensure thread-safe polling
var channelPollingLocks sync.Map

// GetChannelPollingLock returns or creates a mutex for the given channel ID
func GetChannelPollingLock(channelId int) *sync.Mutex {
	if lock, exists := channelPollingLocks.Load(channelId); exists {
		return lock.(*sync.Mutex)
	}
	// Create new lock for this channel
	newLock := &sync.Mutex{}
	actual, _ := channelPollingLocks.LoadOrStore(channelId, newLock)
	return actual.(*sync.Mutex)
}

// CleanupChannelPollingLocks removes locks for channels that no longer exist
// This is optional and can be called periodically to prevent memory leaks
func CleanupChannelPollingLocks() {
	var activeChannelIds []int
	DB.Model(&Channel{}).Pluck("id", &activeChannelIds)

	activeChannelSet := make(map[int]bool)
	for _, id := range activeChannelIds {
		activeChannelSet[id] = true
	}

	channelPollingLocks.Range(func(key, value interface{}) bool {
		channelId := key.(int)
		if !activeChannelSet[channelId] {
			channelPollingLocks.Delete(channelId)
		}
		return true
	})
}

func handlerMultiKeyUpdate(channel *Channel, usingKey string, status int, reason string) {
	keys := channel.GetKeys()
	if len(keys) == 0 {
		channel.Status = status
	} else {
		keyIndex := -1
		for i, key := range keys {
			if key == usingKey {
				keyIndex = i
				break
			}
		}
		if keyIndex < 0 {
			if usingKey != "" {
				common.SysLog(fmt.Sprintf("failed to update multi-key status: channel_id=%d, using key not found", channel.Id))
				return
			}
			channel.Status = status
			info := channel.GetOtherInfo()
			info["status_reason"] = reason
			info["status_time"] = common.GetTimestamp()
			channel.SetOtherInfo(info)
			return
		}
		if channel.ChannelInfo.MultiKeyStatusList == nil {
			channel.ChannelInfo.MultiKeyStatusList = make(map[int]int)
		}
		if status == common.ChannelStatusEnabled {
			delete(channel.ChannelInfo.MultiKeyStatusList, keyIndex)
		} else {
			channel.ChannelInfo.MultiKeyStatusList[keyIndex] = status
			if channel.ChannelInfo.MultiKeyDisabledReason == nil {
				channel.ChannelInfo.MultiKeyDisabledReason = make(map[int]string)
			}
			if channel.ChannelInfo.MultiKeyDisabledTime == nil {
				channel.ChannelInfo.MultiKeyDisabledTime = make(map[int]int64)
			}
			channel.ChannelInfo.MultiKeyDisabledReason[keyIndex] = reason
			channel.ChannelInfo.MultiKeyDisabledTime[keyIndex] = common.GetTimestamp()
		}
		if !channel.HasEnabledKey() {
			channel.Status = common.ChannelStatusAutoDisabled
			info := channel.GetOtherInfo()
			info["status_reason"] = "All keys are disabled"
			info["status_time"] = common.GetTimestamp()
			channel.SetOtherInfo(info)
		} else if status == common.ChannelStatusEnabled {
			channel.Status = common.ChannelStatusEnabled
		}
	}
}

const channelStatusUpdateMaxAttempts = 3

func withChannelStatusLocks(channelId int, update func() (bool, error)) (bool, error) {
	return withChannelStatusesLocks([]int{channelId}, update)
}

func withChannelStatusesLocks(channelIDs []int, update func() (bool, error)) (bool, error) {
	if common.MemoryCacheEnabled {
		channelStatusLock.Lock()
		defer channelStatusLock.Unlock()
	}

	// ChannelInfo stores both multi-key status and the polling cursor. Hold the
	// same per-channel lock from the first read through persistence so neither
	// writer can save a stale JSON snapshot over the other.
	sortedIDs := append([]int(nil), channelIDs...)
	sort.Ints(sortedIDs)
	pollingLocks := make([]*sync.Mutex, 0, len(sortedIDs))
	previousID := 0
	for index, channelID := range sortedIDs {
		if index > 0 && channelID == previousID {
			continue
		}
		pollingLock := GetChannelPollingLock(channelID)
		pollingLock.Lock()
		pollingLocks = append(pollingLocks, pollingLock)
		previousID = channelID
	}
	defer func() {
		for index := len(pollingLocks) - 1; index >= 0; index-- {
			pollingLocks[index].Unlock()
		}
	}()

	return update()
}

func UpdateChannelStatus(channelId int, usingKey string, status int, reason string) bool {
	changed, err := withChannelStatusLocks(channelId, func() (bool, error) {
		return updateChannelStatusLocked(channelId, usingKey, status, reason)
	})
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to update channel status: channel_id=%d, status=%d, error=%v", channelId, status, err))
		return false
	}
	return changed
}

func updateChannelStatusLocked(channelId int, usingKey string, status int, reason string) (bool, error) {
	if common.MemoryCacheEnabled {
		channelCache, _ := CacheGetChannel(channelId)
		if channelCache == nil {
			return false, nil
		}
		if channelCache.ChannelInfo.IsMultiKey {
			beforeStatus := channelCache.Status
			// 如果是多Key模式，更新缓存中的状态
			handlerMultiKeyUpdate(channelCache, usingKey, status, reason)
			if beforeStatus != channelCache.Status {
				CacheUpdateChannelStatus(channelId, channelCache.Status)
			}
			//CacheUpdateChannel(channelCache)
			//return true
		}
	}

	channel, err := GetChannelById(channelId, true)
	if err != nil {
		return false, nil
	}
	if channel.Status == status {
		return false, nil
	}

	if channel.ChannelInfo.IsMultiKey {
		beforeStatus := channel.Status
		handlerMultiKeyUpdate(channel, usingKey, status, reason)
		if err := channel.saveStatusState(); err != nil {
			return false, err
		}
		if beforeStatus != channel.Status {
			if err := UpdateAbilityStatus(channelId, status == common.ChannelStatusEnabled); err != nil {
				common.SysLog(fmt.Sprintf("failed to update ability status: channel_id=%d, error=%v", channelId, err))
			}
		}
		return true, nil
	}

	for range channelStatusUpdateMaxAttempts {
		current, err := GetChannelById(channelId, true)
		if err != nil {
			return false, err
		}
		if current.ChannelInfo.IsMultiKey || current.Status == status {
			return false, nil
		}

		expectedOtherInfo := current.OtherInfo
		info := current.GetOtherInfo()
		info["status_reason"] = reason
		info["status_time"] = common.GetTimestamp()
		current.SetOtherInfo(info)

		changed, err := updateSingleKeyChannelStatusIfUnchangedLocked(
			channelId,
			current.Key,
			current.GetTag(),
			current.Status,
			expectedOtherInfo,
			status,
			current.OtherInfo,
		)
		if err != nil || changed {
			return changed, err
		}
	}
	return false, nil
}

// UpdateSingleKeyChannelStatusIfUnchanged atomically updates status-owned
// channel state only while the locked row still matches the caller's snapshot.
func UpdateSingleKeyChannelStatusIfUnchanged(
	channelId int,
	expectedKey string,
	expectedTag string,
	expectedStatus int,
	expectedOtherInfo string,
	status int,
	otherInfo string,
) (bool, error) {
	return withChannelStatusLocks(channelId, func() (bool, error) {
		return updateSingleKeyChannelStatusIfUnchangedLocked(
			channelId,
			expectedKey,
			expectedTag,
			expectedStatus,
			expectedOtherInfo,
			status,
			otherInfo,
		)
	})
}

func updateSingleKeyChannelStatusIfUnchangedLocked(
	channelId int,
	expectedKey string,
	expectedTag string,
	expectedStatus int,
	expectedOtherInfo string,
	status int,
	otherInfo string,
) (bool, error) {
	changed := false
	var updated Channel
	err := DB.Transaction(func(tx *gorm.DB) error {
		var current Channel
		if err := lockForUpdate(tx).Where("id = ?", channelId).First(&current).Error; err != nil {
			return err
		}
		if current.ChannelInfo.IsMultiKey ||
			current.Key != expectedKey ||
			current.GetTag() != expectedTag ||
			current.Status != expectedStatus ||
			current.OtherInfo != expectedOtherInfo {
			return nil
		}

		if err := tx.Model(&Channel{}).Where("id = ?", channelId).Updates(map[string]any{
			"status":     status,
			"other_info": otherInfo,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Ability{}).Where("channel_id = ?", channelId).
			Select("enabled").Update("enabled", status == common.ChannelStatusEnabled).Error; err != nil {
			return err
		}
		current.Status = status
		current.OtherInfo = otherInfo
		updated = current
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if changed {
		CacheUpdateChannel(&updated)
	}
	return changed, nil
}

type SingleKeyChannelStatusUpdate struct {
	Expected  *Channel
	Status    int
	OtherInfo string
}

var errSingleKeyChannelStatusSnapshotChanged = errors.New("single-key channel status snapshot changed")

// UpdateSingleKeyChannelStatusesIfUnchanged atomically applies a complete
// single-key status domain after locking and validating every row.
func UpdateSingleKeyChannelStatusesIfUnchanged(updates []SingleKeyChannelStatusUpdate) (bool, error) {
	if len(updates) == 0 {
		return false, nil
	}

	sortedUpdates := append([]SingleKeyChannelStatusUpdate(nil), updates...)
	sort.Slice(sortedUpdates, func(i, j int) bool {
		if sortedUpdates[i].Expected == nil {
			return false
		}
		if sortedUpdates[j].Expected == nil {
			return true
		}
		return sortedUpdates[i].Expected.Id < sortedUpdates[j].Expected.Id
	})
	channelIDs := make([]int, 0, len(sortedUpdates))
	for index, update := range sortedUpdates {
		if update.Expected == nil || update.Expected.Id == 0 {
			return false, errors.New("single-key channel status snapshot is missing")
		}
		if index > 0 && sortedUpdates[index-1].Expected.Id == update.Expected.Id {
			return false, errors.New("single-key channel status snapshot is duplicated")
		}
		channelIDs = append(channelIDs, update.Expected.Id)
	}

	return withChannelStatusesLocks(channelIDs, func() (bool, error) {
		updatedChannels := make([]Channel, len(sortedUpdates))
		err := DB.Transaction(func(tx *gorm.DB) error {
			for index, update := range sortedUpdates {
				var current Channel
				if err := lockForUpdate(tx).
					Where("id = ?", update.Expected.Id).
					First(&current).Error; err != nil {
					return err
				}
				if current.ChannelInfo.IsMultiKey ||
					current.Key != update.Expected.Key ||
					current.GetTag() != update.Expected.GetTag() ||
					current.Status != update.Expected.Status ||
					current.OtherInfo != update.Expected.OtherInfo {
					return errSingleKeyChannelStatusSnapshotChanged
				}
				updatedChannels[index] = current
			}

			for index, update := range sortedUpdates {
				expected := update.Expected
				channelQuery := tx.Model(&Channel{}).
					Where("id = ?", expected.Id).
					Where("key = ?", expected.Key).
					Where("status = ?", expected.Status).
					Where("other_info = ?", expected.OtherInfo)
				if expected.Tag == nil {
					channelQuery = channelQuery.Where("tag IS NULL")
				} else {
					channelQuery = channelQuery.Where("tag = ?", *expected.Tag)
				}
				result := channelQuery.Updates(map[string]any{
					"status":     update.Status,
					"other_info": update.OtherInfo,
				})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errSingleKeyChannelStatusSnapshotChanged
				}
				if err := tx.Model(&Ability{}).Where("channel_id = ?", expected.Id).
					Select("enabled").Update("enabled", update.Status == common.ChannelStatusEnabled).Error; err != nil {
					return err
				}
				updatedChannels[index].Status = update.Status
				updatedChannels[index].OtherInfo = update.OtherInfo
			}
			return nil
		})
		if errors.Is(err, errSingleKeyChannelStatusSnapshotChanged) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for index := range updatedChannels {
			CacheUpdateChannel(&updatedChannels[index])
		}
		return true, nil
	})
}

type MultiKeyChannelStatusUpdateOptions struct {
	PlanQuotaResetAt       int64
	ClearPlanQuotaDeadline bool
}

// UpdateMultiKeyChannelStatusIfUnchanged applies a request-selected key
// transition only while the complete channel status snapshot and request
// identity still match.
func UpdateMultiKeyChannelStatusIfUnchanged(
	expected *Channel,
	observedTag string,
	usingKey string,
	status int,
	reason string,
	options MultiKeyChannelStatusUpdateOptions,
) (bool, error) {
	if expected == nil || expected.Id == 0 {
		return false, errors.New("multi-key channel snapshot is missing")
	}

	return withChannelStatusLocks(expected.Id, func() (bool, error) {
		changed := false
		var updated Channel
		err := DB.Transaction(func(tx *gorm.DB) error {
			var current Channel
			if err := lockForUpdate(tx).Where("id = ?", expected.Id).First(&current).Error; err != nil {
				return err
			}
			if current.Key != expected.Key ||
				current.GetTag() != expected.GetTag() ||
				current.Status != expected.Status ||
				current.OtherInfo != expected.OtherInfo ||
				!reflect.DeepEqual(current.ChannelInfo, expected.ChannelInfo) ||
				current.GetTag() != observedTag ||
				!current.ChannelInfo.IsMultiKey {
				return nil
			}
			keyFound := false
			for _, key := range current.GetKeys() {
				if key == usingKey {
					keyFound = true
					break
				}
			}
			if !keyFound {
				return nil
			}

			updated = current
			channelInfoJSON, err := common.Marshal(current.ChannelInfo)
			if err != nil {
				return err
			}
			if err := common.Unmarshal(channelInfoJSON, &updated.ChannelInfo); err != nil {
				return err
			}
			handlerMultiKeyUpdate(&updated, usingKey, status, reason)
			if updated.Status == common.ChannelStatusAutoDisabled && options.PlanQuotaResetAt > 0 {
				metadata := updated.GetOtherInfo()
				metadata["quota_reset_at"] = options.PlanQuotaResetAt
				metadata["disabled_until"] = options.PlanQuotaResetAt + 60
				updated.SetOtherInfo(metadata)
			}
			if options.ClearPlanQuotaDeadline {
				metadata := updated.GetOtherInfo()
				delete(metadata, "quota_reset_at")
				delete(metadata, "disabled_until")
				updated.SetOtherInfo(metadata)
			}
			if updated.Status == current.Status &&
				updated.OtherInfo == current.OtherInfo &&
				reflect.DeepEqual(updated.ChannelInfo, current.ChannelInfo) {
				return nil
			}

			if err := tx.Model(&Channel{}).Where("id = ?", current.Id).Updates(map[string]any{
				"status":       updated.Status,
				"other_info":   updated.OtherInfo,
				"channel_info": updated.ChannelInfo,
			}).Error; err != nil {
				return err
			}
			if updated.Status != current.Status {
				if err := tx.Model(&Ability{}).Where("channel_id = ?", current.Id).
					Select("enabled").Update("enabled", updated.Status == common.ChannelStatusEnabled).Error; err != nil {
					return err
				}
			}
			changed = true
			return nil
		})
		if err != nil {
			return false, err
		}
		if changed {
			CacheUpdateChannel(&updated)
		}
		return changed, nil
	})
}

func autoDisabledMultiKeyIndexes(channel *Channel) []int {
	if channel == nil {
		return nil
	}
	indexes := make([]int, 0, len(channel.GetKeys()))
	for index := range channel.GetKeys() {
		if channel.ChannelInfo.MultiKeyStatusList[index] == common.ChannelStatusAutoDisabled {
			indexes = append(indexes, index)
		}
	}
	sort.Slice(indexes, func(i, j int) bool {
		leftTime := channel.ChannelInfo.MultiKeyDisabledTime[indexes[i]]
		rightTime := channel.ChannelInfo.MultiKeyDisabledTime[indexes[j]]
		if leftTime != rightTime {
			return leftTime < rightTime
		}
		return indexes[i] < indexes[j]
	})
	return indexes
}

// AdvanceMultiKeyRecoveryCursorIfUnchanged advances only the passive recovery
// cursor while the complete channel snapshot and selected probe key still match.
func AdvanceMultiKeyRecoveryCursorIfUnchanged(expected *Channel, usingKey string) (bool, error) {
	if expected == nil || expected.Id == 0 {
		return false, errors.New("multi-key channel snapshot is missing")
	}

	return withChannelStatusLocks(expected.Id, func() (bool, error) {
		changed := false
		var updated Channel
		err := DB.Transaction(func(tx *gorm.DB) error {
			var current Channel
			if err := lockForUpdate(tx).Where("id = ?", expected.Id).First(&current).Error; err != nil {
				return err
			}
			if current.Key != expected.Key ||
				current.GetTag() != expected.GetTag() ||
				current.Status != expected.Status ||
				current.OtherInfo != expected.OtherInfo ||
				!reflect.DeepEqual(current.ChannelInfo, expected.ChannelInfo) ||
				!current.ChannelInfo.IsMultiKey {
				return nil
			}

			indexes := autoDisabledMultiKeyIndexes(&current)
			if len(indexes) == 0 {
				return nil
			}
			cursor := current.ChannelInfo.MultiKeyRecoveryIndex
			if cursor < 0 {
				cursor = 0
			}
			selectedPosition := cursor % len(indexes)
			keys := current.GetKeys()
			if keys[indexes[selectedPosition]] != usingKey {
				return nil
			}

			updated = current
			channelInfoJSON, err := common.Marshal(current.ChannelInfo)
			if err != nil {
				return err
			}
			if err := common.Unmarshal(channelInfoJSON, &updated.ChannelInfo); err != nil {
				return err
			}
			updated.ChannelInfo.MultiKeyRecoveryIndex = (selectedPosition + 1) % len(indexes)
			if updated.ChannelInfo.MultiKeyRecoveryIndex == current.ChannelInfo.MultiKeyRecoveryIndex {
				return nil
			}
			if err := tx.Model(&Channel{}).Where("id = ?", current.Id).
				Update("channel_info", updated.ChannelInfo).Error; err != nil {
				return err
			}
			changed = true
			return nil
		})
		if err != nil {
			return false, err
		}
		if changed {
			CacheUpdateChannel(&updated)
		}
		return changed, nil
	})
}

type ManagedChannelUpdate struct {
	RouteID               int64
	ExpectedRouteState    string
	ExpectedRouteDetached bool
	Rank                  int
	EffectiveMultiplier   float64
	UpdatedAt             int64
	Priority              int64
	BaseURL               string
	Models                string
	Status                int
}

func managedChannelSnapshotMatches(current *Channel, expected *Channel) bool {
	if current == nil || expected == nil {
		return false
	}
	tagsMatch := current.Tag == nil && expected.Tag == nil
	if current.Tag != nil && expected.Tag != nil {
		tagsMatch = *current.Tag == *expected.Tag
	}
	return current.Key == expected.Key &&
		tagsMatch &&
		current.Status == expected.Status &&
		current.OtherInfo == expected.OtherInfo &&
		current.Models == expected.Models &&
		reflect.DeepEqual(current.Priority, expected.Priority) &&
		reflect.DeepEqual(current.BaseURL, expected.BaseURL)
}

// UpdateManagedChannelIfUnchanged commits managed rank, channel, and ability
// state only while the channel still matches the reconciliation snapshot.
func UpdateManagedChannelIfUnchanged(expected *Channel, update ManagedChannelUpdate) (bool, error) {
	if expected == nil || expected.Id == 0 {
		return false, errors.New("managed channel snapshot is missing")
	}

	return withChannelStatusLocks(expected.Id, func() (bool, error) {
		changed := false
		var updated Channel
		err := DB.Transaction(func(tx *gorm.DB) error {
			var currentRoute UpstreamManagedRoute
			if err := lockForUpdate(tx).Where("id = ?", update.RouteID).First(&currentRoute).Error; err != nil {
				return err
			}
			if currentRoute.ChannelID != expected.Id ||
				currentRoute.State != update.ExpectedRouteState ||
				currentRoute.Detached != update.ExpectedRouteDetached {
				return nil
			}
			if update.Status == common.ChannelStatusEnabled &&
				(currentRoute.Detached || currentRoute.State != UpstreamRouteStateActive) {
				return nil
			}

			var current Channel
			if err := lockForUpdate(tx).Where("id = ?", expected.Id).First(&current).Error; err != nil {
				return err
			}
			if !managedChannelSnapshotMatches(&current, expected) {
				return nil
			}

			channelQuery := tx.Model(&Channel{}).Where(map[string]any{
				"id":         expected.Id,
				"key":        expected.Key,
				"status":     expected.Status,
				"other_info": expected.OtherInfo,
				"models":     expected.Models,
			})
			if expected.Tag == nil {
				channelQuery = channelQuery.Where("tag IS NULL")
			} else {
				channelQuery = channelQuery.Where(map[string]any{"tag": *expected.Tag})
			}
			if expected.Priority == nil {
				channelQuery = channelQuery.Where("priority IS NULL")
			} else {
				channelQuery = channelQuery.Where("priority = ?", *expected.Priority)
			}
			if expected.BaseURL == nil {
				channelQuery = channelQuery.Where("base_url IS NULL")
			} else {
				channelQuery = channelQuery.Where("base_url = ?", *expected.BaseURL)
			}
			result := channelQuery.Updates(map[string]any{
				"priority": update.Priority,
				"base_url": update.BaseURL,
				"models":   update.Models,
				"status":   update.Status,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				var latest Channel
				if err := tx.Where("id = ?", expected.Id).First(&latest).Error; err != nil {
					return err
				}
				if !managedChannelSnapshotMatches(&latest, expected) {
					return nil
				}
			}

			if err := tx.Model(&UpstreamManagedRoute{}).Where("id = ?", currentRoute.ID).Updates(map[string]any{
				"rank":                 update.Rank,
				"effective_multiplier": update.EffectiveMultiplier,
				"updated_at":           update.UpdatedAt,
			}).Error; err != nil {
				return err
			}

			current.Priority = &update.Priority
			current.BaseURL = &update.BaseURL
			current.Models = update.Models
			current.Status = update.Status
			if err := current.UpdateAbilities(tx); err != nil {
				return err
			}
			updated = current
			changed = true
			return nil
		})
		if err != nil {
			return false, err
		}
		if changed {
			CacheUpdateChannel(&updated)
		}
		return changed, nil
	})
}

func MergeChannelStatusMetadata(channelId int, updates map[string]interface{}) error {
	channel, err := GetChannelById(channelId, true)
	if err != nil {
		return err
	}
	info := channel.GetOtherInfo()
	for key, value := range updates {
		if value == nil {
			delete(info, key)
			continue
		}
		info[key] = value
	}
	channel.SetOtherInfo(info)
	return DB.Model(&Channel{}).Where("id = ?", channelId).
		Update("other_info", channel.OtherInfo).Error
}

func EnableChannelByTag(tag string) error {
	err := DB.Model(&Channel{}).Where("tag = ?", tag).Update("status", common.ChannelStatusEnabled).Error
	if err != nil {
		return err
	}
	err = UpdateAbilityStatusByTag(tag, true)
	return err
}

func DisableChannelByTag(tag string) error {
	err := DB.Model(&Channel{}).Where("tag = ?", tag).Update("status", common.ChannelStatusManuallyDisabled).Error
	if err != nil {
		return err
	}
	err = UpdateAbilityStatusByTag(tag, false)
	return err
}

func EditChannelByTag(tag string, newTag *string, modelMapping *string, models *string, group *string, priority *int64, weight *uint, paramOverride *string, headerOverride *string) error {
	updateData := Channel{}
	shouldReCreateAbilities := false
	updatedTag := tag
	// 如果 newTag 不为空且不等于 tag，则更新 tag
	if newTag != nil && *newTag != tag {
		updateData.Tag = newTag
		updatedTag = *newTag
	}
	if modelMapping != nil {
		updateData.ModelMapping = modelMapping
	}
	if models != nil && *models != "" {
		shouldReCreateAbilities = true
		updateData.Models = *models
	}
	if group != nil && *group != "" {
		shouldReCreateAbilities = true
		updateData.Group = *group
	}
	if priority != nil {
		updateData.Priority = priority
	}
	if weight != nil {
		updateData.Weight = weight
	}
	if paramOverride != nil {
		updateData.ParamOverride = paramOverride
	}
	if headerOverride != nil {
		updateData.HeaderOverride = headerOverride
	}

	err := DB.Model(&Channel{}).Where("tag = ?", tag).Updates(updateData).Error
	if err != nil {
		return err
	}
	if shouldReCreateAbilities {
		channels, err := GetChannelsByTag(updatedTag, false, false)
		if err == nil {
			for _, channel := range channels {
				err = channel.UpdateAbilities(nil)
				if err != nil {
					common.SysLog(fmt.Sprintf("failed to update abilities: channel_id=%d, tag=%s, error=%v", channel.Id, channel.GetTag(), err))
				}
			}
		}
	} else {
		err := UpdateAbilityByTag(tag, newTag, priority, weight)
		if err != nil {
			return err
		}
	}
	return nil
}

func UpdateChannelUsedQuota(id int, quota int) {
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeChannelUsedQuota, id, quota)
		return
	}
	updateChannelUsedQuota(id, quota)
}

func updateChannelUsedQuota(id int, quota int) {
	err := DB.Model(&Channel{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", quota)).Error
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to update channel used quota: channel_id=%d, delta_quota=%d, error=%v", id, quota, err))
	}
}

func DeleteChannelByStatus(status int64) (int64, error) {
	result := DB.Where("status = ?", status).Delete(&Channel{})
	return result.RowsAffected, result.Error
}

func DeleteDisabledChannel() (int64, error) {
	result := DB.Where("status = ? or status = ?", common.ChannelStatusAutoDisabled, common.ChannelStatusManuallyDisabled).Delete(&Channel{})
	return result.RowsAffected, result.Error
}

func GetPaginatedTags(offset int, limit int) ([]*string, error) {
	return GetPaginatedChannelTags(DB.Model(&Channel{}), offset, limit)
}

func GetPaginatedChannelTags(query *gorm.DB, offset int, limit int) ([]*string, error) {
	var tags []*string
	err := query.
		Select("DISTINCT tag").
		Where("tag is not null AND tag != ''").
		Order(clause.OrderByColumn{Column: clause.Column{Name: "tag"}}).
		Offset(offset).
		Limit(limit).
		Find(&tags).Error
	return tags, err
}

func SearchTags(keyword string, group string, model string, idSort bool) ([]*string, error) {
	var tags []*string
	modelsCol := "`models`"

	// 如果是 PostgreSQL，使用双引号
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		modelsCol = `"models"`
	}

	baseURLCol := "`base_url`"
	// 如果是 PostgreSQL，使用双引号
	if common.UsingMainDatabase(common.DatabaseTypePostgreSQL) {
		baseURLCol = `"base_url"`
	}

	order := "priority desc"
	if idSort {
		order = "id desc"
	}

	// 构造基础查询
	baseQuery := DB.Model(&Channel{}).Omit("key")

	// 构造WHERE子句
	whereClause := "(id = ? OR name LIKE ? OR " + commonKeyCol + " = ? OR " + baseURLCol + " LIKE ?) AND " + modelsCol + " LIKE ?"
	args := []any{common.String2Int(keyword), "%" + keyword + "%", keyword, "%" + keyword + "%", "%" + model + "%"}
	baseQuery = ApplyChannelGroupFilter(baseQuery.Where(whereClause, args...), group)

	subQuery := baseQuery.
		Select("tag").
		Where("tag != ''").
		Order(order)

	err := DB.Table("(?) as sub", subQuery).
		Select("DISTINCT tag").
		Find(&tags).Error

	if err != nil {
		return nil, err
	}

	return tags, nil
}

func (channel *Channel) ValidateSettings() error {
	channelParams := &dto.ChannelSettings{}
	if channel.Setting != nil && *channel.Setting != "" {
		err := common.Unmarshal([]byte(*channel.Setting), channelParams)
		if err != nil {
			return err
		}
	}
	if _, err := common.ParseProxyURLStrict(channelParams.Proxy); err != nil {
		return fmt.Errorf("invalid channel proxy: %w", err)
	}
	if err := channelParams.ValidateHTTPTransport(); err != nil {
		return err
	}
	channelOtherSettings := &dto.ChannelOtherSettings{}
	if channel.OtherSettings != "" {
		err := common.UnmarshalJsonStr(channel.OtherSettings, channelOtherSettings)
		if err != nil {
			return err
		}
	}
	if err := channelOtherSettings.ValidateRoutingAccount(); err != nil {
		return err
	}
	if channel.Type == constant.ChannelTypeAdvancedCustom {
		if channelOtherSettings.AdvancedCustom == nil {
			return fmt.Errorf("advanced_custom is required")
		}
	}
	if channelOtherSettings.AdvancedCustom != nil {
		if err := channelOtherSettings.AdvancedCustom.Validate(); err != nil {
			return err
		}
	}
	if channel.Type == constant.ChannelTypeAdvancedCustom && channelOtherSettings.UpstreamModelUpdateCheckEnabled {
		if _, ok := channelOtherSettings.AdvancedCustom.ModelListRoute(); !ok {
			return fmt.Errorf("advanced custom channels require a %s route when upstream model update checks are enabled", dto.AdvancedCustomModelListPath)
		}
	}
	return nil
}

func (channel *Channel) GetSetting() dto.ChannelSettings {
	setting := dto.ChannelSettings{}
	if channel.Setting != nil && *channel.Setting != "" {
		err := common.Unmarshal([]byte(*channel.Setting), &setting)
		if err != nil {
			common.SysLog(fmt.Sprintf("failed to unmarshal setting: channel_id=%d, error=%v", channel.Id, err))
			channel.Setting = nil // 清空设置以避免后续错误
			_ = channel.Save()    // 保存修改
		}
	}
	return setting
}

func (channel *Channel) SetSetting(setting dto.ChannelSettings) {
	settingBytes, err := common.Marshal(setting)
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to marshal setting: channel_id=%d, error=%v", channel.Id, err))
		return
	}
	channel.Setting = common.GetPointer[string](string(settingBytes))
}

func (channel *Channel) GetOtherSettings() dto.ChannelOtherSettings {
	setting := dto.ChannelOtherSettings{}
	if channel.OtherSettings != "" {
		err := common.UnmarshalJsonStr(channel.OtherSettings, &setting)
		if err != nil {
			common.SysLog(fmt.Sprintf("failed to unmarshal setting: channel_id=%d, error=%v", channel.Id, err))
			channel.OtherSettings = "{}" // 清空设置以避免后续错误
			_ = channel.Save()           // 保存修改
		}
	}
	return setting
}

func (channel *Channel) SetOtherSettings(setting dto.ChannelOtherSettings) {
	settingBytes, err := common.Marshal(setting)
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to marshal setting: channel_id=%d, error=%v", channel.Id, err))
		return
	}
	channel.OtherSettings = string(settingBytes)
}

func (channel *Channel) GetParamOverride() map[string]interface{} {
	paramOverride := make(map[string]interface{})
	if channel.ParamOverride != nil && *channel.ParamOverride != "" {
		err := common.Unmarshal([]byte(*channel.ParamOverride), &paramOverride)
		if err != nil {
			common.SysLog(fmt.Sprintf("failed to unmarshal param override: channel_id=%d, error=%v", channel.Id, err))
		}
	}
	return paramOverride
}

func (channel *Channel) GetHeaderOverride() map[string]interface{} {
	headerOverride := make(map[string]interface{})
	if channel.HeaderOverride != nil && *channel.HeaderOverride != "" {
		err := common.Unmarshal([]byte(*channel.HeaderOverride), &headerOverride)
		if err != nil {
			common.SysLog(fmt.Sprintf("failed to unmarshal header override: channel_id=%d, error=%v", channel.Id, err))
		}
	}
	return headerOverride
}

func GetChannelsByIds(ids []int) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("id in (?)", ids).Find(&channels).Error
	return channels, err
}

func BatchSetChannelTag(ids []int, tag *string) error {
	// 开启事务
	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// 更新标签
	err := tx.Model(&Channel{}).Where("id in (?)", ids).Update("tag", tag).Error
	if err != nil {
		tx.Rollback()
		return err
	}

	// update ability status
	channels, err := GetChannelsByIds(ids)
	if err != nil {
		tx.Rollback()
		return err
	}

	for _, channel := range channels {
		err = channel.UpdateAbilities(tx)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	// 提交事务
	return tx.Commit().Error
}

// CountAllChannels returns total channels in DB
func CountAllChannels() (int64, error) {
	var total int64
	err := DB.Model(&Channel{}).Count(&total).Error
	return total, err
}

// CountAllTags returns number of non-empty distinct tags
func CountAllTags() (int64, error) {
	return CountChannelTags(DB.Model(&Channel{}))
}

func CountChannelTags(query *gorm.DB) (int64, error) {
	var total int64
	err := query.Where("tag is not null AND tag != ''").Distinct("tag").Count(&total).Error
	return total, err
}

// Get channels of specified type with pagination
func GetChannelsByType(startIdx int, num int, idSort bool, channelType int) ([]*Channel, error) {
	var channels []*Channel
	order := "priority desc"
	if idSort {
		order = "id desc"
	}
	err := DB.Where("type = ?", channelType).Order(order).Limit(num).Offset(startIdx).Omit("key").Find(&channels).Error
	return channels, err
}

// Count channels of specific type
func CountChannelsByType(channelType int) (int64, error) {
	var count int64
	err := DB.Model(&Channel{}).Where("type = ?", channelType).Count(&count).Error
	return count, err
}

// Return map[type]count for all channels
func CountChannelsGroupByType() (map[int64]int64, error) {
	type result struct {
		Type  int64 `gorm:"column:type"`
		Count int64 `gorm:"column:count"`
	}
	var results []result
	err := DB.Model(&Channel{}).Select("type, count(*) as count").Group("type").Find(&results).Error
	if err != nil {
		return nil, err
	}
	counts := make(map[int64]int64)
	for _, r := range results {
		counts[r.Type] = r.Count
	}
	return counts, nil
}
