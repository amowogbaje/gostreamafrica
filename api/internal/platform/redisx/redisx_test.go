package redisx_test

import (
	"context"
	"os"
	"testing"
	"time"

	"streamafrica/api/internal/platform/redisx"
)

func TestNew_InvalidURLDoesNotLeak(t *testing.T) {
	_, err := redisx.New("redis://:hunter2@host:notaport/0")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "invalid REDIS_URL" {
		t.Fatalf("unexpected error text: %q", got)
	}
}

// Integration test: skipped unless TEST_REDIS_URL is set.
func TestCheck_Integration(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		skipOrFail(t, "TEST_REDIS_URL not set")
	}
	c, err := redisx.New(url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := redisx.Check(c)(ctx); err != nil {
		t.Fatalf("ping failed: %v", err)
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
