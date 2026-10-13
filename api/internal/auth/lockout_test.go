package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
)

func fastTracker(t *testing.T) *auth.RedisTracker {
	t.Helper()
	return auth.NewRedisTracker(redisTestClient(t),
		auth.LockPolicy{Window: time.Minute, Threshold: 3, Base: 400 * time.Millisecond, Max: 5 * time.Second},
		auth.LockPolicy{Window: time.Minute, Threshold: 6, Base: 400 * time.Millisecond, Max: 5 * time.Second})
}

func TestRedisTracker_LocksPerAccountAndIP(t *testing.T) {
	tr := fastTracker(t)
	ctx := context.Background()
	email, ip, otherIP := uniqueEmail("lk"), randomIP(), randomIP()

	for i := 0; i < 2; i++ {
		_ = tr.Failed(ctx, email, ip)
		if d, err := tr.Locked(ctx, email, ip); err != nil || d != 0 {
			t.Fatalf("locked too early after %d failures: %v %v", i+1, d, err)
		}
	}
	_ = tr.Failed(ctx, email, ip)
	d, err := tr.Locked(ctx, email, ip)
	if err != nil || d <= 0 || d > 400*time.Millisecond {
		t.Fatalf("expected a ~400ms lock: %v %v", d, err)
	}
	// the same account from another IP, and another account from this IP, are unaffected
	if d, _ := tr.Locked(ctx, email, otherIP); d != 0 {
		t.Fatalf("lock leaked to another IP: %v", d)
	}
	if d, _ := tr.Locked(ctx, uniqueEmail("other"), ip); d != 0 {
		t.Fatalf("lock leaked to another account: %v", d)
	}
	// email case does not matter
	if d, _ := tr.Locked(ctx, " "+strings.ToUpper(email)+" ", ip); d == 0 {
		t.Fatal("lock must apply regardless of case or padding")
	}
}

func TestRedisTracker_BackoffGrowsAndExpires(t *testing.T) {
	tr := fastTracker(t)
	ctx := context.Background()
	email, ip := uniqueEmail("bo"), randomIP()
	for i := 0; i < 3; i++ {
		_ = tr.Failed(ctx, email, ip)
	}
	first, _ := tr.Locked(ctx, email, ip)
	time.Sleep(first + 60*time.Millisecond)
	if d, _ := tr.Locked(ctx, email, ip); d != 0 {
		t.Fatalf("lock did not expire: %v", d)
	}
	_ = tr.Failed(ctx, email, ip) // 4th failure: the lock doubles
	second, _ := tr.Locked(ctx, email, ip)
	if second <= first || second > 800*time.Millisecond {
		t.Fatalf("backoff did not grow: first=%v second=%v", first, second)
	}
}

func TestRedisTracker_SuccessClearsPairCounter(t *testing.T) {
	tr := fastTracker(t)
	ctx := context.Background()
	email, ip := uniqueEmail("ok"), randomIP()
	_ = tr.Failed(ctx, email, ip)
	_ = tr.Failed(ctx, email, ip)
	_ = tr.Succeeded(ctx, email, ip)
	_ = tr.Failed(ctx, email, ip)
	_ = tr.Failed(ctx, email, ip)
	if d, _ := tr.Locked(ctx, email, ip); d != 0 {
		t.Fatalf("counter was not reset by the success: %v", d)
	}
}

func TestRedisTracker_DistributedAttackLocksAccountEverywhere(t *testing.T) {
	tr := fastTracker(t)
	ctx := context.Background()
	email := uniqueEmail("dist")
	for i := 0; i < 6; i++ { // one failure from each of 6 different IPs: no pair ever reaches 3
		_ = tr.Failed(ctx, email, randomIP())
	}
	if d, _ := tr.Locked(ctx, email, randomIP()); d <= 0 {
		t.Fatal("account-wide lock did not engage")
	}
}

func TestRedisTracker_ProductionPolicies(t *testing.T) {
	p, a := auth.DefaultPairPolicy(), auth.DefaultAccountPolicy()
	if p.Threshold != 5 || p.Base != 30*time.Second || p.Max != 15*time.Minute || p.Window != 15*time.Minute {
		t.Errorf("pair policy changed: %+v", p)
	}
	if a.Threshold != 30 || a.Base != 5*time.Minute || a.Max != 30*time.Minute || a.Window != time.Hour {
		t.Errorf("account policy changed: %+v", a)
	}
}
