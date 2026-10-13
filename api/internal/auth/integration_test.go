package auth_test

import (
	"crypto/sha256"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/ratelimit"
)

// integ is the full stack minus HTTP and SMTP: real PostgreSQL (throwaway schema), real Redis
// (limits and lockout), real argon2id and JWT code, and a fake mailer that captures emails.
type integ struct {
	*harness
	pool *pgxpool.Pool
}

func newIntegration(t *testing.T) *integ {
	t.Helper()
	pool := pgTestPool(t)
	rdb := redisTestClient(t)
	h := newHarness(t, options{
		store:   auth.NewPGStore(pool),
		tracker: auth.NewRedisTracker(rdb, auth.DefaultPairPolicy(), auth.DefaultAccountPolicy()),
		limiter: ratelimit.NewRedisStore(rdb),
	})
	return &integ{harness: h, pool: pool}
}

func TestIntegration_RegisterVerifyLoginRefreshLogout(t *testing.T) {
	in := newIntegration(t)
	email := uniqueEmail("flow")

	// register
	s := in.register(email)
	if n := count(t, in.pool, `SELECT count(*) FROM profiles p JOIN users u ON u.id = p.user_id WHERE u.email = $1::citext AND p.display_name = 'Test User'`, email); n != 1 {
		t.Fatal("user and profile were not created together")
	}
	// only hashes are stored, never the tokens themselves
	sum := sha256.Sum256([]byte(s.refresh))
	if n := count(t, in.pool, `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, sum[:]); n != 1 {
		t.Fatal("refresh token hash not stored")
	}
	if n := count(t, in.pool, `SELECT count(*) FROM users WHERE password_hash LIKE '$argon2id$v=19$m=%'`); n != 1 {
		t.Fatal("password not stored as argon2id")
	}

	// verify
	tok := tokenIn(t, mustLast(t, in.harness, email, "Confirm"))
	if rec := in.do(post, "/v1/auth/verify-email", map[string]string{"token": tok}); rec.Code != http.StatusNoContent {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	if rec := in.do(post, "/v1/auth/verify-email", map[string]string{"token": tok}); rec.Code != http.StatusBadRequest {
		t.Fatalf("verify replay: %d", rec.Code)
	}

	// login (wrong, then right)
	if rec := in.login(email, "wrong password!"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	rec := in.login(email, goodPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	b := decode[authBody](t, rec)
	if b.User.EmailVerifiedAt == nil {
		t.Fatal("login must report the verified email")
	}
	cookie := cookieNamed(rec, "sa_refresh").Value

	// refresh rotates
	r2 := in.refreshWithCookie(cookie)
	if r2.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", r2.Code, r2.Body.String())
	}
	next := cookieNamed(r2, "sa_refresh").Value
	if next == cookie {
		t.Fatal("not rotated")
	}
	if me := in.do(get, "/v1/me", nil, withBearer(decode[authBody](t, r2).AccessToken)); me.Code != http.StatusOK {
		t.Fatalf("me: %d", me.Code)
	}

	// logout ends that session only
	if out := in.do(post, "/v1/auth/logout", nil, withCookie("sa_refresh", next)); out.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", out.Code)
	}
	if rec := in.refreshWithCookie(next); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session alive after logout: %d", rec.Code)
	}
	if rec := in.refreshWithCookie(s.refresh); rec.Code != http.StatusOK {
		t.Fatalf("logout must end only one session; the registration session should still work: %d", rec.Code)
	}
}

func TestIntegration_RefreshTokenReuseDetection(t *testing.T) {
	in := newIntegration(t)
	s := in.register(uniqueEmail("reuse"))

	first := in.refreshWithCookie(s.refresh)
	if first.Code != http.StatusOK {
		t.Fatalf("refresh: %d", first.Code)
	}
	newest := cookieNamed(first, "sa_refresh").Value

	// an attacker replays the token the victim already used
	if rec := in.refreshWithCookie(s.refresh); rec.Code != http.StatusUnauthorized || errCode(t, rec) != "invalid_refresh_token" {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	// ...which also kills the victim's newest token: the whole family is revoked
	if rec := in.refreshWithCookie(newest); rec.Code != http.StatusUnauthorized {
		t.Fatalf("family survived: %d", rec.Code)
	}
	if live := count(t, in.pool, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1::uuid AND revoked_at IS NULL`, s.id); live != 0 {
		t.Fatalf("%d refresh tokens still live", live)
	}
	// the user can simply sign in again
	if rec := in.login(s.email, goodPassword); rec.Code != http.StatusOK {
		t.Fatalf("re-login: %d", rec.Code)
	}
}

func TestIntegration_DoubleSubmitOnlyOneSucceeds(t *testing.T) {
	in := newIntegration(t)
	s := in.register(uniqueEmail("race"))

	codes := make([]int, 6)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = in.refreshWithCookie(s.refresh).Code
		}()
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		} else if c != http.StatusUnauthorized {
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d simultaneous refreshes succeeded, want exactly 1 (%v)", ok, len(codes), codes)
	}
}

func TestIntegration_ForgotAndResetPassword(t *testing.T) {
	in := newIntegration(t)
	email := uniqueEmail("reset")
	s := in.register(email)

	if rec := in.do(post, "/v1/auth/forgot-password", map[string]string{"email": email}); rec.Code != http.StatusAccepted {
		t.Fatalf("forgot: %d", rec.Code)
	}
	tok := tokenIn(t, mustLast(t, in.harness, email, "Reset"))
	if rec := in.do(post, "/v1/auth/reset-password", map[string]string{"token": tok, "new_password": "password123"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("weak password: %d", rec.Code)
	}
	if rec := in.do(post, "/v1/auth/reset-password", map[string]string{"token": tok, "new_password": "a brand new passphrase"}); rec.Code != http.StatusNoContent {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if rec := in.refreshWithCookie(s.refresh); rec.Code != http.StatusUnauthorized {
		t.Fatalf("existing session survived the reset: %d", rec.Code)
	}
	if rec := in.login(email, goodPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password: %d", rec.Code)
	}
	if rec := in.login(email, "a brand new passphrase"); rec.Code != http.StatusOK {
		t.Fatalf("new password: %d", rec.Code)
	}
	if rec := in.do(post, "/v1/auth/reset-password", map[string]string{"token": tok, "new_password": "yet another passphrase"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("replay: %d", rec.Code)
	}
}

func TestIntegration_RoleChangesAndEscalation(t *testing.T) {
	in := newIntegration(t)
	email := uniqueEmail("role")
	s := in.register(email)

	if rec := in.do(get, "/test/admin", nil, withBearer(s.access)); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer on admin route: %d", rec.Code)
	}
	// the only way to become admin is a database change, never an API call
	for _, body := range []string{`{"role":"admin"}`, `{"status":"active"}`} {
		if rec := in.do(patch, "/v1/me", body, withBearer(s.access)); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if _, err := in.pool.Exec(testCtx(), `UPDATE users SET role = 'admin' WHERE email = $1::citext`, email); err != nil {
		t.Fatal(err)
	}
	if rec := in.do(get, "/test/admin", nil, withBearer(s.access)); rec.Code != http.StatusForbidden {
		t.Fatalf("old token must keep its role until refreshed: %d", rec.Code)
	}
	r := in.refreshWithCookie(s.refresh)
	if rec := in.do(get, "/test/admin", nil, withBearer(decode[authBody](t, r).AccessToken)); rec.Code != http.StatusNoContent {
		t.Fatalf("promotion not applied after refresh: %d", rec.Code)
	}

	// suspending the account stops refreshes and logins
	if _, err := in.pool.Exec(testCtx(), `UPDATE users SET status = 'suspended' WHERE email = $1::citext`, email); err != nil {
		t.Fatal(err)
	}
	if rec := in.refreshWithCookie(cookieNamed(r, "sa_refresh").Value); rec.Code != http.StatusUnauthorized {
		t.Fatalf("suspended user refreshed: %d", rec.Code)
	}
	if rec := in.login(email, goodPassword); rec.Code != http.StatusForbidden {
		t.Fatalf("suspended login: %d", rec.Code)
	}
}

func TestIntegration_RealLockoutAndRateLimit(t *testing.T) {
	in := newIntegration(t)
	email := uniqueEmail("brute")
	in.register(email)

	for i := 0; i < 5; i++ {
		if rec := in.login(email, "wrong password!"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	rec := in.login(email, goodPassword)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || errCode(t, rec) != "too_many_attempts" {
		t.Fatalf("lockout: %d %v", rec.Code, rec.Header())
	}
	if rec := in.login(email, goodPassword, withIP(randomIP())); rec.Code != http.StatusOK {
		t.Fatalf("the real owner on another IP must not be locked out: %d", rec.Code)
	}

	// real Redis rate limit: the 6th registration from one IP in an hour is refused
	ip := randomIP()
	for i := 0; i < 5; i++ {
		if rec := in.do(post, "/v1/auth/register", reg(uniqueEmail("rl"), goodPassword, "U"), withIP(ip)); rec.Code != http.StatusCreated {
			t.Fatalf("register %d: %d", i, rec.Code)
		}
	}
	if rec := in.do(post, "/v1/auth/register", reg(uniqueEmail("rl"), goodPassword, "U"), withIP(ip)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("register limit: %d", rec.Code)
	}
}

func TestIntegration_ExpiredTokensWithRealDatabase(t *testing.T) {
	in := newIntegration(t)
	email := uniqueEmail("exp")
	s := in.register(email)
	verify := tokenIn(t, mustLast(t, in.harness, email, "Confirm"))

	in.clock.Advance(25 * time.Hour)
	if rec := in.do(post, "/v1/auth/verify-email", map[string]string{"token": verify}); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired verification link: %d", rec.Code)
	}
	if rec := in.do(get, "/v1/me", nil, withBearer(s.access)); rec.Code != http.StatusUnauthorized || errCode(t, rec) != "token_expired" {
		t.Fatalf("expired access token: %d", rec.Code)
	}
	in.clock.Advance(31 * 24 * time.Hour)
	if rec := in.refreshWithCookie(s.refresh); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired refresh token: %d", rec.Code)
	}
}
