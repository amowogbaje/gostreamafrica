package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/api/internal/auth"
)

// pgTestPool returns a pool bound to a throwaway schema with every .up.sql
// migration applied, so the test never touches real data. Skipped unless
// TEST_DATABASE_URL is set.
func pgTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	files, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return pool
}

func TestPGStore_Integration(t *testing.T) {
	pool := pgTestPool(t)
	s := auth.NewPGStore(pool)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "Ada@Example.com", "hash-1", "Ada")
	if err != nil || u.ID == "" || u.Role != "viewer" || u.Email != "Ada@Example.com" {
		t.Fatalf("create: %+v err=%v", u, err)
	}
	if _, err := s.CreateUser(ctx, "ada@example.COM", "hash-2", "Other"); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("duplicate (case-insensitive) err=%v", err)
	}

	got, hash, err := s.UserByEmail(ctx, "ADA@example.com")
	if err != nil || got.ID != u.ID || hash != "hash-1" {
		t.Fatalf("lookup: %+v hash=%q err=%v", got, hash, err)
	}
	if _, _, err := s.UserByEmail(ctx, "ghost@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("missing user err=%v", err)
	}
	if err := s.UpdatePasswordHash(ctx, u.ID, "hash-3"); err != nil {
		t.Fatal(err)
	}
	if _, h, _ := s.UserByEmail(ctx, "ada@example.com"); h != "hash-3" {
		t.Fatalf("hash not updated: %q", h)
	}

	tok := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	if err := s.CreateSession(ctx, u.ID, tok, now.Add(time.Hour), "test-agent", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if su, err := s.UserBySession(ctx, tok, now); err != nil || su.ID != u.ID {
		t.Fatalf("session lookup: %+v err=%v", su, err)
	}
	if _, err := s.UserBySession(ctx, tok, now.Add(2*time.Hour)); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("expired session err=%v", err)
	}
	if err := s.DeleteSession(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySession(ctx, tok, now); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("deleted session err=%v", err)
	}
	if err := s.DeleteSession(ctx, tok); err != nil {
		t.Fatalf("deleting twice must not fail: %v", err)
	}
}

func TestService_EndToEndOnPostgres(t *testing.T) {
	pool := pgTestPool(t)
	hasher, _ := auth.NewHasher(auth.Params{MemoryKiB: 64, Time: 1, Parallelism: 1})
	svc, err := auth.NewService(auth.NewPGStore(pool), hasher, time.Hour, nopLogger(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := svc.Register(ctx, "bola@example.com", "a long enough password", "Bola", "ua", "203.0.113.1"); err != nil {
		t.Fatal(err)
	}
	user, sess, err := svc.Login(ctx, "BOLA@example.com", "a long enough password", "ua", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Authenticate(ctx, sess.Token); err != nil || got.ID != user.ID {
		t.Fatalf("authenticate: %+v err=%v", got, err)
	}
	if _, _, err := svc.Login(ctx, "bola@example.com", "wrong password here", "ua", "ip"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password err=%v", err)
	}
	if err := svc.Logout(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, sess.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("after logout err=%v", err)
	}
}
