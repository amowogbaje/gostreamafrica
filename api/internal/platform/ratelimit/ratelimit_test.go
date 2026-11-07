package ratelimit_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/ratelimit"
)

type fakeStore struct {
	mu     sync.Mutex
	counts map[string]int64
	reset  time.Duration
	err    error
}

func newFake() *fakeStore { return &fakeStore{counts: map[string]int64{}, reset: 30 * time.Second} }

func (f *fakeStore) Hit(_ context.Context, key string, _ time.Duration) (int64, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, 0, f.err
	}
	f.counts[key]++
	return f.counts[key], f.reset, nil
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func build(store ratelimit.Store, rule ratelimit.Rule) http.Handler {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return httpx.Chain(ok, httpx.RequestID, ratelimit.Middleware(store, quiet(), rule, ratelimit.IPKey(0)))
}

func call(h http.Handler, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_BlocksOverLimit(t *testing.T) {
	h := build(newFake(), ratelimit.Rule{Name: "login", Limit: 2, Window: time.Minute})

	for i := 1; i <= 2; i++ {
		if rec := call(h, "192.0.2.1:1000"); rec.Code != http.StatusNoContent {
			t.Fatalf("request %d: code=%d", i, rec.Code)
		}
	}
	rec := call(h, "192.0.2.1:1000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "30" || rec.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("headers: %v", rec.Header())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "rate_limited" || body.Error.RequestID == "" {
		t.Fatalf("bad body: %+v", body)
	}
}

func TestMiddleware_SeparatesClients(t *testing.T) {
	h := build(newFake(), ratelimit.Rule{Name: "login", Limit: 1, Window: time.Minute})
	if call(h, "192.0.2.1:1").Code != http.StatusNoContent {
		t.Fatal("first client should pass")
	}
	if call(h, "192.0.2.1:2").Code != http.StatusTooManyRequests {
		t.Fatal("same IP, different port, should be limited")
	}
	if call(h, "192.0.2.2:1").Code != http.StatusNoContent {
		t.Fatal("other IP should not be affected")
	}
}

func TestMiddleware_StoreFailure(t *testing.T) {
	broken := newFake()
	broken.err = errors.New("redis down")

	closed := build(broken, ratelimit.Rule{Name: "login", Limit: 1, Window: time.Minute})
	rec := call(closed, "192.0.2.1:1")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("fail-closed: code=%d headers=%v", rec.Code, rec.Header())
	}
	got := rec.Body.String()
	if !strings.Contains(got, "rate_limiter_unavailable") || strings.Contains(got, "redis down") {
		t.Fatalf("body=%s", got)
	}

	open := build(broken, ratelimit.Rule{Name: "login", Limit: 1, Window: time.Minute, FailOpen: true})
	if rec := call(open, "192.0.2.1:1"); rec.Code != http.StatusNoContent {
		t.Fatalf("fail-open: code=%d", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name, remote, xff string
		hops              int
		want              string
	}{
		{"no proxy ignores header", "203.0.113.5:1000", "1.1.1.1", 0, "203.0.113.5"},
		{"one proxy takes rightmost", "10.0.0.2:80", "9.9.9.9, 203.0.113.7", 1, "203.0.113.7"},
		{"two proxies take second from right", "10.0.0.2:80", "198.51.100.1, 203.0.113.8", 2, "198.51.100.1"},
		{"missing header falls back to peer", "203.0.113.5:1000", "", 1, "203.0.113.5"},
		{"garbage falls back to peer", "203.0.113.5:1000", "not-an-ip", 1, "203.0.113.5"},
		{"too few entries falls back to peer", "203.0.113.5:1000", "198.51.100.1", 2, "203.0.113.5"},
		{"ipv6 peer", "[2001:db8::1]:443", "", 0, "2001:db8::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = c.remote
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := ratelimit.ClientIP(req, c.hops); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

// Integration test against a real Redis: skipped unless TEST_REDIS_URL is set.
func TestRedisStore_Integration(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		skipOrFail(t, "TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	defer c.Close()
	store := ratelimit.NewRedisStore(c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := "rl:test:" + time.Now().Format("150405.000000000")
	n1, reset, err := store.Hit(ctx, key, time.Second)
	if err != nil || n1 != 1 || reset <= 0 || reset > time.Second {
		t.Fatalf("first hit: n=%d reset=%v err=%v", n1, reset, err)
	}
	if n2, _, _ := store.Hit(ctx, key, time.Second); n2 != 2 {
		t.Fatalf("second hit n=%d", n2)
	}
	time.Sleep(1200 * time.Millisecond)
	if n3, _, _ := store.Hit(ctx, key, time.Second); n3 != 1 {
		t.Fatalf("after window n=%d", n3)
	}
}

// skipOrFail skips an integration test whose service is not available, unless REQUIRE_INTEGRATION=1 (set by
// `make test` and CI), when a missing service is a failure: tests must not be silently skipped there.
func skipOrFail(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("REQUIRE_INTEGRATION") == "1" {
		t.Fatalf("%s, but REQUIRE_INTEGRATION=1: integration tests may not be skipped (start the stack with `make up`)", msg)
	}
	t.Skip(msg)
}
