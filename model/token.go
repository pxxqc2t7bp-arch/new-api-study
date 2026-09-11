package model

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
)

type Token struct {
	Id                       int            `json:"id"`
	UserId                   int            `json:"user_id" gorm:"index"`
	Key                      string         `json:"key" gorm:"type:varchar(128);uniqueIndex"`
	Status                   int            `json:"status" gorm:"default:1"`
	Name                     string         `json:"name" gorm:"index" `
	CreatedTime              int64          `json:"created_time" gorm:"bigint"`
	AccessedTime             int64          `json:"accessed_time" gorm:"bigint"`
	ExpiredTime              int64          `json:"expired_time" gorm:"bigint;default:-1"` // -1 means never expired
	RemainQuota              int            `json:"remain_quota" gorm:"default:0"`
	UnlimitedQuota           bool           `json:"unlimited_quota"`
	ModelLimitsEnabled       bool           `json:"model_limits_enabled"`
	ModelLimits              string         `json:"model_limits" gorm:"type:text"`
	AllowIps                 *string        `json:"allow_ips" gorm:"default:''"`
	StreamRecoveryEnabled    bool           `json:"stream_recovery_enabled"`
	UsedQuota                int            `json:"used_quota" gorm:"default:0"` // used quota
	Group                    string         `json:"group" gorm:"default:''"`
	CrossGroupRetry          bool           `json:"cross_group_retry"` // 跨分组重试，仅auto分组有效
	AutoGroups               string         `json:"-" gorm:"type:text"`
	DefaultRoutingStrategy   string         `json:"default_routing_strategy" gorm:"type:varchar(16)"`
	AllowedRoutingStrategies string         `json:"-" gorm:"type:text"`
	DefaultConversionPolicy  string         `json:"default_conversion_policy" gorm:"type:varchar(16)"`
	AllowLossyConversion     bool           `json:"allow_lossy_conversion"`
	CacheGeneration          int64          `json:"-" gorm:"bigint;default:0"`
	DeletedAt                gorm.DeletedAt `gorm:"index"`
}

func (token *Token) GetAutoGroups() ([]string, error) {
	if token.AutoGroups == "" {
		return nil, nil
	}
	var groups []string
	if err := common.UnmarshalJsonStr(token.AutoGroups, &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

func (token *Token) SetAutoGroups(groups []string) error {
	if len(groups) == 0 {
		token.AutoGroups = ""
		return nil
	}
	data, err := common.Marshal(groups)
	if err != nil {
		return err
	}
	token.AutoGroups = string(data)
	return nil
}

func (token *Token) GetAllowedRoutingStrategies() ([]string, error) {
	if token.AllowedRoutingStrategies == "" {
		return []string{string(hosttypes.RoutingStrategyStable)}, nil
	}
	var values []string
	if err := common.UnmarshalJsonStr(token.AllowedRoutingStrategies, &values); err != nil {
		return nil, err
	}
	return normalizeRoutingStrategies(values)
}

func (token *Token) SetAllowedRoutingStrategies(values []string) error {
	normalized, err := normalizeRoutingStrategies(values)
	if err != nil {
		return err
	}
	data, err := common.Marshal(normalized)
	if err != nil {
		return err
	}
	token.AllowedRoutingStrategies = string(data)
	return nil
}

func (token *Token) NormalizeRequestPolicySettings() error {
	defaultRouting := strings.TrimSpace(token.DefaultRoutingStrategy)
	if defaultRouting == "" {
		defaultRouting = string(hosttypes.RoutingStrategyStable)
	}
	routing, ok := hosttypes.ParseRoutingStrategy(defaultRouting)
	if !ok {
		return fmt.Errorf("unsupported default routing strategy %q", defaultRouting)
	}

	allowed, err := token.GetAllowedRoutingStrategies()
	if err != nil {
		return fmt.Errorf("invalid allowed routing strategies: %w", err)
	}
	if !slices.Contains(allowed, string(routing)) {
		return fmt.Errorf("default routing strategy %q is not authorized", routing)
	}

	conversion := relaytypes.ConversionLossPolicy(strings.TrimSpace(token.DefaultConversionPolicy))
	if conversion == "" {
		conversion = relaytypes.ConversionLossPolicyStrict
	}
	switch conversion {
	case relaytypes.ConversionLossPolicyStrict:
	case relaytypes.ConversionLossPolicySafe, relaytypes.ConversionLossPolicyAllow:
		if !token.AllowLossyConversion {
			return fmt.Errorf("default conversion policy %q requires lossy conversion authorization", conversion)
		}
	default:
		return fmt.Errorf("unsupported default conversion policy %q", conversion)
	}

	token.DefaultRoutingStrategy = string(routing)
	if err := token.SetAllowedRoutingStrategies(allowed); err != nil {
		return err
	}
	token.DefaultConversionPolicy = string(conversion)
	return nil
}

func normalizeRoutingStrategies(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{string(hosttypes.RoutingStrategyStable)}, nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[hosttypes.RoutingStrategy]struct{}, len(values))
	for _, value := range values {
		strategy, ok := hosttypes.ParseRoutingStrategy(value)
		if !ok {
			return nil, fmt.Errorf("unsupported routing strategy %q", strings.TrimSpace(value))
		}
		if _, exists := seen[strategy]; exists {
			return nil, fmt.Errorf("duplicate routing strategy %q", strategy)
		}
		seen[strategy] = struct{}{}
		normalized = append(normalized, string(strategy))
	}
	return normalized, nil
}

func (token *Token) Clean() {
	token.Key = ""
}

func MaskTokenKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return strings.Repeat("*", len(key))
	}
	if len(key) <= 8 {
		return key[:2] + "****" + key[len(key)-2:]
	}
	return key[:4] + "**********" + key[len(key)-4:]
}

func (token *Token) GetFullKey() string {
	return token.Key
}

func (token *Token) GetMaskedKey() string {
	return MaskTokenKey(token.Key)
}

func (token *Token) GetIpLimits() []string {
	// delete empty spaces
	//split with \n
	ipLimits := make([]string, 0)
	if token.AllowIps == nil {
		return ipLimits
	}
	cleanIps := strings.ReplaceAll(*token.AllowIps, " ", "")
	if cleanIps == "" {
		return ipLimits
	}
	ips := strings.SplitSeq(cleanIps, "\n")
	for ip := range ips {
		ip = strings.TrimSpace(ip)
		ip = strings.ReplaceAll(ip, ",", "")
		if ip != "" {
			ipLimits = append(ipLimits, ip)
		}
	}
	return ipLimits
}

func GetAllUserTokens(userId int, startIdx int, num int) ([]*Token, error) {
	var tokens []*Token
	var err error
	err = DB.Where("user_id = ?", userId).Order("id desc").Limit(num).Offset(startIdx).Find(&tokens).Error
	return tokens, err
}

// sanitizeLikePattern 校验并清洗用户输入的 LIKE 搜索模式。
// 规则：
//  1. 转义 ! 和 _（使用 ! 作为 ESCAPE 字符，兼容 MySQL/PostgreSQL/SQLite）
//  2. 连续的 % 合并为单个 %
//  3. 最多允许 2 个 %
//  4. 含 % 时（模糊搜索），去掉 % 后关键词长度必须 >= 2
//  5. 不含 % 时按精确匹配
func sanitizeLikePattern(input string) (string, error) {
	// 1. 先转义 ESCAPE 字符 ! 自身，再转义 _
	//    使用 ! 而非 \ 作为 ESCAPE 字符，避免 MySQL 中反斜杠的字符串转义问题
	input = strings.ReplaceAll(input, "!", "!!")
	input = strings.ReplaceAll(input, `_`, `!_`)

	if err := validateLikePattern(input); err != nil {
		return "", err
	}

	// 5. 无 % 时，精确全匹配
	return input, nil
}

func validateLikePattern(input string) error {
	// 1. 连续的 % 直接拒绝
	if strings.Contains(input, "%%") {
		return errors.New("搜索模式中不允许包含连续的 % 通配符")
	}

	// 2. 统计 % 数量，不得超过 2
	count := strings.Count(input, "%")
	if count > 2 {
		return errors.New("搜索模式中最多允许包含 2 个 % 通配符")
	}

	// 3. 含 % 时，去掉 % 后关键词长度必须 >= 2
	if count > 0 {
		stripped := strings.ReplaceAll(input, "%", "")
		if len(stripped) < 2 {
			return errors.New("使用模糊搜索时，关键词长度至少为 2 个字符")
		}
	}

	return nil
}

const searchHardLimit = 100

func SearchUserTokens(userId int, keyword string, token string, offset int, limit int) (tokens []*Token, total int64, err error) {
	// model 层强制截断
	if limit <= 0 || limit > searchHardLimit {
		limit = searchHardLimit
	}
	if offset < 0 {
		offset = 0
	}

	if token != "" {
		token = strings.TrimPrefix(token, "sk-")
	}

	// 超量用户（令牌数超过上限）只允许精确搜索，禁止模糊搜索
	maxTokens := operation_setting.GetMaxUserTokens()
	hasFuzzy := strings.Contains(keyword, "%") || strings.Contains(token, "%")
	if hasFuzzy {
		count, err := CountUserTokens(userId)
		if err != nil {
			common.SysLog("failed to count user tokens: " + err.Error())
			return nil, 0, errors.New("获取令牌数量失败")
		}
		if int(count) > maxTokens {
			return nil, 0, errors.New("令牌数量超过上限，仅允许精确搜索，请勿使用 % 通配符")
		}
	}

	baseQuery := DB.Model(&Token{}).Where("user_id = ?", userId)

	// 非空才加 LIKE 条件，空则跳过（不过滤该字段）
	if keyword != "" {
		keywordPattern, err := sanitizeLikePattern(keyword)
		if err != nil {
			return nil, 0, err
		}
		baseQuery = baseQuery.Where("name LIKE ? ESCAPE '!'", keywordPattern)
	}
	if token != "" {
		tokenPattern, err := sanitizeLikePattern(token)
		if err != nil {
			return nil, 0, err
		}
		baseQuery = baseQuery.Where(commonKeyCol+" LIKE ? ESCAPE '!'", tokenPattern)
	}

	// 先查匹配总数（用于分页，受 maxTokens 上限保护，避免全表 COUNT）
	err = baseQuery.Limit(maxTokens).Count(&total).Error
	if err != nil {
		common.SysError("failed to count search tokens: " + err.Error())
		return nil, 0, errors.New("搜索令牌失败")
	}

	// 再分页查数据
	err = baseQuery.Order("id desc").Offset(offset).Limit(limit).Find(&tokens).Error
	if err != nil {
		common.SysError("failed to search tokens: " + err.Error())
		return nil, 0, errors.New("搜索令牌失败")
	}
	return tokens, total, nil
}

func ValidateUserToken(key string) (token *Token, err error) {
	if key == "" {
		return nil, ErrTokenNotProvided
	}
	token, err = GetTokenByKey(key, false)
	if err == nil {
		if token.Status == common.TokenStatusExhausted ||
			token.Status == common.TokenStatusExpired ||
			token.Status != common.TokenStatusEnabled {
			return token, ErrTokenInvalid
		}
		if token.ExpiredTime != -1 && token.ExpiredTime < common.GetTimestamp() {
			if !common.RedisEnabled {
				token.Status = common.TokenStatusExpired
				err := token.SelectUpdate()
				if err != nil {
					common.SysLog("failed to update token status" + err.Error())
				}
			}
			return token, ErrTokenInvalid
		}
		if !token.UnlimitedQuota && token.RemainQuota <= 0 {
			if !common.RedisEnabled {
				token.Status = common.TokenStatusExhausted
				err := token.SelectUpdate()
				if err != nil {
					common.SysLog("failed to update token status" + err.Error())
				}
			}
			return token, ErrTokenInvalid
		}
		return token, nil
	}
	common.SysLog("ValidateUserToken: failed to get token: " + err.Error())
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTokenInvalid
	}
	return nil, fmt.Errorf("%w: %v", ErrDatabase, err)
}

func GetTokenByIds(id int, userId int) (*Token, error) {
	if id == 0 || userId == 0 {
		return nil, errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	var err error = nil
	err = DB.First(&token, "id = ? and user_id = ?", id, userId).Error
	return &token, err
}

func GetTokenById(id int) (*Token, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	token := Token{Id: id}
	var err error = nil
	err = DB.First(&token, "id = ?", id).Error
	return &token, err
}

func getTokenByKeyFromDB(key string) (*Token, error) {
	token := &Token{}
	if err := DB.Where(&Token{Key: key}).First(token).Error; err != nil {
		return nil, err
	}
	return token, nil
}

func getTokenByKeyAcrossGeneration(key string) (*Token, error) {
	for range 3 {
		state, err := loadTokenCacheGenerationState(key)
		if err != nil {
			return nil, err
		}
		expectedGeneration := state.generation
		loaded, readErr := getTokenByKeyFromDB(key)
		if state.pendingMarker {
			return nil, errTokenCacheMutationPending
		}
		if expectedGeneration%2 != 0 {
			switch {
			case errors.Is(readErr, gorm.ErrRecordNotFound):
				if finishErr := commitTokenCacheDeleteMutation(key, expectedGeneration); finishErr != nil {
					return nil, errTokenCacheMutationPending
				}
				continue
			case readErr != nil:
				return nil, readErr
			case loaded.CacheGeneration == expectedGeneration+1:
				if finishErr := commitTokenCacheMutation(*loaded, expectedGeneration); finishErr != nil {
					return nil, errTokenCacheMutationPending
				}
				continue
			default:
				return nil, errTokenCacheMutationPending
			}
		}
		if readErr != nil {
			return nil, readErr
		}
		if loaded.CacheGeneration > expectedGeneration && loaded.CacheGeneration%2 == 0 {
			restored, restoreErr := restoreTokenCacheGenerationForColdCache(key, loaded.CacheGeneration)
			if restoreErr != nil || !restored {
				return nil, errTokenCacheMutationPending
			}
			continue
		}
		code, cacheErr := cacheInitToken(*loaded, expectedGeneration)
		if cacheErr != nil {
			return nil, fmt.Errorf("failed to init token cache: %w", cacheErr)
		}
		if code == 1 {
			return loaded, nil
		}
		if code == 2 {
			return cacheGetTokenByKey(key)
		}
	}
	return nil, errTokenCacheMutationPending
}

func GetTokenByKey(key string, fromDB bool) (token *Token, err error) {
	if !fromDB && common.RedisEnabled {
		token, err := cacheGetTokenByKey(key)
		if err == nil {
			return token, nil
		}
		if errors.Is(err, errTokenCacheMutationPending) {
			return getTokenByKeyAcrossGeneration(key)
		}
	}
	if common.RedisEnabled {
		return getTokenByKeyAcrossGeneration(key)
	}
	return getTokenByKeyFromDB(key)
}

func (token *Token) Insert() error {
	var err error
	err = DB.Create(token).Error
	return err
}

var ErrTokenMutationCommitted = errors.New("token database mutation committed but cache synchronization failed")

var commitTokenMutationTransaction = func(tx *gorm.DB) error {
	return tx.Commit().Error
}

type TokenMutationCommittedError struct {
	Count int
	Cause error
}

func (err *TokenMutationCommittedError) Error() string {
	return fmt.Sprintf("%s (committed_count=%d): %v", ErrTokenMutationCommitted, err.Count, err.Cause)
}

func (err *TokenMutationCommittedError) Unwrap() error {
	return err.Cause
}

func (*TokenMutationCommittedError) Is(target error) bool {
	return target == ErrTokenMutationCommitted
}

func (err *TokenMutationCommittedError) CommittedCount() int {
	return err.Count
}

func reconcileTokenMutationCommitError(token *Token, expected *Token, deleteCache bool, generation int64, commitErr error) error {
	stored, readErr := getTokenByKeyFromDB(token.Key)
	if deleteCache && errors.Is(readErr, gorm.ErrRecordNotFound) {
		if generation == 0 {
			return nil
		}
		finalizeErr := commitTokenCacheDeleteMutation(token.Key, generation)
		return &TokenMutationCommittedError{
			Count: 1,
			Cause: errors.Join(commitErr, finalizeErr),
		}
	}
	if readErr == nil && !deleteCache && expected != nil && reflect.DeepEqual(stored, expected) {
		*token = *stored
		if generation == 0 {
			return nil
		}
		finalizeErr := commitTokenCacheMutation(*stored, generation)
		return &TokenMutationCommittedError{
			Count: 1,
			Cause: errors.Join(commitErr, finalizeErr),
		}
	}
	if readErr != nil && !errors.Is(readErr, gorm.ErrRecordNotFound) {
		return errors.Join(commitErr, fmt.Errorf("failed to reconcile token database mutation: %w", readErr))
	}
	return errors.Join(commitErr, errors.New("token database mutation outcome is uncertain"))
}

func mutateTokenMetadata(token *Token, deleteCache bool, mutation func(*gorm.DB, int64) error) error {
	minimumGeneration := token.CacheGeneration
	generation, beginErr := beginTokenCacheMutation(token.Key, minimumGeneration)
	if beginErr != nil {
		return beginErr
	}
	committedGeneration := minimumGeneration
	if generation > 0 {
		committedGeneration = generation + 1
	}
	tx := DB.Begin()
	if tx.Error != nil {
		token.CacheGeneration = minimumGeneration
		if rollbackErr := rollbackTokenCacheMutation(token.Key, generation); rollbackErr != nil {
			return errors.Join(tx.Error, fmt.Errorf("failed to roll back token cache fence: %w", rollbackErr))
		}
		return tx.Error
	}
	if mutationErr := mutation(tx, committedGeneration); mutationErr != nil {
		if rollbackErr := tx.Rollback().Error; rollbackErr != nil {
			return errors.Join(mutationErr, fmt.Errorf("failed to roll back token database transaction: %w", rollbackErr))
		}
		token.CacheGeneration = minimumGeneration
		if rollbackErr := rollbackTokenCacheMutation(token.Key, generation); rollbackErr != nil {
			return errors.Join(mutationErr, fmt.Errorf("failed to roll back token cache fence: %w", rollbackErr))
		}
		return mutationErr
	}
	var expected *Token
	if !deleteCache {
		expected = &Token{}
		if readErr := tx.Where(&Token{Key: token.Key}).First(expected).Error; readErr != nil {
			if rollbackErr := tx.Rollback().Error; rollbackErr != nil {
				return errors.Join(readErr, fmt.Errorf("failed to roll back token database transaction: %w", rollbackErr))
			}
			token.CacheGeneration = minimumGeneration
			if rollbackErr := rollbackTokenCacheMutation(token.Key, generation); rollbackErr != nil {
				return errors.Join(readErr, fmt.Errorf("failed to roll back token cache fence: %w", rollbackErr))
			}
			return readErr
		}
	}
	if commitErr := commitTokenMutationTransaction(tx); commitErr != nil {
		return reconcileTokenMutationCommitError(token, expected, deleteCache, generation, commitErr)
	}
	if generation == 0 {
		if expected != nil {
			*token = *expected
		}
		return nil
	}
	if deleteCache {
		if finalizeErr := commitTokenCacheDeleteMutation(token.Key, generation); finalizeErr != nil {
			return &TokenMutationCommittedError{Count: 1, Cause: finalizeErr}
		}
		return nil
	}

	stored, err := getTokenByKeyFromDB(token.Key)
	if err != nil {
		return &TokenMutationCommittedError{
			Count: 1,
			Cause: fmt.Errorf("failed to reload committed token metadata: %w", err),
		}
	}
	if stored.Id != token.Id || stored.CacheGeneration != committedGeneration {
		return &TokenMutationCommittedError{
			Count: 1,
			Cause: errors.New("committed token cache generation could not be verified"),
		}
	}
	*token = *stored
	if err := commitTokenCacheMutation(*stored, generation); err != nil {
		return &TokenMutationCommittedError{Count: 1, Cause: err}
	}
	return nil
}

// Update Make sure your token's fields is completed, because this will update non-zero values
func (token *Token) Update() (err error) {
	return token.update(nil)
}

// UpdateWithQuotaDelta applies an administrator-requested quota adjustment
// relative to the snapshot they edited, preserving quota writes that committed
// after that snapshot was read.
func (token *Token) UpdateWithQuotaDelta(quotaDelta int64) error {
	return token.update(&quotaDelta)
}

func (token *Token) update(quotaDelta *int64) error {
	return mutateTokenMetadata(token, false, func(tx *gorm.DB, committedGeneration int64) error {
		token.CacheGeneration = committedGeneration
		fields := []string{"name", "status", "expired_time", "unlimited_quota",
			"model_limits_enabled", "model_limits", "allow_ips", "stream_recovery_enabled", "group",
			"cross_group_retry", "auto_groups", "default_routing_strategy", "allowed_routing_strategies",
			"default_conversion_policy", "allow_lossy_conversion", "cache_generation"}
		if quotaDelta == nil {
			fields = append(fields, "remain_quota")
		}
		if err := tx.Model(token).Select(fields).Updates(token).Error; err != nil {
			return err
		}
		if quotaDelta != nil && *quotaDelta != 0 {
			return tx.Model(token).UpdateColumn("remain_quota", gorm.Expr("remain_quota + ?", *quotaDelta)).Error
		}
		return nil
	})
}

func (token *Token) SelectUpdate() (err error) {
	return mutateTokenMetadata(token, false, func(tx *gorm.DB, committedGeneration int64) error {
		token.CacheGeneration = committedGeneration
		// Select is required so disabled/exhausted zero values are persisted.
		return tx.Model(token).Select("status", "cache_generation").Updates(token).Error
	})
}

func (token *Token) Delete() (err error) {
	return mutateTokenMetadata(token, true, func(tx *gorm.DB, _ int64) error {
		return tx.Delete(token).Error
	})
}

func (token *Token) IsModelLimitsEnabled() bool {
	return token.ModelLimitsEnabled
}

func (token *Token) GetModelLimits() []string {
	if token.ModelLimits == "" {
		return []string{}
	}
	return strings.Split(token.ModelLimits, ",")
}

func (token *Token) GetModelLimitsMap() map[string]bool {
	limits := token.GetModelLimits()
	limitsMap := make(map[string]bool)
	for _, limit := range limits {
		limitsMap[limit] = true
	}
	return limitsMap
}

func DisableModelLimits(tokenId int) error {
	token, err := GetTokenById(tokenId)
	if err != nil {
		return err
	}
	token.ModelLimitsEnabled = false
	token.ModelLimits = ""
	return token.Update()
}

func DeleteTokenById(id int, userId int) (err error) {
	// Why we need userId here? In case user want to delete other's token.
	if id == 0 || userId == 0 {
		return errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	err = DB.Where(token).First(&token).Error
	if err != nil {
		return err
	}
	return token.Delete()
}

func IncreaseTokenQuota(tokenId int, key string, quota int) (err error) {
	if quota < 0 {
		return errors.New("quota 不能为负数！")
	}
	if common.RedisEnabled {
		gopool.Go(func() {
			// 守卫式增量：哈希不存在时跳过，由下次读取从数据库水合，
			// 绝不创建只有配额字段的残缺哈希。
			if _, err := cacheApplyTokenQuotaDelta(tokenId, key, int64(quota)); err != nil {
				common.SysLog("failed to increase token quota: " + err.Error())
			}
		})
	}
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeTokenQuota, tokenId, quota)
		return nil
	}
	return increaseTokenQuota(tokenId, quota)
}

func increaseTokenQuota(id int, quota int) (err error) {
	err = DB.Model(&Token{}).Where("id = ?", id).Updates(
		map[string]any{
			"remain_quota":  gorm.Expr("remain_quota + ?", quota),
			"used_quota":    gorm.Expr("used_quota - ?", quota),
			"accessed_time": common.GetTimestamp(),
		},
	).Error
	return err
}

func DecreaseTokenQuota(id int, key string, quota int) (err error) {
	if quota < 0 {
		return errors.New("quota 不能为负数！")
	}
	if common.RedisEnabled {
		gopool.Go(func() {
			if _, err := cacheApplyTokenQuotaDelta(id, key, int64(-quota)); err != nil {
				common.SysLog("failed to decrease token quota: " + err.Error())
			}
		})
	}
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeTokenQuota, id, -quota)
		return nil
	}
	return decreaseTokenQuota(id, quota)
}

func decreaseTokenQuota(id int, quota int) (err error) {
	err = DB.Model(&Token{}).Where("id = ?", id).Updates(
		map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", quota),
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": common.GetTimestamp(),
		},
	).Error
	return err
}

// CountUserTokens returns total number of tokens for the given user, used for pagination
func CountUserTokens(userId int) (int64, error) {
	var total int64
	err := DB.Model(&Token{}).Where("user_id = ?", userId).Count(&total).Error
	return total, err
}

type tokenCacheMutation struct {
	id         int
	key        string
	generation int64
}

func reconcileBatchTokenDeleteCommit(cacheMutations []tokenCacheMutation, commitErr error) (int, error) {
	committedCount := 0
	cacheSyncRequired := false
	causes := []error{commitErr}
	for _, mutation := range cacheMutations {
		cacheSyncRequired = cacheSyncRequired || mutation.generation > 0
		var stored Token
		readErr := DB.Where(&Token{Id: mutation.id, Key: mutation.key}).First(&stored).Error
		switch {
		case errors.Is(readErr, gorm.ErrRecordNotFound):
			committedCount++
			if finalizeErr := commitTokenCacheDeleteMutation(mutation.key, mutation.generation); finalizeErr != nil {
				causes = append(causes, finalizeErr)
			}
		case readErr != nil:
			causes = append(causes, fmt.Errorf("failed to reconcile deleted token %d: %w", mutation.id, readErr))
		}
	}
	if committedCount == len(cacheMutations) && !cacheSyncRequired {
		return committedCount, nil
	}
	if committedCount > 0 {
		return committedCount, &TokenMutationCommittedError{
			Count: committedCount,
			Cause: errors.Join(causes...),
		}
	}
	return 0, errors.Join(causes...)
}

// BatchDeleteTokens 删除指定用户的一组令牌，返回成功删除数量
func BatchDeleteTokens(ids []int, userId int) (int, error) {
	if len(ids) == 0 {
		return 0, errors.New("ids 不能为空！")
	}

	tx := DB.Begin()

	var tokens []Token
	if err := tx.Where("user_id = ? AND id IN (?)", userId, ids).Find(&tokens).Error; err != nil {
		tx.Rollback()
		return 0, err
	}
	cacheMutations := make([]tokenCacheMutation, 0, len(tokens))
	for _, token := range tokens {
		generation, err := beginTokenCacheMutation(token.Key, token.CacheGeneration)
		if err != nil {
			if rollbackErr := tx.Rollback().Error; rollbackErr != nil {
				return 0, errors.Join(err, fmt.Errorf("failed to roll back token database transaction: %w", rollbackErr))
			}
			for _, mutation := range cacheMutations {
				if rollbackErr := rollbackTokenCacheMutation(mutation.key, mutation.generation); rollbackErr != nil {
					common.SysError("failed to roll back token cache fence: " + rollbackErr.Error())
				}
			}
			return 0, err
		}
		cacheMutations = append(cacheMutations, tokenCacheMutation{id: token.Id, key: token.Key, generation: generation})
	}

	if err := tx.Where("user_id = ? AND id IN (?)", userId, ids).Delete(&Token{}).Error; err != nil {
		if rollbackErr := tx.Rollback().Error; rollbackErr != nil {
			return 0, errors.Join(err, fmt.Errorf("failed to roll back token database transaction: %w", rollbackErr))
		}
		for _, mutation := range cacheMutations {
			if rollbackErr := rollbackTokenCacheMutation(mutation.key, mutation.generation); rollbackErr != nil {
				common.SysError("failed to roll back token cache fence: " + rollbackErr.Error())
			}
		}
		return 0, err
	}

	if err := commitTokenMutationTransaction(tx); err != nil {
		return reconcileBatchTokenDeleteCommit(cacheMutations, err)
	}

	var finalizationErrors []error
	for _, mutation := range cacheMutations {
		if err := commitTokenCacheDeleteMutation(mutation.key, mutation.generation); err != nil {
			finalizationErrors = append(finalizationErrors, err)
		}
	}
	if len(finalizationErrors) > 0 {
		return len(tokens), &TokenMutationCommittedError{
			Count: len(tokens),
			Cause: errors.Join(finalizationErrors...),
		}
	}
	return len(tokens), nil
}

func GetTokenKeysByIds(ids []int, userId int) ([]Token, error) {
	var tokens []Token
	err := DB.Select("id", commonKeyCol).
		Where("user_id = ? AND id IN (?)", userId, ids).
		Find(&tokens).Error
	return tokens, err
}

// InvalidateUserTokensCache 清理指定用户所有令牌在 Redis 中的缓存，
// 配合 InvalidateUserCache 使用，可在用户被禁用/删除时立即阻断其令牌的请求。
// 下一次请求将从数据库重新加载令牌及用户状态，从而立即识别出被禁用的用户。
func InvalidateUserTokensCache(userId int) error {
	if !common.RedisEnabled {
		return nil
	}
	if userId <= 0 {
		return errors.New("userId 无效")
	}
	var tokens []Token
	if err := DB.Unscoped().
		Select("id", commonKeyCol).
		Where("user_id = ?", userId).
		Find(&tokens).Error; err != nil {
		return err
	}
	return invalidateTokensCache(tokens)
}

func invalidateTokensCache(tokens []Token) error {
	if !common.RedisEnabled {
		return nil
	}
	var firstErr error
	for _, t := range tokens {
		if t.Key == "" {
			continue
		}
		if err := invalidateTokenCache(t.Key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
