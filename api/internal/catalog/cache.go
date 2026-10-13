package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache stores rendered responses. Invalidation is by epoch: every cached name lives under the
// current epoch, and Invalidate bumps the epoch, so every cached value becomes unreachable at once
// (and expires by TTL later). A reader that loaded the old epoch writes under the OLD epoch, so a
// slow reader can never put stale data back after an invalidation.
type Cache interface {
	// Load returns the cached value and the epoch it was read under.
	Load(ctx context.Context, name string) (value []byte, epoch int64, hit bool, err error)
	// Store saves value under the epoch obtained from Load.
	Store(ctx context.Context, epoch int64, name string, value []byte, ttl time.Duration) error
	// Invalidate makes everything cached so far unreachable.
	Invalidate(ctx context.Context) error
}

// defaultNamespace prefixes every key this cache writes.
const defaultNamespace = "catalog"

// loadScript reads the epoch and the value in one round trip.
// KEYS[1] is the epoch key, ARGV[1] the namespace and ARGV[2] the entry name.
// Returns {epoch, 0} on a miss and {epoch, 1, value} on a hit.
const loadScript = `
local e = redis.call('GET', KEYS[1])
if not e then e = '0' end
local v = redis.call('GET', ARGV[1] .. ':' .. e .. ':' .. ARGV[2])
if not v then return {e, 0} end
return {e, 1, v}
`

// RedisCache is the production Cache.
type RedisCache struct {
	client *redis.Client
	load   *redis.Script
	ns     string
}

// NewRedisCache wraps a client. namespace separates caches that share one Redis (environments,
// tests); empty means "catalog".
func NewRedisCache(c *redis.Client, namespace string) *RedisCache {
	if namespace == "" {
		namespace = defaultNamespace
	}
	return &RedisCache{client: c, load: redis.NewScript(loadScript), ns: namespace}
}

func (c *RedisCache) epochKey() string { return c.ns + ":epoch" }

// Load implements Cache.
func (c *RedisCache) Load(ctx context.Context, name string) ([]byte, int64, bool, error) {
	res, err := c.load.Run(ctx, c.client, []string{c.epochKey()}, c.ns, name).Slice()
	if err != nil {
		return nil, 0, false, err
	}
	if len(res) < 2 {
		return nil, 0, false, errors.New("unexpected cache script result")
	}
	epochStr, _ := res[0].(string)
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		return nil, 0, false, fmt.Errorf("bad cache epoch: %w", err)
	}
	if hit, _ := res[1].(int64); hit != 1 || len(res) < 3 {
		return nil, epoch, false, nil
	}
	val, _ := res[2].(string)
	return []byte(val), epoch, true, nil
}

// Store implements Cache.
func (c *RedisCache) Store(ctx context.Context, epoch int64, name string, value []byte, ttl time.Duration) error {
	return c.client.Set(ctx, fmt.Sprintf("%s:%d:%s", c.ns, epoch, name), value, ttl).Err()
}

// Invalidate implements Cache.
func (c *RedisCache) Invalidate(ctx context.Context) error {
	return c.client.Incr(ctx, c.epochKey()).Err()
}

// NoCache is a Cache that never hits. Useful when Redis is not wanted (and in tests).
type NoCache struct{}

// Load implements Cache.
func (NoCache) Load(context.Context, string) ([]byte, int64, bool, error) { return nil, 0, false, nil }

// Store implements Cache.
func (NoCache) Store(context.Context, int64, string, []byte, time.Duration) error { return nil }

// Invalidate implements Cache.
func (NoCache) Invalidate(context.Context) error { return nil }
