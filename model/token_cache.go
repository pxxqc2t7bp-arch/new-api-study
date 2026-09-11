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

func getTokenCacheFenceKey(key string) string {
	return fmt.Sprintf("token:fence:%s", common.GenerateHMAC(key))
}

func getTokenCachePendingFenceKey(key string) string {
	return fmt.Sprintf("token:fence:pending:%s", common.GenerateHMAC(key))
}

const tokenCacheFenceGenerationKey = "token:fence:generation"

func tokenCacheTTLSeconds() int {
	ttl := common.RedisKeyCacheSeconds()
	if ttl <= 0 {
		return 60
	}
	return ttl
}

// tokenCacheFenceSeconds must outlive a token mutation's database write plus
// any in-flight reader's DB-read-to-cache-init gap. The fence is not deleted
// after commit; it expires naturally so a reader holding a pre-mutation
// snapshot cannot publish it right after the mutation cleared the cache.
// While the fence exists readers simply serve the database without caching.
const tokenCacheFenceSeconds = 10

var errTokenCacheMutationPending = errors.New("token metadata update is pending")

var errTokenCacheMutationCommitted = errors.New("token metadata was recently updated")

type tokenCacheFenceState int

const (
	tokenCacheFenceNone tokenCacheFenceState = iota
	tokenCacheFencePending
	tokenCacheFenceCommitted
)

func getTokenCacheFenceState(key string) (tokenCacheFenceState, int64, error) {
	values, err := common.RDB.MGet(
		context.Background(),
		getTokenCachePendingFenceKey(key),
		getTokenCacheFenceKey(key),
	).Result()
	if err != nil {
		return tokenCacheFenceNone, 0, err
	}
	for index, value := range values {
		if value == nil {
			continue
		}
		generation, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
		if err != nil || generation <= 0 {
			return tokenCacheFenceNone, 0, fmt.Errorf("invalid token cache fence generation")
		}
		if index == 0 {
			return tokenCacheFencePending, generation, nil
		}
		return tokenCacheFenceCommitted, generation, nil
	}
	return tokenCacheFenceNone, 0, nil
}

// beginTokenCacheMutation atomically publishes a pending generation and drops
// the cached hash before a metadata write can reach the database.
func beginTokenCacheMutation(key string) (int64, error) {
	if !common.RedisEnabled || key == "" {
		return 0, nil
	}
	const script = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
local generation = redis.call('INCR', KEYS[3])
redis.call('SET', KEYS[1], generation, 'EX', ARGV[1])
redis.call('DEL', KEYS[2])
return generation`
	generation, err := common.RDB.Eval(context.Background(), script, []string{
		getTokenCachePendingFenceKey(key),
		getTokenCacheKey(key),
		tokenCacheFenceGenerationKey,
	}, tokenCacheFenceSeconds).Int64()
	if err != nil {
		return 0, err
	}
	if generation == 0 {
		return 0, errTokenCacheMutationPending
	}
	return generation, nil
}

// commitTokenCacheMutation promotes a pending generation to a bounded
// committed marker. Readers bypass Redis until it expires, so delayed
// pre-mutation snapshots cannot repopulate the hash.
func commitTokenCacheMutation(key string, generation int64) error {
	if !common.RedisEnabled || key == "" || generation == 0 {
		return nil
	}
	const script = `
local incoming = tonumber(ARGV[1])
local pending = tonumber(redis.call('GET', KEYS[1]) or '0')
local committed = tonumber(redis.call('GET', KEYS[2]) or '0')
if committed < incoming then
  redis.call('SET', KEYS[2], incoming, 'EX', ARGV[2])
else
  redis.call('EXPIRE', KEYS[2], ARGV[2])
end
if pending == incoming then
  redis.call('DEL', KEYS[1])
end
redis.call('DEL', KEYS[3])
return 1`
	return common.RDB.Eval(context.Background(), script, []string{
		getTokenCachePendingFenceKey(key),
		getTokenCacheFenceKey(key),
		getTokenCacheKey(key),
	}, generation, tokenCacheFenceSeconds).Err()
}

func rollbackTokenCacheMutation(key string, generation int64) error {
	if !common.RedisEnabled || key == "" || generation == 0 {
		return nil
	}
	const script = `
if tonumber(redis.call('GET', KEYS[1]) or '0') == tonumber(ARGV[1]) then
  redis.call('DEL', KEYS[1])
end
return 1`
	return common.RDB.Eval(context.Background(), script,
		[]string{getTokenCachePendingFenceKey(key)}, generation,
	).Err()
}

// invalidateTokenCache publishes an already-committed generation for database
// changes whose transaction was fenced elsewhere, such as user revocation.
func invalidateTokenCache(key string) error {
	if !common.RedisEnabled || key == "" {
		return nil
	}
	const script = `
local generation = redis.call('INCR', KEYS[3])
redis.call('SET', KEYS[1], generation, 'EX', ARGV[1])
redis.call('DEL', KEYS[2])
return generation`
	return common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheFenceKey(key),
		getTokenCacheKey(key),
		tokenCacheFenceGenerationKey,
	}, tokenCacheFenceSeconds).Err()
}

// cacheInitToken publishes a database snapshot only when no mutation fence is
// active and the hash is cold. An existing hash only gets its TTL refreshed:
// its RemainQuota may already be ahead of this snapshot because atomic
// pre-consume decrements Redis first, so a snapshot must never overwrite any
// field of a live hash.
// 返回值：0=被 fence 拦截，1=完成初始化，2=哈希已存在，仅刷新 TTL。
func cacheInitToken(token Token) (int, error) {
	if !common.RedisEnabled {
		return 0, nil
	}
	allowIps := ""
	if token.AllowIps != nil {
		allowIps = *token.AllowIps
	}
	const script = `
if redis.call('EXISTS', KEYS[2]) == 1 or redis.call('EXISTS', KEYS[3]) == 1 then
  return 0
end
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[21])
  return 2
end
redis.call('HSET', KEYS[1],
  'Id', ARGV[1], 'UserId', ARGV[2], 'Status', ARGV[3], 'Name', ARGV[4],
  'CreatedTime', ARGV[5], 'AccessedTime', ARGV[6], 'ExpiredTime', ARGV[7],
  'UnlimitedQuota', ARGV[8], 'ModelLimitsEnabled', ARGV[9], 'ModelLimits', ARGV[10],
  'AllowIps', ARGV[11], 'Group', ARGV[12], 'CrossGroupRetry', ARGV[13],
  'AutoGroups', ARGV[14], 'RemainQuota', ARGV[15], 'UsedQuota', ARGV[16],
  'DefaultRoutingStrategy', ARGV[17], 'AllowedRoutingStrategies', ARGV[18],
  'DefaultConversionPolicy', ARGV[19], 'AllowLossyConversion', ARGV[20])
redis.call('EXPIRE', KEYS[1], ARGV[21])
return 1`

	return common.RDB.Eval(context.Background(), script, []string{
		getTokenCacheKey(token.Key),
		getTokenCachePendingFenceKey(token.Key),
		getTokenCacheFenceKey(token.Key),
	},
		token.Id, token.UserId, token.Status, token.Name,
		token.CreatedTime, token.AccessedTime, token.ExpiredTime,
		strconv.FormatBool(token.UnlimitedQuota), strconv.FormatBool(token.ModelLimitsEnabled),
		token.ModelLimits, allowIps, token.Group, strconv.FormatBool(token.CrossGroupRetry),
		token.AutoGroups, token.RemainQuota, token.UsedQuota,
		token.DefaultRoutingStrategy, token.AllowedRoutingStrategies,
		token.DefaultConversionPolicy, strconv.FormatBool(token.AllowLossyConversion),
		tokenCacheTTLSeconds(),
	).Int()
}

// cacheGetTokenByKey 从缓存读取 token；不完整的哈希（如仅有配额字段）会被拒绝。
func cacheGetTokenByKey(key string) (*Token, error) {
	if !common.RedisEnabled {
		return nil, fmt.Errorf("redis is not enabled")
	}
	state, _, err := getTokenCacheFenceState(key)
	if err != nil {
		return nil, err
	}
	switch state {
	case tokenCacheFencePending:
		return nil, errTokenCacheMutationPending
	case tokenCacheFenceCommitted:
		return nil, errTokenCacheMutationCommitted
	}
	var token Token
	if cacheErr := common.RedisHGetObj(getTokenCacheKey(key), &token); cacheErr != nil {
		return nil, cacheErr
	}
	state, _, err = getTokenCacheFenceState(key)
	if err != nil {
		return nil, err
	}
	switch state {
	case tokenCacheFencePending:
		return nil, errTokenCacheMutationPending
	case tokenCacheFenceCommitted:
		return nil, errTokenCacheMutationCommitted
	}
	if token.Id <= 0 {
		return nil, fmt.Errorf("token cache is incomplete")
	}
	token.Key = key
	return &token, nil
}
