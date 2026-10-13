package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// FailureTracker implements login lockout with exponential backoff. It is consulted before
// the password is checked, so a locked-out caller learns nothing about the password.
type FailureTracker interface {
	// Locked returns how long the caller must wait (0 when not locked).
	Locked(ctx context.Context, email, ip string) (time.Duration, error)
	// Failed records a failed attempt for this email from this IP.
	Failed(ctx context.Context, email, ip string) error
	// Succeeded clears the failure counter for this email from this IP.
	Succeeded(ctx context.Context, email, ip string) error
}

// LockPolicy says when a counter starts locking and how the lock grows:
// the lock is Base * 2^(failures - Threshold), capped at Max.
type LockPolicy struct {
	Window    time.Duration // failures older than this are forgotten
	Threshold int           // failures before the first lock
	Base      time.Duration
	Max       time.Duration
}

// DefaultPairPolicy locks one (account, IP) pair: 5 failures in 15 minutes, 30s doubling to 15m.
// Keyed by IP as well, so an attacker cannot lock the real owner out from another address.
func DefaultPairPolicy() LockPolicy {
	return LockPolicy{Window: 15 * time.Minute, Threshold: 5, Base: 30 * time.Second, Max: 15 * time.Minute}
}

// DefaultAccountPolicy locks an account across all IPs after a distributed attack:
// 30 failures in an hour, 5 minutes doubling to 30m.
func DefaultAccountPolicy() LockPolicy {
	return LockPolicy{Window: time.Hour, Threshold: 30, Base: 5 * time.Minute, Max: 30 * time.Minute}
}

// failScript counts a failure and, once over the threshold, sets an exponentially growing lock.
// Returns {failures, lock_ms}.
const failScript = `
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local lock = 0
local threshold = tonumber(ARGV[2])
if n >= threshold then
  local exp = n - threshold
  if exp > 30 then exp = 30 end
  lock = tonumber(ARGV[3]) * (2 ^ exp)
  if lock > tonumber(ARGV[4]) then lock = tonumber(ARGV[4]) end
  lock = math.floor(lock)
  redis.call('SET', KEYS[2], '1', 'PX', lock)
end
return {n, lock}
`

// RedisTracker is the production FailureTracker.
type RedisTracker struct {
	client  *redis.Client
	script  *redis.Script
	pair    LockPolicy
	account LockPolicy
}

// NewRedisTracker builds a tracker with the given policies.
func NewRedisTracker(c *redis.Client, pair, account LockPolicy) *RedisTracker {
	return &RedisTracker{client: c, script: redis.NewScript(failScript), pair: pair, account: account}
}

func trackerKeys(email, ip string) (pairFail, pairLock, acctFail, acctLock string) {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	h := hex.EncodeToString(sum[:])
	return "auth:fail:p:" + h + ":" + ip, "auth:lock:p:" + h + ":" + ip,
		"auth:fail:a:" + h, "auth:lock:a:" + h
}

// Locked implements FailureTracker.
func (t *RedisTracker) Locked(ctx context.Context, email, ip string) (time.Duration, error) {
	_, pairLock, _, acctLock := trackerKeys(email, ip)
	pipe := t.client.Pipeline()
	a := pipe.PTTL(ctx, pairLock)
	b := pipe.PTTL(ctx, acctLock)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	var longest time.Duration
	for _, d := range []time.Duration{a.Val(), b.Val()} {
		if d > longest { // PTTL is negative when the key is missing
			longest = d
		}
	}
	return longest, nil
}

// Failed implements FailureTracker.
func (t *RedisTracker) Failed(ctx context.Context, email, ip string) error {
	pairFail, pairLock, acctFail, acctLock := trackerKeys(email, ip)
	run := func(failKey, lockKey string, p LockPolicy) error {
		return t.script.Run(ctx, t.client, []string{failKey, lockKey},
			p.Window.Milliseconds(), p.Threshold, p.Base.Milliseconds(), p.Max.Milliseconds()).Err()
	}
	if err := run(pairFail, pairLock, t.pair); err != nil {
		return err
	}
	return run(acctFail, acctLock, t.account)
}

// Succeeded implements FailureTracker. The account-wide counter is left to expire on its own.
func (t *RedisTracker) Succeeded(ctx context.Context, email, ip string) error {
	pairFail, _, _, _ := trackerKeys(email, ip)
	return t.client.Del(ctx, pairFail).Err()
}
