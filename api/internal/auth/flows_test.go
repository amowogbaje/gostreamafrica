package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"streamafrica/api/internal/auth"
)

const (
	post  = http.MethodPost
	get   = http.MethodGet
	patch = http.MethodPatch
)

func reg(email, pw, name string) map[string]string {
	return map[string]string{"email": email, "password": pw, "display_name": name}
}

// ---------------------------------------------------------------- register

func TestRegister_Success(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("ada")
	rec := h.do(post, "/v1/auth/register", reg(email, goodPassword, "Ada"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	b := decode[authBody](t, rec)
	if b.TokenType != "Bearer" || b.ExpiresIn != 900 || b.AccessToken == "" {
		t.Fatalf("bad token fields: %+v", b)
	}
	if b.RefreshToken != "" {
		t.Fatal("web clients must not get the refresh token in the body")
	}
	if b.User.Role != auth.RoleViewer || b.User.Status != auth.StatusActive || b.User.EmailVerifiedAt != nil {
		t.Fatalf("bad user: %+v", b.User)
	}
	if b.Profile.DisplayName != "Ada" || b.Profile.PreferredLanguage != "en" {
		t.Fatalf("bad profile: %+v", b.Profile)
	}
	c := cookieNamed(rec, "sa_refresh")
	if c == nil || c.Value == "" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.Path != "/" || !c.Expires.After(time.Now()) {
		t.Fatalf("bad refresh cookie: %+v", c)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("auth responses must not be cached")
	}
	if s := rec.Body.String(); strings.Contains(s, "argon2") || strings.Contains(s, "password") {
		t.Fatalf("response leaks credentials: %s", s)
	}
	if h.mem != nil && !strings.HasPrefix(h.mem.storedHash(email), "$argon2id$") {
		t.Fatal("password is not stored as an argon2id hash")
	}
	claims, err := h.tokens.Verify(b.AccessToken)
	if err != nil || claims.UserID != b.User.ID || claims.Role != auth.RoleViewer {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	msg, ok := h.mail.last(email, "Confirm")
	if !ok || !strings.Contains(msg.Body, "http://localhost:8088/verify-email#token=") {
		t.Fatalf("verification email missing or wrong: %+v", msg)
	}
}

func TestRegister_CookieSettingsFromConfig(t *testing.T) {
	h := newHarness(t, options{secure: true, cookiePth: "/api/v1/auth"})
	rec := h.do(post, "/v1/auth/register", reg(uniqueEmail("a"), goodPassword, "A"))
	c := cookieNamed(rec, "sa_refresh")
	if c == nil || !c.Secure || c.Path != "/api/v1/auth" {
		t.Fatalf("cookie: %+v", c)
	}
}

func TestRegister_Validation(t *testing.T) {
	h := newHarness(t, options{})
	cases := []struct {
		name, field string
		body        map[string]string
	}{
		{"bad email", "email", reg("nope", goodPassword, "Ada")},
		{"short password", "password", reg(uniqueEmail("a"), "short", "Ada")},
		{"common password", "password", reg(uniqueEmail("a"), "password123", "Ada")},
		{"password equals email", "password", reg("adalovelace@example.com", "adalovelace", "Ada")},
		{"missing name", "display_name", reg(uniqueEmail("a"), goodPassword, "")},
		{"name too long", "display_name", reg(uniqueEmail("a"), goodPassword, strings.Repeat("n", 81))},
	}
	for _, c := range cases {
		rec := h.do(post, "/v1/auth/register", c.body, withIP(randomIP()))
		if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec) != "validation_failed" || !strings.Contains(rec.Body.String(), `"`+c.field+`"`) {
			t.Errorf("%s: code=%d body=%s", c.name, rec.Code, rec.Body.String())
		}
	}
}

func TestRegister_DuplicateAndMassAssignment(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("dup")
	h.register(email)
	rec := h.do(post, "/v1/auth/register", reg(strings.ToUpper(email), goodPassword, "Again"), withIP(randomIP()))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "email_taken" {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
	for _, extra := range []string{`"role":"admin"`, `"status":"active"`, `"email_verified_at":"2020-01-01T00:00:00Z"`, `"id":"x"`} {
		body := `{"email":"` + uniqueEmail("mass") + `","password":"` + goodPassword + `","display_name":"M",` + extra + `}`
		if rec := h.do(post, "/v1/auth/register", body, withIP(randomIP())); rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_json" {
			t.Errorf("mass assignment %s accepted: %d %s", extra, rec.Code, rec.Body.String())
		}
	}
}

func TestRequestShape(t *testing.T) {
	h := newHarness(t, options{})
	if rec := h.do(post, "/v1/auth/register", reg(uniqueEmail("a"), goodPassword, "A"), withHeader("Content-Type", "text/plain")); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("content type: %d", rec.Code)
	}
	big := `{"email":"a@b.co","password":"` + strings.Repeat("x", 9000) + `","display_name":"A"}`
	if rec := h.do(post, "/v1/auth/register", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", rec.Code)
	}
	if rec := h.do(get, "/v1/auth/login", nil); rec.Code != http.StatusMethodNotAllowed || errCode(t, rec) != "method_not_allowed" {
		t.Errorf("method: %d", rec.Code)
	}
	if rec := h.do(post, "/v1/me", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/me: %d", rec.Code)
	}
}

// ---------------------------------------------------------------- email verification

func TestVerifyEmail(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("v")
	s := h.register(email)
	msg, _ := h.mail.last(email, "Confirm")
	tok := tokenIn(t, msg)

	if rec := h.do(post, "/v1/auth/verify-email", map[string]string{"token": tok}); rec.Code != http.StatusNoContent {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	me := decode[auth.Me](t, h.do(get, "/v1/me", nil, withBearer(s.access)))
	if me.User.EmailVerifiedAt == nil {
		t.Fatal("email not marked verified")
	}
	// single use
	if rec := h.do(post, "/v1/auth/verify-email", map[string]string{"token": tok}); rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_or_expired_token" {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{"abc", strings.Repeat("A", 43), "", strings.Repeat("A", 500)} {
		if rec := h.do(post, "/v1/auth/verify-email", map[string]string{"token": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("token %q: %d", bad, rec.Code)
		}
	}
}

func TestVerifyEmail_ExpiryAndNewerLinkWins(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("v")
	h.register(email)
	first := tokenIn(t, mustLast(t, h, email, "Confirm"))

	h.do(post, "/v1/auth/resend-verification", map[string]string{"email": email})
	second := tokenIn(t, mustLast(t, h, email, "Confirm"))
	if first == second {
		t.Fatal("resend must issue a new token")
	}
	if rec := h.do(post, "/v1/auth/verify-email", map[string]string{"token": first}); rec.Code != http.StatusBadRequest {
		t.Fatalf("older link still works: %d", rec.Code)
	}
	h.clock.Advance(25 * time.Hour)
	if rec := h.do(post, "/v1/auth/verify-email", map[string]string{"token": second}); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired link works: %d", rec.Code)
	}
}

func mustLast(t *testing.T, h *harness, to, subject string) auth.Message {
	t.Helper()
	m, ok := h.mail.last(to, subject)
	if !ok {
		t.Fatalf("no %q email to %s", subject, to)
	}
	return m
}

func TestResendVerification_NeverRevealsAccounts(t *testing.T) {
	h := newHarness(t, options{})
	unverified, verified := uniqueEmail("u"), uniqueEmail("v")
	h.register(unverified)
	h.register(verified)
	h.do(post, "/v1/auth/verify-email", map[string]string{"token": tokenIn(t, mustLast(t, h, verified, "Confirm"))})

	before := h.mail.count()
	var bodies []string
	for _, e := range []string{uniqueEmail("ghost"), verified} {
		rec := h.do(post, "/v1/auth/resend-verification", map[string]string{"email": e})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: %d", e, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	if h.mail.count() != before {
		t.Fatal("mail sent for an unknown or already verified account")
	}
	rec := h.do(post, "/v1/auth/resend-verification", map[string]string{"email": unverified})
	if rec.Code != http.StatusAccepted || rec.Body.String() != bodies[0] || h.mail.count() != before+1 {
		t.Fatalf("known account: %d (response must equal the unknown-account response)", rec.Code)
	}
	// per-account limit: over 3/hour the answer stays 202 but no more mail is sent
	for i := 0; i < 4; i++ {
		h.do(post, "/v1/auth/resend-verification", map[string]string{"email": unverified})
	}
	if got := h.mail.count(); got > before+3 {
		t.Fatalf("account limit not enforced: %d mails", got-before)
	}
}

// ---------------------------------------------------------------- login

func TestLogin_SuccessAndUnverifiedAllowed(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("l")
	h.register(email)
	rec := h.login(strings.ToUpper(email), goodPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	b := decode[authBody](t, rec)
	if b.AccessToken == "" || b.User.EmailVerifiedAt != nil || b.RefreshToken != "" || cookieNamed(rec, "sa_refresh") == nil {
		t.Fatalf("bad login response: %+v", b)
	}
	if h.tracker != nil && h.tracker.succeeded != 1 {
		t.Fatal("tracker not told about the success")
	}
}

func TestLogin_FailuresAreIndistinguishable(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("l")
	h.register(email)
	wrong := h.login(email, "wrong password!")
	unknown := h.login(uniqueEmail("ghost"), "wrong password!")
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("codes %d / %d", wrong.Code, unknown.Code)
	}
	strip := func(s string) string {
		var m map[string]map[string]any
		_ = json.Unmarshal([]byte(s), &m)
		delete(m["error"], "request_id")
		b, _ := json.Marshal(m)
		return string(b)
	}
	if strip(wrong.Body.String()) != strip(unknown.Body.String()) || errCode(t, wrong) != "invalid_credentials" {
		t.Fatalf("responses differ:\n%s\n%s", wrong.Body.String(), unknown.Body.String())
	}
	if cookieNamed(wrong, "sa_refresh") != nil || cookieNamed(unknown, "sa_refresh") != nil {
		t.Fatal("failed login must not set a cookie")
	}
	if h.tracker != nil && h.tracker.failed != 2 {
		t.Fatalf("both failures must be recorded (so lockout cannot reveal which accounts exist): %d", h.tracker.failed)
	}
}

func TestLogin_SuspendedAccount(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("s")
	h.register(email)
	h.setStatus(t, email, auth.StatusSuspended)
	if rec := h.login(email, goodPassword); rec.Code != http.StatusForbidden || errCode(t, rec) != "account_disabled" {
		t.Fatalf("suspended: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.login(email, "wrong password!"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password on a suspended account must look like any other: %d", rec.Code)
	}
}

func (h *harness) setStatus(t *testing.T, email string, s auth.Status) {
	t.Helper()
	if h.mem == nil {
		t.Skip("needs the in-memory store")
	}
	h.mem.setStatus(email, s)
}

func TestLogin_LockoutWiring(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("lock")
	h.register(email)
	for i := 0; i < 5; i++ {
		if rec := h.login(email, "wrong password!"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	// locked: even the CORRECT password is refused, before any password check
	rec := h.login(email, goodPassword)
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec) != "too_many_attempts" || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("locked: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	// another IP is not locked
	if rec := h.login(email, goodPassword, withIP(randomIP())); rec.Code != http.StatusOK {
		t.Fatalf("other IP: %d", rec.Code)
	}
}

func TestLogin_SuccessResetsFailures(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("reset")
	h.register(email)
	for i := 0; i < 3; i++ {
		h.login(email, "wrong password!")
	}
	if rec := h.login(email, goodPassword); rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	for i := 0; i < 4; i++ {
		if rec := h.login(email, "wrong password!"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure counter was not reset: attempt %d -> %d", i, rec.Code)
		}
	}
}

func TestLogin_FailsClosedWhenTrackerDown(t *testing.T) {
	tr := newFakeTracker()
	tr.err = errors.New("redis down")
	h := newHarness(t, options{tracker: tr})
	if rec := h.login("a@example.com", "whatever12345"); rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "rate_limiter_unavailable" {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLogin_Validation(t *testing.T) {
	h := newHarness(t, options{})
	if rec := h.do(post, "/v1/auth/login", `{"email":"","password":""}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestLogin_UpgradesWeakHash(t *testing.T) {
	h := newHarness(t, options{})
	if h.mem == nil {
		t.Skip("needs the in-memory store")
	}
	weak, _ := auth.NewHasher(auth.Params{MemoryKiB: 32, Time: 1, Parallelism: 1})
	old, _ := weak.Hash(context.Background(), goodPassword)
	email := uniqueEmail("rehash")
	if _, err := h.mem.CreateUser(context.Background(), email, old, "R"); err != nil {
		t.Fatal(err)
	}
	if rec := h.login(email, goodPassword); rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	if now := h.mem.storedHash(email); now == old || !strings.Contains(now, "m=64,t=1,p=1") {
		t.Fatalf("hash not upgraded: %s", now)
	}
}

// ---------------------------------------------------------------- refresh tokens

func TestRefresh_RotationAndReuseDetection(t *testing.T) {
	h := newHarness(t, options{})
	s := h.register(uniqueEmail("r"))

	rec := h.refreshWithCookie(s.refresh)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	b := decode[authBody](t, rec)
	c2 := cookieNamed(rec, "sa_refresh")
	if c2 == nil || c2.Value == "" || c2.Value == s.refresh {
		t.Fatal("refresh token was not rotated")
	}
	if b.RefreshToken != "" {
		t.Fatal("a cookie client must never receive the refresh token in the body")
	}
	if me := h.do(get, "/v1/me", nil, withBearer(b.AccessToken)); me.Code != http.StatusOK {
		t.Fatalf("new access token rejected: %d", me.Code)
	}

	// The OLD token comes back (a stolen copy): rejected, and the whole family is burned.
	stolen := h.refreshWithCookie(s.refresh)
	if stolen.Code != http.StatusUnauthorized || errCode(t, stolen) != "invalid_refresh_token" {
		t.Fatalf("reuse: %d %s", stolen.Code, stolen.Body.String())
	}
	if cleared := cookieNamed(stolen, "sa_refresh"); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("cookie not cleared on reuse: %+v", cleared)
	}
	if legit := h.refreshWithCookie(c2.Value); legit.Code != http.StatusUnauthorized {
		t.Fatalf("family survived reuse: the newest token still works (%d)", legit.Code)
	}
}

func TestRefresh_OtherSessionsSurviveReuseOfOneFamily(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("fam")
	first := h.register(email)
	second := decode[authBody](t, h.login(email, goodPassword))
	_ = second
	// login created a second family; recover its cookie
	rec := h.login(email, goodPassword)
	other := cookieNamed(rec, "sa_refresh").Value

	h.refreshWithCookie(first.refresh)
	h.refreshWithCookie(first.refresh) // reuse: burns only the first family
	if rec := h.refreshWithCookie(other); rec.Code != http.StatusOK {
		t.Fatalf("an unrelated session was revoked: %d", rec.Code)
	}
}

func TestRefresh_InvalidInputs(t *testing.T) {
	h := newHarness(t, options{})
	if rec := h.do(post, "/v1/auth/refresh", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	for _, bad := range []string{"garbage", strings.Repeat("A", 43), strings.Repeat("!", 43), strings.Repeat("A", 500)} {
		if rec := h.refreshWithCookie(bad); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", bad, rec.Code)
		}
	}
}

func TestRefresh_ExpiryAndAbsoluteLifetime(t *testing.T) {
	h := newHarness(t, options{})
	idle := h.register(uniqueEmail("idle"))
	h.clock.Advance(31 * 24 * time.Hour)
	if rec := h.refreshWithCookie(idle.refresh); rec.Code != http.StatusUnauthorized {
		t.Fatalf("idle expiry: %d", rec.Code)
	}

	// Keeping a session alive by refreshing every 29 days still ends at the 90 day cap.
	s := h.register(uniqueEmail("cap"))
	tok := s.refresh
	for i := 0; i < 3; i++ {
		h.clock.Advance(29 * 24 * time.Hour)
		rec := h.refreshWithCookie(tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("refresh %d: %d", i, rec.Code)
		}
		tok = cookieNamed(rec, "sa_refresh").Value
	}
	h.clock.Advance(5 * 24 * time.Hour) // 92 days after login
	if rec := h.refreshWithCookie(tok); rec.Code != http.StatusUnauthorized {
		t.Fatalf("absolute lifetime not enforced: %d", rec.Code)
	}
}

func TestRefresh_BodyTransportForMobile(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("m")
	h.register(email)

	rec := h.login(email, goodPassword, withHeader("X-Auth-Transport", "body"))
	b := decode[authBody](t, rec)
	if rec.Code != http.StatusOK || b.RefreshToken == "" || b.RefreshExpiresIn <= 0 || cookieNamed(rec, "sa_refresh") != nil {
		t.Fatalf("body transport login: %d %+v cookie=%v", rec.Code, b, cookieNamed(rec, "sa_refresh"))
	}

	r2 := h.do(post, "/v1/auth/refresh", map[string]string{"refresh_token": b.RefreshToken})
	b2 := decode[authBody](t, r2)
	if r2.Code != http.StatusOK || b2.RefreshToken == "" || b2.RefreshToken == b.RefreshToken || cookieNamed(r2, "sa_refresh") != nil {
		t.Fatalf("body refresh: %d %+v", r2.Code, b2)
	}
	if reuse := h.do(post, "/v1/auth/refresh", map[string]string{"refresh_token": b.RefreshToken}); reuse.Code != http.StatusUnauthorized {
		t.Fatalf("body reuse: %d", reuse.Code)
	}
	if dead := h.do(post, "/v1/auth/refresh", map[string]string{"refresh_token": b2.RefreshToken}); dead.Code != http.StatusUnauthorized {
		t.Fatalf("family survived body reuse: %d", dead.Code)
	}
	if out := h.do(post, "/v1/auth/logout", map[string]string{"refresh_token": "x"}); out.Code != http.StatusNoContent {
		t.Fatalf("body logout with unknown token: %d", out.Code)
	}
}

func TestRefresh_CookieRequestsAreOriginChecked(t *testing.T) {
	h := newHarness(t, options{origins: []string{"http://localhost:3000"}})
	s := h.register(uniqueEmail("o"))

	evil := h.refreshWithCookie(s.refresh, withHeader("Origin", "https://evil.example"))
	if evil.Code != http.StatusForbidden || errCode(t, evil) != "forbidden_origin" {
		t.Fatalf("cross-site refresh: %d %s", evil.Code, evil.Body.String())
	}
	if rec := h.refreshWithCookie(s.refresh, withHeader("Origin", "null")); rec.Code != http.StatusForbidden {
		t.Fatalf("null origin: %d", rec.Code)
	}
	// the rejected requests did not burn the token
	ok := h.refreshWithCookie(s.refresh, withHeader("Origin", "http://localhost:3000"))
	if ok.Code != http.StatusOK {
		t.Fatalf("allowed origin: %d", ok.Code)
	}
	if rec := h.do(post, "/v1/auth/logout", nil, withCookie("sa_refresh", cookieNamed(ok, "sa_refresh").Value), withHeader("Origin", "https://evil.example")); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site logout: %d", rec.Code)
	}
}

func TestRefresh_SuspendedUserLosesSessions(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("susp")
	s := h.register(email)
	h.setStatus(t, email, auth.StatusSuspended)
	if rec := h.refreshWithCookie(s.refresh); rec.Code != http.StatusUnauthorized {
		t.Fatalf("suspended refresh: %d", rec.Code)
	}
}

func TestLogoutAndLogoutAll(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("lo")
	a := h.register(email)
	bRec := h.login(email, goodPassword)
	b := session{access: decode[authBody](t, bRec).AccessToken, refresh: cookieNamed(bRec, "sa_refresh").Value}

	out := h.do(post, "/v1/auth/logout", nil, withCookie("sa_refresh", a.refresh))
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", out.Code)
	}
	if c := cookieNamed(out, "sa_refresh"); c == nil || c.MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %+v", c)
	}
	if rec := h.refreshWithCookie(a.refresh); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session still alive after logout: %d", rec.Code)
	}
	if rec := h.refreshWithCookie(b.refresh); rec.Code != http.StatusOK {
		t.Fatalf("logout must only end ONE session: %d", rec.Code)
	}
	if rec := h.do(post, "/v1/auth/logout", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("logout without cookie: %d", rec.Code)
	}

	// logout-all needs a valid access token and ends every session
	third := decode[authBody](t, h.login(email, goodPassword))
	fourthRec := h.login(email, goodPassword)
	if rec := h.do(post, "/v1/auth/logout-all", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout-all without token: %d", rec.Code)
	}
	if rec := h.do(post, "/v1/auth/logout-all", nil, withBearer(third.AccessToken)); rec.Code != http.StatusNoContent {
		t.Fatalf("logout-all: %d", rec.Code)
	}
	if rec := h.refreshWithCookie(cookieNamed(fourthRec, "sa_refresh").Value); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout-all left a session alive: %d", rec.Code)
	}
}

// ---------------------------------------------------------------- password reset

func TestForgotAndResetPassword(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("f")
	s := h.register(email)

	unknown := h.do(post, "/v1/auth/forgot-password", map[string]string{"email": uniqueEmail("ghost")})
	before := h.mail.count()
	known := h.do(post, "/v1/auth/forgot-password", map[string]string{"email": email})
	if unknown.Code != http.StatusAccepted || known.Code != http.StatusAccepted || unknown.Body.String() != known.Body.String() {
		t.Fatalf("responses must be identical: %d %q / %d %q", unknown.Code, unknown.Body.String(), known.Code, known.Body.String())
	}
	if h.mail.count() != before+1 {
		t.Fatal("reset email not sent to the known account (or sent for the unknown one)")
	}
	tok := tokenIn(t, mustLast(t, h, email, "Reset"))

	// a weak new password is rejected BEFORE the token is consumed
	weak := h.do(post, "/v1/auth/reset-password", map[string]string{"token": tok, "new_password": "password123"})
	if weak.Code != http.StatusUnprocessableEntity {
		t.Fatalf("weak: %d", weak.Code)
	}
	if rec := h.do(post, "/v1/auth/reset-password", map[string]string{"token": tok, "new_password": "a brand new passphrase"}); rec.Code != http.StatusNoContent {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.login(email, goodPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password still works: %d", rec.Code)
	}
	if rec := h.login(email, "a brand new passphrase"); rec.Code != http.StatusOK {
		t.Fatalf("new password rejected: %d", rec.Code)
	}
	if rec := h.refreshWithCookie(s.refresh); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reset must sign out existing sessions: %d", rec.Code)
	}
	// single use
	if rec := h.do(post, "/v1/auth/reset-password", map[string]string{"token": tok, "new_password": "another good passphrase"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("token reused: %d", rec.Code)
	}
}

func TestResetPassword_ExpiryAndSupersededLinks(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("e")
	h.register(email)
	h.do(post, "/v1/auth/forgot-password", map[string]string{"email": email})
	first := tokenIn(t, mustLast(t, h, email, "Reset"))
	h.do(post, "/v1/auth/forgot-password", map[string]string{"email": email})
	second := tokenIn(t, mustLast(t, h, email, "Reset"))

	if rec := h.do(post, "/v1/auth/reset-password", map[string]string{"token": first, "new_password": "a brand new passphrase"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("superseded link works: %d", rec.Code)
	}
	h.clock.Advance(61 * time.Minute)
	if rec := h.do(post, "/v1/auth/reset-password", map[string]string{"token": second, "new_password": "a brand new passphrase"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired link works: %d", rec.Code)
	}
	if rec := h.do(post, "/v1/auth/reset-password", map[string]string{"new_password": "a brand new passphrase"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing token: %d", rec.Code)
	}
}

func TestEmailTokensAreNotInterchangeable(t *testing.T) {
	h := newHarness(t, options{})
	email := uniqueEmail("x")
	h.register(email)
	verifyTok := tokenIn(t, mustLast(t, h, email, "Confirm"))
	if rec := h.do(post, "/v1/auth/reset-password", map[string]string{"token": verifyTok, "new_password": "a brand new passphrase"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("verification token accepted as a reset token: %d", rec.Code)
	}
}

// ---------------------------------------------------------------- /v1/me

func TestMe_GetAndPatch(t *testing.T) {
	h := newHarness(t, options{})
	s := h.register(uniqueEmail("me"))

	if rec := h.do(get, "/v1/me", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	me := decode[auth.Me](t, h.do(get, "/v1/me", nil, withBearer(s.access)))
	if me.User.ID != s.id || me.Profile.DisplayName != "Test User" {
		t.Fatalf("me: %+v", me)
	}

	rec := h.do(patch, "/v1/me", `{"display_name":"  Ọláolú  ","country":"NG","preferred_language":"yo","avatar_url":"https://cdn.example.com/a.png"}`, withBearer(s.access))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	p := decode[auth.Me](t, rec).Profile
	if p.DisplayName != "Ọláolú" || p.Country == nil || *p.Country != "NG" || p.PreferredLanguage != "yo" || p.AvatarURL == nil {
		t.Fatalf("profile: %+v", p)
	}
	cleared := decode[auth.Me](t, h.do(patch, "/v1/me", `{"avatar_url":null,"country":null}`, withBearer(s.access))).Profile
	if cleared.AvatarURL != nil || cleared.Country != nil || cleared.DisplayName != "Ọláolú" {
		t.Fatalf("clear: %+v", cleared)
	}

	for name, body := range map[string]string{
		"empty":        `{}`,
		"null name":    `{"display_name":null}`,
		"bad country":  `{"country":"nigeria"}`,
		"http avatar":  `{"avatar_url":"http://x.example.com/a.png"}`,
		"bad language": `{"preferred_language":"English"}`,
	} {
		if rec := h.do(patch, "/v1/me", body, withBearer(s.access)); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestMe_CannotChangeRoleOrIdentity(t *testing.T) {
	h := newHarness(t, options{})
	s := h.register(uniqueEmail("esc"))
	for _, body := range []string{`{"role":"admin"}`, `{"status":"active"}`, `{"email":"x@example.com"}`, `{"email_verified_at":"2020-01-01T00:00:00Z"}`, `{"user_id":"other"}`} {
		if rec := h.do(patch, "/v1/me", body, withBearer(s.access)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d", body, rec.Code)
		}
	}
	if me := decode[auth.Me](t, h.do(get, "/v1/me", nil, withBearer(s.access))); me.User.Role != auth.RoleViewer {
		t.Fatalf("role changed: %s", me.User.Role)
	}
}

// ---------------------------------------------------------------- authorization

func forgeToken(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthn_RejectsBadAndForgedTokens(t *testing.T) {
	h := newHarness(t, options{})
	s := h.register(uniqueEmail("z"))
	parts := strings.Split(s.access, ".")
	var payload map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &payload)
	payload["role"] = "admin"
	edited, _ := json.Marshal(payload)
	escalated := parts[0] + "." + base64.RawURLEncoding.EncodeToString(edited) + "." + parts[2]

	claims := jwt.MapClaims{
		"iss": "streamafrica", "aud": "streamafrica-api", "sub": s.id, "role": "admin",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	cases := map[string]struct {
		header string
		want   int
		code   string
	}{
		"no header":            {"", 401, "unauthenticated"},
		"basic scheme":         {"Basic abc", 401, "unauthenticated"},
		"empty bearer":         {"Bearer ", 401, "unauthenticated"},
		"garbage":              {"Bearer not.a.jwt", 401, "invalid_token"},
		"payload role swapped": {"Bearer " + escalated, 401, "invalid_token"},
		"alg none":             {"Bearer " + forgeToken(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claims), 401, "invalid_token"},
		"guessed secret":       {"Bearer " + forgeToken(t, jwt.SigningMethodHS256, []byte("secret"), claims), 401, "invalid_token"},
		"other key, admin":     {"Bearer " + forgeToken(t, jwt.SigningMethodHS256, []byte("an-entirely-different-secret-0123456789"), claims), 401, "invalid_token"},
		"HS512 same key":       {"Bearer " + forgeToken(t, jwt.SigningMethodHS512, testSecret, claims), 401, "invalid_token"},
	}
	for name, c := range cases {
		rec := h.do(get, "/test/admin", nil, withHeader("Authorization", c.header))
		if rec.Code != c.want || errCode(t, rec) != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: missing WWW-Authenticate", name)
		}
	}
}

func TestAuthn_ExpiredAccessToken(t *testing.T) {
	h := newHarness(t, options{})
	s := h.register(uniqueEmail("exp"))
	if rec := h.do(get, "/v1/me", nil, withBearer(s.access)); rec.Code != http.StatusOK {
		t.Fatalf("fresh token: %d", rec.Code)
	}
	h.clock.Advance(15*time.Minute + 10*time.Second)
	rec := h.do(get, "/v1/me", nil, withBearer(s.access))
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "token_expired" {
		t.Fatalf("expired token: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthz_RolesAndEscalation(t *testing.T) {
	h := newHarness(t, options{})
	if h.mem == nil {
		t.Skip("needs the in-memory store")
	}
	email := uniqueEmail("role")
	h.register(email)
	signIn := func() string {
		rec := h.login(email, goodPassword)
		return decode[authBody](t, rec).AccessToken
	}

	viewer := signIn()
	for _, path := range []string{"/test/admin", "/test/creator"} {
		if rec := h.do(get, path, nil, withBearer(viewer)); rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
			t.Errorf("viewer on %s: %d", path, rec.Code)
		}
	}

	h.mem.setRole(email, auth.RoleCreator)
	// Roles ride in the access token, so a promotion applies after the next sign-in or refresh.
	if rec := h.do(get, "/test/creator", nil, withBearer(viewer)); rec.Code != http.StatusForbidden {
		t.Fatalf("old token must keep its old role until it expires: %d", rec.Code)
	}
	creator := signIn()
	if rec := h.do(get, "/test/creator", nil, withBearer(creator)); rec.Code != http.StatusNoContent {
		t.Errorf("creator on /test/creator: %d", rec.Code)
	}
	if rec := h.do(get, "/test/admin", nil, withBearer(creator)); rec.Code != http.StatusForbidden {
		t.Errorf("creator on /test/admin: %d", rec.Code)
	}

	h.mem.setRole(email, auth.RoleAdmin)
	admin := signIn()
	for _, path := range []string{"/test/admin", "/test/creator"} {
		if rec := h.do(get, path, nil, withBearer(admin)); rec.Code != http.StatusNoContent {
			t.Errorf("admin on %s: %d", path, rec.Code)
		}
	}
}

func TestAuthz_RoleChangeAppliesOnRefresh(t *testing.T) {
	h := newHarness(t, options{})
	if h.mem == nil {
		t.Skip("needs the in-memory store")
	}
	email := uniqueEmail("promo")
	s := h.register(email)
	h.mem.setRole(email, auth.RoleAdmin)
	rec := h.refreshWithCookie(s.refresh)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d", rec.Code)
	}
	if rec := h.do(get, "/test/admin", nil, withBearer(decode[authBody](t, rec).AccessToken)); rec.Code != http.StatusNoContent {
		t.Fatalf("promotion not picked up on refresh: %d", rec.Code)
	}
}

func TestOwnershipHelpers(t *testing.T) {
	owner := auth.Principal{UserID: "creator-a", Role: auth.RoleCreator}
	other := auth.Principal{UserID: "creator-b", Role: auth.RoleCreator}
	admin := auth.Principal{UserID: "admin-1", Role: auth.RoleAdmin}
	viewer := auth.Principal{UserID: "creator-a", Role: auth.RoleViewer}
	cases := []struct {
		name  string
		p     auth.Principal
		owner string
		ok    bool
	}{
		{"owner", owner, "creator-a", true},
		{"someone else", other, "creator-a", false},
		{"admin", admin, "creator-a", true},
		{"no owner recorded", other, "", false},
		{"empty ids never match", auth.Principal{}, "", false},
	}
	for _, c := range cases {
		if err := auth.AssertOwner(c.p, c.owner); (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", c.name, err, c.ok)
		}
	}
	_ = viewer

	lookup := func(_ context.Context, id string) (string, error) {
		if id == "t1" {
			return "creator-a", nil
		}
		return "", auth.ErrNotFound
	}
	if err := auth.AssertTitleOwner(context.Background(), lookup, owner, "t1"); err != nil {
		t.Errorf("owner: %v", err)
	}
	if err := auth.AssertTitleOwner(context.Background(), lookup, other, "t1"); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("other: %v", err)
	}
	if err := auth.AssertTitleOwner(context.Background(), lookup, owner, "nope"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestOwnershipMiddleware(t *testing.T) {
	owners := map[string]string{}
	h := newHarness(t, options{owners: owners})
	if h.mem == nil {
		t.Skip("needs the in-memory store")
	}
	mk := func(role auth.Role) (string, string) {
		email := uniqueEmail(string(role))
		h.register(email)
		h.mem.setRole(email, role)
		b := decode[authBody](t, h.login(email, goodPassword))
		return b.AccessToken, b.User.ID
	}
	aTok, aID := mk(auth.RoleCreator)
	bTok, _ := mk(auth.RoleCreator)
	adminTok, _ := mk(auth.RoleAdmin)
	viewerTok := h.register(uniqueEmail("viewer")).access
	owners["title-1"] = aID

	cases := []struct {
		name, tok, path string
		want            int
	}{
		{"owner", aTok, "/test/title/title-1", 204},
		{"other creator", bTok, "/test/title/title-1", 403},
		{"admin", adminTok, "/test/title/title-1", 204},
		{"viewer blocked by role", viewerTok, "/test/title/title-1", 403},
		{"unknown title", aTok, "/test/title/missing", 404},
	}
	for _, c := range cases {
		if rec := h.do(get, c.path, nil, withBearer(c.tok)); rec.Code != c.want {
			t.Errorf("%s: %d want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
	if rec := h.do(get, "/test/title/title-1", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", rec.Code)
	}
}

// ---------------------------------------------------------------- rate limits

func TestRateLimits(t *testing.T) {
	h := newHarness(t, options{})
	for i := 0; i < 5; i++ {
		if rec := h.do(post, "/v1/auth/register", reg(uniqueEmail("rl"), goodPassword, "U")); rec.Code != http.StatusCreated {
			t.Fatalf("register %d: %d", i, rec.Code)
		}
	}
	rec := h.do(post, "/v1/auth/register", reg(uniqueEmail("rl"), goodPassword, "U"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "42" || errCode(t, rec) != "rate_limited" {
		t.Fatalf("register limit: %d %v", rec.Code, rec.Header())
	}

	h2 := newHarness(t, options{})
	for i := 0; i < 20; i++ {
		if rec := h2.login(uniqueEmail("g"), "whatever12345"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("login %d: %d", i, rec.Code)
		}
	}
	if rec := h2.login(uniqueEmail("g"), "whatever12345"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login-ip limit: %d", rec.Code)
	}
}

func TestRateLimiterDown_FailsClosed(t *testing.T) {
	lim := newFakeLimiter()
	lim.err = errors.New("redis down")
	h := newHarness(t, options{limiter: lim})
	for _, path := range []string{"/v1/auth/register", "/v1/auth/login", "/v1/auth/refresh", "/v1/auth/forgot-password", "/v1/auth/verify-email", "/v1/auth/reset-password"} {
		rec := h.do(post, path, `{}`)
		if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "rate_limiter_unavailable" {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
