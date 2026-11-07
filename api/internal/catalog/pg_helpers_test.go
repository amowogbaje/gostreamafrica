package catalog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"streamafrica/api/internal/auth"
)

// pgTestPool returns a pool bound to a throwaway schema with every .up.sql migration applied.
// Skipped unless TEST_DATABASE_URL is set.
func pgTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		skipOrFail(t, "TEST_DATABASE_URL not set")
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
	cfg.MaxConns = 16
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

func redisTestClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		skipOrFail(t, "TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fx is a database with helpers to build catalog fixtures.
type fx struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *PGStore
	svc   *Service
	base  time.Time

	admin, creatorA, creatorB, viewerA, viewerB string
}

func newFx(t *testing.T) *fx {
	t.Helper()
	pool := pgTestPool(t)
	f := &fx{t: t, ctx: context.Background(), pool: pool, store: NewPGStore(pool), base: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	f.svc = NewService(f.store, NoCache{}, quietLogger(), time.Now, Config{})
	f.admin = f.user("admin")
	f.creatorA = f.user("creator")
	f.creatorB = f.user("creator")
	f.viewerA = f.user("viewer")
	f.viewerB = f.user("viewer")
	return f
}

func (f *fx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%v\n%s", err, sql)
	}
}

func (f *fx) user(role string) string {
	f.t.Helper()
	var id string
	email := role + "-" + randHex(5) + "@example.com"
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ($1::citext, '!', $2::user_role) RETURNING id::text`,
		email, role).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO profiles (user_id, display_name) VALUES ($1::uuid, $2)`, id, role)
	return id
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// day returns base + n days (n may be negative).
func (f *fx) day(n int) *time.Time {
	t := f.base.AddDate(0, 0, n)
	return &t
}

type tspec struct {
	typ, name, synopsis, status, lang string
	published                         *time.Time
	creator                           string
	genres                            []string
}

// title inserts a title. Defaults: published movie in English owned by creatorA, published today+0.
func (f *fx) title(slug string, mod func(*tspec)) string {
	f.t.Helper()
	s := tspec{typ: "movie", name: slug, status: "published", lang: "en", creator: f.creatorA, published: f.day(0)}
	if mod != nil {
		mod(&s)
	}
	if s.status != "published" && s.status != "unpublished" && s.published != nil {
		s.published = nil
	}
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO titles (type, slug, name, synopsis, original_language, status, creator_id, published_at)
		 VALUES ($1::title_type, $2, $3, NULLIF($4, ''), $5, $6::title_status, $7::uuid, $8) RETURNING id::text`,
		s.typ, slug, s.name, s.synopsis, s.lang, s.status, s.creator, s.published).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	for _, g := range s.genres {
		f.exec(`INSERT INTO title_genres (title_id, genre_id) SELECT $1::uuid, id FROM genres WHERE slug = $2`, id, g)
	}
	return id
}

func (f *fx) progress(user, title string, episode *string, pos, dur int, completed bool, at time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO watch_progress (user_id, title_id, episode_id, position_seconds, duration_seconds, completed, updated_at)
	        VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7)`, user, title, episode, pos, dur, completed, at)
}

func (f *fx) list(v Viewer, p ListParams) Page {
	f.t.Helper()
	if p.Limit == 0 {
		p.Limit = 50
	}
	if p.Sort == "" {
		p.Sort = SortNewest
	}
	page, err := f.svc.List(f.ctx, v, p)
	if err != nil {
		f.t.Fatal(err)
	}
	return page
}

func slugs(items []TitleSummary) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Slug
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *fx) as(id string, role auth.Role) Viewer { return Viewer{UserID: id, Role: role} }

// skipOrFail skips an integration test whose service is not available, unless REQUIRE_INTEGRATION=1 (set by
// `make test` and CI), when a missing service is a failure: tests must not be silently skipped there.
func skipOrFail(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("REQUIRE_INTEGRATION") == "1" {
		t.Fatalf("%s, but REQUIRE_INTEGRATION=1: integration tests may not be skipped (start the stack with `make up`)", msg)
	}
	t.Skip(msg)
}
