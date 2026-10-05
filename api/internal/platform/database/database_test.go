package database_test

import (
	"context"
	"os"
	"testing"
	"time"

	"streamafrica/api/internal/platform/database"
)

func TestNew_InvalidURLDoesNotLeak(t *testing.T) {
	_, err := database.New(context.Background(), "postgres://u:hunter2@:bad/db%zz", 2)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "invalid DATABASE_URL" {
		t.Fatalf("unexpected error text: %q", got)
	}
}

// Integration test: skipped unless TEST_DATABASE_URL is set.
func TestCheck_Integration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := database.New(ctx, url, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Check(pool)(ctx); err != nil {
		t.Fatalf("check failed: %v", err)
	}
}
