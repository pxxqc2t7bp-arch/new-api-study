package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/common"
)

func getTokenCacheKey(key string) string {
	return fmt.Sprintf("token:%s", common.GenerateHMAC(key))
}

func getTokenCacheGenerationKey(key string) string {
	return fmt.Sprintf("token:generation:%s", common.GenerateHMAC(key))
}

func getTokenCachePendingFenceKey(key string) string {
	return fmt.Sprintf("token:fence:pending:%s", common.GenerateHMAC(key))
}

func tokenCacheTTLSeconds() int {
	ttl := common.RedisKeyCacheSeconds()
	if ttl <= 0 {
		return 60
	}
	return ttl
}

// tokenCacheFenceSeconds bounds the advisory pending marker. Correctness does
// not depend on its lifetime: an odd persistent generation keeps readers
// fail-closed until commit or rollback advances it to the next even value.
const tokenCacheFenceSeconds = 10

var errTokenCacheMutationPending = errors.New("token metadata update is pending")

type tokenCacheGenerationState struct {
	generation        int64
	generationPresent bool
	pendingMarker     bool
}

func loadTokenCacheGenerationState(key string) (tokenCacheGenerationState, error) {
	values, err := common.RDB.MGet(
		context.Background(),
		getTokenCacheGenerationKey(key),
		getTokenCachePendingFenceKey(key),
	).Result()
	if err != nil {
		return tokenCacheGenerationState{}, err
	}

	state := tokenCacheGenerationState{}
	if values[0] != nil {
		state.generationPresent = true
		state.generation, err = strconv.ParseInt(fmt.Sprint(values[0]), 10, 64)
		if err != nil || state.generation < 0 {
			return tokenCacheGenerationState{}, fmt.Errorf("invalid token cache generation")
		}
	}
	if values[1] != nil {
		pendingGeneration, parseErr := strconv.ParseInt(fmt.Sprint(values[1]), 10, 64)
		if parseErr != nil || pendingGeneration <= 0 {
			return tokenCacheGenerationState{}, fmt.Errorf("invalid token cache pending generation")
		}
		state.pendingMarker = true
	}
	return state, nil
}

func getTokenCacheGenerationState(key string) (int64, bool, error) {
	state, err := loadTokenCacheGenerationState(key)
	if err != nil {
		return 0, false, err
	}
	return state.generation, state.pendingMarker || state.generation%2 != 0, nil
}

// beginTokenCacheMutation advances the persistent generation from even to odd
// and publishes an advisory pending marker before the database write. The odd
// generation remains after the marker expires, while the fenced hash retains
// live quota counters for commit or rollback.
func beginTokenCacheMutation(key string, minimumGeneration int64) (int64, error) {
	if !common.RedisEnabled || key == "" {
		return 0, nil
	}
	if minimumGeneration < 0 || minimumGeneration%2 != 0 {
		return 0, fmt.Errorf("invalid token database cache generation")
	}
	const script = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local minimum = tonumber(ARGV[2])
if current % 2 ~= 0 or redis.call('EXISTS', KEYS[2]) == 1 then
  return 0
end
if current < minimum then
  current = minimum
  redis.call('SET', KEYS[1], current)
end
local generation = current + 1
redis.call('SET', KEYS[1], generation)
redis.call('SET', KEYS[2], generation, 'EX', ARGV[1])
return generation`
	generation, err := common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheGenerationKey(key),
		getTokenCachePendingFenceKey(key),
	}, tokenCacheFenceSeconds, minimumGeneration).Int64()
	if err != nil {
		return 0, err
	}
	if generation == 0 {
		return 0, errTokenCacheMutationPending
	}
	return generation, nil
}

// commitTokenCacheMutation atomically publishes committed metadata into an
// existing complete hash without replacing its live quota counters. A cold or
// incomplete hash remains absent so a later read can initialize it from DB.
func commitTokenCacheMutation(token Token, generation int64) error {
	if !common.RedisEnabled || token.Key == "" || generation == 0 {
		return nil
	}
	allowIps := ""
	if token.AllowIps != nil {
		allowIps = *token.AllowIps
	}
	const script = `
local incoming = tonumber(ARGV[1])
local currentValue = redis.call('GET', KEYS[1])
local pending = tonumber(redis.call('GET', KEYS[2]) or '0')
if not currentValue then
  if pending ~= incoming then
    return 0
  end
  redis.call('SET', KEYS[1], incoming)
  currentValue = tostring(incoming)
end
local current = tonumber(currentValue)
if current > incoming then
  return 1
end
if current ~= incoming or current % 2 == 0 then
  return 0
end
current = current + 1
redis.call('SET', KEYS[1], current)
if redis.call('EXISTS', KEYS[3]) == 1 then
  if tonumber(redis.call('HGET', KEYS[3], 'Id') or '0') == tonumber(ARGV[2])
    and redis.call('HEXISTS', KEYS[3], 'RemainQuota') == 1
    and redis.call('HEXISTS', KEYS[3], 'UsedQuota') == 1 then
    local cachedRemain = tonumber(redis.call('HGET', KEYS[3], 'RemainQuota'))
    local cachedUsed = tonumber(redis.call('HGET', KEYS[3], 'UsedQuota'))
    local databaseRemain = tonumber(ARGV[18])
    local databaseUsed = tonumber(ARGV[19])
    if cachedRemain == nil or cachedUsed == nil or databaseRemain == nil or databaseUsed == nil then
      redis.call('DEL', KEYS[3])
    else
      local quotaDelta = (databaseRemain + databaseUsed) - (cachedRemain + cachedUsed)
      redis.call('HINCRBY', KEYS[3], 'RemainQuota', quotaDelta)
      redis.call('HSET', KEYS[3],
        'Status', ARGV[3], 'Name', ARGV[4], 'ExpiredTime', ARGV[5],
        'UnlimitedQuota', ARGV[6], 'ModelLimitsEnabled', ARGV[7],
        'ModelLimits', ARGV[8], 'AllowIps', ARGV[9],
        'StreamRecoveryEnabled', ARGV[10], 'Group', ARGV[11],
        'CrossGroupRetry', ARGV[12], 'AutoGroups', ARGV[13],
        'DefaultRoutingStrategy', ARGV[14],
        'AllowedRoutingStrategies', ARGV[15],
        'DefaultConversionPolicy', ARGV[16],
        'AllowLossyConversion', ARGV[17],
        'CacheGeneration', current)
      redis.call('EXPIRE', KEYS[3], ARGV[20])
    end
  else
    redis.call('DEL', KEYS[3])
  end
end
if tonumber(redis.call('GET', KEYS[2]) or '0') == incoming then
  redis.call('DEL', KEYS[2])
end
return 1`
	result, err := common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheGenerationKey(token.Key),
		getTokenCachePendingFenceKey(token.Key),
		getTokenCacheKey(token.Key),
	},
		generation, token.Id, token.Status, token.Name, token.ExpiredTime,
		strconv.FormatBool(token.UnlimitedQuota), strconv.FormatBool(token.ModelLimitsEnabled),
		token.ModelLimits, allowIps, strconv.FormatBool(token.StreamRecoveryEnabled),
		token.Group, strconv.FormatBool(token.CrossGroupRetry), token.AutoGroups,
		token.DefaultRoutingStrategy, token.AllowedRoutingStrategies,
		token.DefaultConversionPolicy, strconv.FormatBool(token.AllowLossyConversion),
		token.RemainQuota, token.UsedQuota,
		tokenCacheTTLSeconds(),
	).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("token cache generation changed before finalization")
	}
	return nil
}

func commitTokenCacheDeleteMutation(key string, generation int64) error {
	if !common.RedisEnabled || key == "" || generation == 0 {
		return nil
	}
	const script = `
local incoming = tonumber(ARGV[1])
local currentValue = redis.call('GET', KEYS[1])
local pending = tonumber(redis.call('GET', KEYS[2]) or '0')
if not currentValue then
  if pending ~= incoming then
    return 0
  end
  redis.call('SET', KEYS[1], incoming)
  currentValue = tostring(incoming)
end
local current = tonumber(currentValue)
if current > incoming then
  return 1
end
if current ~= incoming or current % 2 == 0 then
  return 0
end
redis.call('SET', KEYS[1], current + 1)
redis.call('DEL', KEYS[3])
if tonumber(redis.call('GET', KEYS[2]) or '0') == incoming then
  redis.call('DEL', KEYS[2])
end
return 1`
	result, err := common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheGenerationKey(key),
		getTokenCachePendingFenceKey(key),
		getTokenCacheKey(key),
	}, generation).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("token cache generation changed before delete finalization")
	}
	return nil
}

func rollbackTokenCacheMutation(key string, generation int64) error {
	if !common.RedisEnabled || key == "" || generation == 0 {
		return nil
	}
	const script = `
local incoming = tonumber(ARGV[1])
local currentValue = redis.call('GET', KEYS[1])
local pending = tonumber(redis.call('GET', KEYS[2]) or '0')
if not currentValue then
  if pending ~= incoming then
    return 0
  end
  redis.call('SET', KEYS[1], incoming)
  currentValue = tostring(incoming)
end
local current = tonumber(currentValue)
if current > incoming then
  return 1
end
if current ~= incoming or current % 2 == 0 then
  return 0
end
redis.call('SET', KEYS[1], current - 1)
if tonumber(redis.call('GET', KEYS[2]) or '0') == incoming then
  redis.call('DEL', KEYS[2])
end
return 1`
	result, err := common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheGenerationKey(key),
		getTokenCachePendingFenceKey(key),
	}, generation).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("token cache generation changed before rollback")
	}
	return nil
}

// invalidateTokenCache advances a stable generation by two for database
// changes fenced elsewhere, such as user revocation. An unresolved odd
// generation remains fail-closed for its owning mutation to finalize.
func invalidateTokenCache(key string) error {
	if !common.RedisEnabled || key == "" {
		return nil
	}
	const script = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
if current % 2 == 0 then
  current = current + 2
  redis.call('SET', KEYS[1], current)
end
redis.call('DEL', KEYS[2])
return current`
	return common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheGenerationKey(key),
		getTokenCacheKey(key),
	}).Err()
}

// cacheInitToken publishes a database snapshot only when the persistent
// generation still matches the reader's pre-read snapshot and is stable. An
// existing hash is never overwritten because its quota fields may contain
// live Redis reservations that are newer than the database snapshot.
// 返回值：0=代际变化或 mutation 未完成，1=完成初始化，2=同代哈希已存在，仅刷新 TTL。
func cacheInitToken(token Token, expectedGeneration int64) (int, error) {
	if !common.RedisEnabled {
		return 0, nil
	}
	if expectedGeneration < 0 || expectedGeneration%2 != 0 {
		return 0, nil
	}
	if token.CacheGeneration < 0 || token.CacheGeneration%2 != 0 {
		return 0, fmt.Errorf("invalid token database cache generation")
	}
	allowIps := ""
	if token.AllowIps != nil {
		allowIps = *token.AllowIps
	}
	const script = `
local expected = tonumber(ARGV[23])
local database = tonumber(ARGV[24])
local current = tonumber(redis.call('GET', KEYS[3]) or '0')
if current < database then
  redis.call('SET', KEYS[3], database)
  return 0
end
if current ~= expected or current % 2 ~= 0 or redis.call('EXISTS', KEYS[2]) == 1 then
  return 0
end
if redis.call('EXISTS', KEYS[1]) == 1 then
  local cached = tonumber(redis.call('HGET', KEYS[1], 'CacheGeneration') or '0')
  if cached ~= expected then
    return 0
  end
  redis.call('SET', KEYS[3], expected)
  redis.call('EXPIRE', KEYS[1], ARGV[22])
  return 2
end
redis.call('SET', KEYS[3], expected)
redis.call('HSET', KEYS[1],
  'Id', ARGV[1], 'UserId', ARGV[2], 'Status', ARGV[3], 'Name', ARGV[4],
  'CreatedTime', ARGV[5], 'AccessedTime', ARGV[6], 'ExpiredTime', ARGV[7],
  'UnlimitedQuota', ARGV[8], 'ModelLimitsEnabled', ARGV[9], 'ModelLimits', ARGV[10],
  'AllowIps', ARGV[11], 'StreamRecoveryEnabled', ARGV[12],
  'Group', ARGV[13], 'CrossGroupRetry', ARGV[14],
  'AutoGroups', ARGV[15], 'RemainQuota', ARGV[16], 'UsedQuota', ARGV[17],
  'DefaultRoutingStrategy', ARGV[18], 'AllowedRoutingStrategies', ARGV[19],
  'DefaultConversionPolicy', ARGV[20], 'AllowLossyConversion', ARGV[21],
  'CacheGeneration', ARGV[23])
redis.call('EXPIRE', KEYS[1], ARGV[22])
return 1`

	return common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheKey(token.Key),
		getTokenCachePendingFenceKey(token.Key),
		getTokenCacheGenerationKey(token.Key),
	},
		token.Id, token.UserId, token.Status, token.Name,
		token.CreatedTime, token.AccessedTime, token.ExpiredTime,
		strconv.FormatBool(token.UnlimitedQuota), strconv.FormatBool(token.ModelLimitsEnabled),
		token.ModelLimits, allowIps, strconv.FormatBool(token.StreamRecoveryEnabled),
		token.Group, strconv.FormatBool(token.CrossGroupRetry),
		token.AutoGroups, token.RemainQuota, token.UsedQuota,
		token.DefaultRoutingStrategy, token.AllowedRoutingStrategies,
		token.DefaultConversionPolicy, strconv.FormatBool(token.AllowLossyConversion),
		tokenCacheTTLSeconds(), expectedGeneration, token.CacheGeneration,
	).Int()
}

// cacheGetTokenByKey 从缓存读取 token；不完整的哈希（如仅有配额字段）会被拒绝。
func cacheGetTokenByKey(key string) (*Token, error) {
	if !common.RedisEnabled {
		return nil, fmt.Errorf("redis is not enabled")
	}
	beforeState, err := loadTokenCacheGenerationState(key)
	if err != nil {
		return nil, err
	}
	if !beforeState.generationPresent {
		return nil, fmt.Errorf("token cache generation is missing")
	}
	if beforeState.pendingMarker || beforeState.generation%2 != 0 {
		return nil, errTokenCacheMutationPending
	}
	var token Token
	if cacheErr := common.RedisHGetObj(getTokenCacheKey(key), &token); cacheErr != nil {
		return nil, cacheErr
	}
	afterState, err := loadTokenCacheGenerationState(key)
	if err != nil {
		return nil, err
	}
	if !afterState.generationPresent {
		return nil, fmt.Errorf("token cache generation is missing")
	}
	if afterState.pendingMarker || afterState.generation%2 != 0 {
		return nil, errTokenCacheMutationPending
	}
	if beforeState.generation != afterState.generation || token.CacheGeneration != afterState.generation {
		return nil, fmt.Errorf("token cache generation is stale")
	}
	if token.Id <= 0 {
		return nil, fmt.Errorf("token cache is incomplete")
	}
	token.Key = key
	return &token, nil
}
