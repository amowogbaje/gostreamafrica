package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"streamafrica/api/internal/auth"
)

// pgTestPool returns a pool bound to a throwaway schema with every .up.sql migration applied,
// so tests never touch real data. Skipped unless TEST_DATABASE_URL is set.
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
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(opt)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func hashOf(b byte) []byte {
	h := make([]byte, 32)
	for i := range h {
		h[i] = b
	}
	return h
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPG_UsersAndProfiles(t *testing.T) {
	pool := pgTestPool(t)
	s := auth.NewPGStore(pool)
	ctx := context.Background()

	me, err := s.CreateUser(ctx, "Ada@Example.com", "hash-1", "Ada")
	if err != nil || me.User.ID == "" || me.User.Role != auth.RoleViewer || me.User.Status != auth.StatusActive ||
		me.User.EmailVerifiedAt != nil || me.Profile.DisplayName != "Ada" || me.Profile.PreferredLanguage != "en" {
		t.Fatalf("create: %+v err=%v", me, err)
	}
	if _, err := s.CreateUser(ctx, "ada@example.COM", "hash-2", "Other"); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("duplicate (case-insensitive): %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM profiles`); n != 1 {
		t.Fatalf("a failed signup left %d profiles (should be 1)", n)
	}

	u, hash, err := s.UserByEmail(ctx, "ADA@example.com")
	if err != nil || u.ID != me.User.ID || hash != "hash-1" {
		t.Fatalf("lookup: %+v %q %v", u, hash, err)
	}
	if _, _, err := s.UserByEmail(ctx, "ghost@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.UpdatePasswordHash(ctx, me.User.ID, "hash-3"); err != nil {
		t.Fatal(err)
	}
	if _, h, _ := s.UserByEmail(ctx, "ada@example.com"); h != "hash-3" {
		t.Fatalf("hash: %q", h)
	}

	ng, yo, name := "NG", "yo", "Ọláolú"
	p, err := s.UpdateProfile(ctx, me.User.ID, auth.ProfilePatch{DisplayName: &name, CountrySet: true, Country: &ng, PreferredLanguage: &yo})
	if err != nil || p.DisplayName != name || p.Country == nil || *p.Country != "NG" || p.PreferredLanguage != "yo" {
		t.Fatalf("patch: %+v %v", p, err)
	}
	p, err = s.UpdateProfile(ctx, me.User.ID, auth.ProfilePatch{CountrySet: true, Country: nil})
	if err != nil || p.Country != nil || p.DisplayName != name {
		t.Fatalf("clear country must leave other fields alone: %+v %v", p, err)
	}
	if _, err := s.UpdateProfile(ctx, "00000000-0000-0000-0000-000000000000", auth.ProfilePatch{DisplayName: &name}); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestPG_SchemaConstraints(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "c@example.com", "h", "C")
	bad := map[string]string{
		"unknown role":      `UPDATE users SET role = 'root' WHERE email = 'c@example.com'`,
		"unknown status":    `UPDATE users SET status = 'banned' WHERE email = 'c@example.com'`,
		"lowercase country": `UPDATE profiles SET country = 'ng'`,
		"http avatar":       `UPDATE profiles SET avatar_url = 'http://x.example.com/a.png'`,
		"bad language":      `UPDATE profiles SET preferred_language = 'English'`,
		"empty name":        `UPDATE profiles SET display_name = ''`,
		"short token hash":  `INSERT INTO refresh_tokens (user_id, token_hash, expires_at, family_expires_at) VALUES ('` + me.User.ID + `', '\x01', now(), now())`,
		"expiry past cap":   `INSERT INTO refresh_tokens (user_id, token_hash, expires_at, family_expires_at) VALUES ('` + me.User.ID + `', decode(repeat('ab', 32), 'hex'), now() + interval '2 days', now() + interval '1 day')`,
	}
	for name, sql := range bad {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: database accepted bad data", name)
		}
	}
}

func TestPG_CascadesAndRestrict(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "gone@example.com", "h", "G")
	_ = s.InsertRefreshToken(ctx, me.User.ID, hashOf(1), time.Now().Add(time.Hour), time.Now().Add(time.Hour), "ua", "203.0.113.5")
	_ = s.IssueOneTimeToken(ctx, me.User.ID, auth.PurposePasswordReset, hashOf(2), time.Now().Add(time.Hour))

	// a user who owns a title cannot be deleted (ON DELETE RESTRICT)
	owner, _ := s.CreateUser(ctx, "owner@example.com", "h", "O")
	if _, err := pool.Exec(ctx, `INSERT INTO titles (type, slug, name, creator_id) VALUES ('movie', 'owned', 'Owned', $1::uuid)`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, owner.User.ID); err == nil {
		t.Fatal("deleting a user who owns titles must be refused")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, me.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"profiles", "refresh_tokens", "one_time_tokens"} {
		if n := count(t, pool, `SELECT count(*) FROM `+table+` WHERE `+map[string]string{"profiles": "user_id", "refresh_tokens": "user_id", "one_time_tokens": "user_id"}[table]+` = $1::uuid`, me.User.ID); n != 0 {
			t.Errorf("%s rows survived deleting the user", table)
		}
	}
}

func TestPG_RefreshRotation(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "rot@example.com", "h", "R")
	uid := me.User.ID
	now := time.Now().UTC().Truncate(time.Millisecond)
	ttl := 30 * 24 * time.Hour

	if err := s.InsertRefreshToken(ctx, uid, hashOf(1), now.Add(ttl), now.Add(90*24*time.Hour), "ua", "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	res, err := s.RotateRefreshToken(ctx, hashOf(1), hashOf(2), now, ttl, "ua2", "203.0.113.6")
	if err != nil || res.UserID != uid || res.FamilyID == "" || !res.ExpiresAt.Equal(now.Add(ttl)) {
		t.Fatalf("rotate: %+v %v", res, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE family_id = $1::uuid`, res.FamilyID); n != 2 {
		t.Fatalf("family has %d tokens", n)
	}
	// the old token is marked used and points at its replacement
	if n := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1 AND used_at IS NOT NULL AND replaced_by IS NOT NULL`, hashOf(1)); n != 1 {
		t.Fatal("old token not marked used")
	}

	// reuse of the old token revokes the whole family, including the newest token
	if _, err := s.RotateRefreshToken(ctx, hashOf(1), hashOf(3), now, ttl, "", ""); !errors.Is(err, auth.ErrRefreshReused) {
		t.Fatalf("reuse: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE family_id = $1::uuid AND revoked_at IS NULL`, res.FamilyID); n != 0 {
		t.Fatalf("%d tokens in the family survived reuse", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, hashOf(3)); n != 0 {
		t.Fatal("a token was issued for a reused refresh token")
	}
	if _, err := s.RotateRefreshToken(ctx, hashOf(2), hashOf(4), now, ttl, "", ""); !errors.Is(err, auth.ErrRefreshInvalid) {
		t.Fatalf("revoked newest token: %v", err)
	}
	if _, err := s.RotateRefreshToken(ctx, hashOf(99), hashOf(5), now, ttl, "", ""); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestPG_RefreshExpiryAndFamilyCap(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "cap@example.com", "h", "C")
	now := time.Now().UTC().Truncate(time.Millisecond)

	_ = s.InsertRefreshToken(ctx, me.User.ID, hashOf(1), now.Add(time.Hour), now.Add(48*time.Hour), "", "")
	if _, err := s.RotateRefreshToken(ctx, hashOf(1), hashOf(2), now.Add(2*time.Hour), time.Hour, "", ""); !errors.Is(err, auth.ErrRefreshInvalid) {
		t.Fatalf("expired token: %v", err)
	}

	_ = s.InsertRefreshToken(ctx, me.User.ID, hashOf(3), now.Add(24*time.Hour), now.Add(25*time.Hour), "", "")
	res, err := s.RotateRefreshToken(ctx, hashOf(3), hashOf(4), now.Add(time.Hour), 30*24*time.Hour, "", "")
	if err != nil || !res.ExpiresAt.Equal(now.Add(25*time.Hour)) {
		t.Fatalf("new expiry must be capped at the family limit: %+v %v", res, err)
	}
	if _, err := s.RotateRefreshToken(ctx, hashOf(4), hashOf(5), now.Add(26*time.Hour), time.Hour, "", ""); !errors.Is(err, auth.ErrRefreshInvalid) {
		t.Fatalf("past the family cap: %v", err)
	}
}

func TestPG_ConcurrentRefreshOnlyOneWins(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "race@example.com", "h", "R")
	now := time.Now().UTC()
	_ = s.InsertRefreshToken(ctx, me.User.ID, hashOf(1), now.Add(time.Hour), now.Add(time.Hour), "", "")

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.RotateRefreshToken(ctx, hashOf(1), hashOf(byte(10+i)), now, time.Hour, "", "")
		}()
	}
	wg.Wait()
	wins := 0
	for _, e := range errs {
		switch {
		case e == nil:
			wins++
		case !errors.Is(e, auth.ErrRefreshReused) && !errors.Is(e, auth.ErrRefreshInvalid):
			// Losers see "reused" (they found the token already rotated) or "invalid" (an earlier
			// loser's reuse response had already revoked the family). Both are safe rejections.
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent refreshes succeeded, want exactly 1", wins)
	}
	// concurrent use of one token is indistinguishable from theft: the family ends up revoked
	if live := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL`); live != 0 {
		t.Fatalf("%d tokens still live after the race", live)
	}
}

func TestPG_RevokeFamilyAndAll(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "rv@example.com", "h", "R")
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	for i := byte(1); i <= 3; i++ {
		_ = s.InsertRefreshToken(ctx, me.User.ID, hashOf(i), exp, exp, "", "")
	}
	if err := s.RevokeFamilyOf(ctx, hashOf(1), now); err != nil {
		t.Fatal(err)
	}
	if live := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL`); live != 2 {
		t.Fatalf("revoking one family must leave the others: %d live", live)
	}
	if err := s.RevokeFamilyOf(ctx, hashOf(77), now); err != nil {
		t.Fatalf("unknown token is not an error: %v", err)
	}
	if err := s.RevokeAllRefreshTokens(ctx, me.User.ID, now); err != nil {
		t.Fatal(err)
	}
	if live := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL`); live != 0 {
		t.Fatalf("%d live after revoke-all", live)
	}
}

func TestPG_OneTimeTokens(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "ott@example.com", "h", "O")
	now := time.Now().UTC().Truncate(time.Millisecond)

	_ = s.IssueOneTimeToken(ctx, me.User.ID, auth.PurposeEmailVerification, hashOf(1), now.Add(time.Hour))
	_ = s.IssueOneTimeToken(ctx, me.User.ID, auth.PurposeEmailVerification, hashOf(2), now.Add(time.Hour))
	if _, err := s.VerifyEmailWithToken(ctx, hashOf(1), now); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("superseded token: %v", err)
	}
	if _, err := s.VerifyEmailWithToken(ctx, hashOf(2), now.Add(2*time.Hour)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expired token: %v", err)
	}
	if uid, err := s.VerifyEmailWithToken(ctx, hashOf(2), now); err != nil || uid != me.User.ID {
		t.Fatalf("verify: %q %v", uid, err)
	}
	if _, err := s.VerifyEmailWithToken(ctx, hashOf(2), now); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("replay: %v", err)
	}
	if got, _ := s.UserByID(ctx, me.User.ID); got.User.EmailVerifiedAt == nil {
		t.Fatal("email not marked verified")
	}

	// the wrong purpose never works
	_ = s.IssueOneTimeToken(ctx, me.User.ID, auth.PurposeEmailVerification, hashOf(3), now.Add(time.Hour))
	if _, err := s.ResetPasswordWithToken(ctx, hashOf(3), now, "new-hash"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("verification token used for reset: %v", err)
	}
}

func TestPG_ResetPassword_SingleUseUnderConcurrency(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "reset@example.com", "old-hash", "R")
	now := time.Now().UTC()
	_ = s.InsertRefreshToken(ctx, me.User.ID, hashOf(50), now.Add(time.Hour), now.Add(time.Hour), "", "")
	_ = s.IssueOneTimeToken(ctx, me.User.ID, auth.PurposePasswordReset, hashOf(9), now.Add(time.Hour))

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.ResetPasswordWithToken(ctx, hashOf(9), now, "new-hash")
		}()
	}
	wg.Wait()
	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
		} else if !errors.Is(e, auth.ErrInvalidToken) {
			t.Fatalf("unexpected: %v", e)
		}
	}
	if wins != 1 {
		t.Fatalf("%d resets succeeded with one token, want 1", wins)
	}
	if _, h, _ := s.UserByEmail(ctx, "reset@example.com"); h != "new-hash" {
		t.Fatalf("hash not updated: %q", h)
	}
	if live := count(t, pool, `SELECT count(*) FROM refresh_tokens WHERE revoked_at IS NULL`); live != 0 {
		t.Fatal("reset must revoke every refresh token")
	}
}

func TestPG_TitleOwnerLookup(t *testing.T) {
	pool := pgTestPool(t)
	ctx := context.Background()
	s := auth.NewPGStore(pool)
	me, _ := s.CreateUser(ctx, "creator@example.com", "h", "C")
	var titleID string
	if err := pool.QueryRow(ctx, `INSERT INTO titles (type, slug, name, creator_id) VALUES ('movie', 'mine', 'Mine', $1::uuid) RETURNING id::text`, me.User.ID).Scan(&titleID); err != nil {
		t.Fatal(err)
	}
	lookup := auth.PGTitleOwner(pool)
	if owner, err := lookup(ctx, titleID); err != nil || owner != me.User.ID {
		t.Fatalf("lookup: %q %v", owner, err)
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid", "", "'; DROP TABLE titles;--"} {
		if _, err := lookup(ctx, id); !errors.Is(err, auth.ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if err := auth.AssertTitleOwner(ctx, lookup, auth.Principal{UserID: me.User.ID, Role: auth.RoleCreator}, titleID); err != nil {
		t.Errorf("owner: %v", err)
	}
	if err := auth.AssertTitleOwner(ctx, lookup, auth.Principal{UserID: "someone-else", Role: auth.RoleCreator}, titleID); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("other creator: %v", err)
	}
}
