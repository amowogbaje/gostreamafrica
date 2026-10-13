// Package ratelimit provides a Redis-backed fixed-window rate limiter and HTTP middleware.
//
// Trade-off: a fixed window allows a burst of up to 2x the limit across a window
// boundary. That is acceptable for login and payment endpoints and keeps the
// implementation to one atomic Lua call.
package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"streamafrica/api/internal/platform/httpx"
)

// Store counts hits per key inside a window.
type Store interface {
	// Hit records one request and returns the new count and the time until the window resets.
	Hit(ctx context.Context, key string, window time.Duration) (count int64, resetIn time.Duration, err error)
}

// hitScript increments the counter, starts the window on the first hit and
// repairs a key that somehow lost its expiry. It returns {count, ttl_ms}.
const hitScript = `
local c = redis.call('INCR', KEYS[1])
if c == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {c, ttl}
`

// RedisStore is the production Store.
type RedisStore struct {
	client *redis.Client
	script *redis.Script
}

// NewRedisStore wraps a client.
func NewRedisStore(c *redis.Client) *RedisStore {
	return &RedisStore{client: c, script: redis.NewScript(hitScript)}
}

// Hit implements Store.
func (s *RedisStore) Hit(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	vals, err := s.script.Run(ctx, s.client, []string{key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return 0, 0, err
	}
	if len(vals) != 2 {
		return 0, 0, errors.New("unexpected script result")
	}
	return vals[0], time.Duration(vals[1]) * time.Millisecond, nil
}

// Rule describes one limit. Name is part of the Redis key and the log line.
type Rule struct {
	Name   string
	Limit  int64
	Window time.Duration
	// FailOpen decides what happens when Redis is unreachable. false (the safe
	// default for auth and payments) answers 503; true lets the request through.
	FailOpen bool
}

// Decision is the outcome of one limiter check.
type Decision struct {
	Allowed    bool
	Limit      int64
	Remaining  int64
	RetryAfter time.Duration
}

// Allow records one hit for key under rule and reports whether it is within the limit.
func Allow(ctx context.Context, store Store, rule Rule, key string) (Decision, error) {
	count, resetIn, err := store.Hit(ctx, "rl:"+rule.Name+":"+key, rule.Window)
	if err != nil {
		return Decision{}, err
	}
	remaining := rule.Limit - count
	if remaining < 0 {
		remaining = 0
	}
	return Decision{
		Allowed:    count <= rule.Limit,
		Limit:      rule.Limit,
		Remaining:  remaining,
		RetryAfter: resetIn,
	}, nil
}

// Reject writes the 429 response (with rate-limit headers) for a denied decision.
func Reject(w http.ResponseWriter, r *http.Request, d Decision) {
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
	w.Header().Set("X-RateLimit-Remaining", "0")
	secs := int(math.Ceil(d.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpx.WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests, try again later")
}

// RejectUnavailable writes the 503 used when the limiter store fails and the rule is fail-closed.
func RejectUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	httpx.WriteError(w, r, http.StatusServiceUnavailable,
		"rate_limiter_unavailable", "service temporarily unavailable")
}

// KeyFunc picks the identity being limited (for example the client IP).
type KeyFunc func(*http.Request) string

// IPKey limits by client IP, trusting `hops` reverse proxies in front of the API.
func IPKey(hops int) KeyFunc {
	return func(r *http.Request) string { return ClientIP(r, hops) }
}

// Middleware enforces rule using store. Rejections use the standard error format.
func Middleware(store Store, logger *slog.Logger, rule Rule, key KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, err := Allow(r.Context(), store, rule, key(r))
			if err != nil {
				logger.ErrorContext(r.Context(), "rate limiter store error",
					"rule", rule.Name, "fail_open", rule.FailOpen, "error", err.Error())
				if rule.FailOpen {
					next.ServeHTTP(w, r)
					return
				}
				RejectUnavailable(w, r)
				return
			}
			if !d.Allowed {
				Reject(w, r, d)
				return
			}
			w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(d.Remaining, 10))
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the caller's IP. With hops == 0 it uses the TCP peer address.
// With hops == N it takes the Nth entry from the RIGHT of X-Forwarded-For, because
// each trusted proxy appends the address it saw; entries further left are
// client-controlled and can be forged. Anything unparsable falls back to the peer.
func ClientIP(r *http.Request, hops int) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if hops <= 0 {
		return peer
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(parts) < hops {
		return peer
	}
	candidate := strings.TrimSpace(parts[len(parts)-hops])
	if net.ParseIP(candidate) == nil {
		return peer
	}
	return candidate
}
