package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/ratelimit"
	"streamafrica/api/internal/platform/server"
)

// ---- fakes

type fakeStore struct {
	mu       sync.Mutex
	users    map[string]*rec // key: lower-case email
	sessions map[string]string
	now      time.Time
	expiry   map[string]time.Time
}

type rec struct {
	user auth.User
	hash string
}

func newFakeStore() *fakeStore {
	return &fakeStore{users: map[string]*rec{}, sessions: map[string]string{}, expiry: map[string]time.Time{}, now: time.Now()}
}

func (f *fakeStore) CreateUser(_ context.Context, email, hash, name string) (auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := strings.ToLower(email)
	if _, ok := f.users[k]; ok {
		return auth.User{}, auth.ErrEmailTaken
	}
	u := auth.User{ID: "id-" + k, Email: email, Name: name, Role: "viewer", CreatedAt: f.now}
	f.users[k] = &rec{user: u, hash: hash}
	return u, nil
}

func (f *fakeStore) UserByEmail(_ context.Context, email string) (auth.User, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.users[strings.ToLower(email)]
	if !ok {
		return auth.User{}, "", auth.ErrNotFound
	}
	return r.user, r.hash, nil
}

func (f *fakeStore) UpdatePasswordHash(_ context.Context, userID, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.users {
		if r.user.ID == userID {
			r.hash = hash
		}
	}
	return nil
}

func (f *fakeStore) CreateSession(_ context.Context, userID string, tokenHash []byte, expiresAt time.Time, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[string(tokenHash)] = userID
	f.expiry[string(tokenHash)] = expiresAt
	return nil
}

func (f *fakeStore) UserBySession(_ context.Context, tokenHash []byte, now time.Time) (auth.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.sessions[string(tokenHash)]
	if !ok || !f.expiry[string(tokenHash)].After(now) {
		return auth.User{}, auth.ErrNotFound
	}
	for _, r := range f.users {
		if r.user.ID == id {
			return r.user, nil
		}
	}
	return auth.User{}, auth.ErrNotFound
}

func (f *fakeStore) DeleteSession(_ context.Context, tokenHash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, string(tokenHash))
	return nil
}

type fakeLimiter struct {
	mu     sync.Mutex
	counts map[string]int64
	err    error
}

func (l *fakeLimiter) Hit(_ context.Context, key string, _ time.Duration) (int64, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return 0, 0, l.err
	}
	l.counts[key]++
	return l.counts[key], 42 * time.Second, nil
}

// ---- harness

type env struct {
	h       http.Handler
	store   *fakeStore
	limiter *fakeLimiter
	svc     *auth.Service
	hasher  *auth.Hasher
}

func newEnv(t *testing.T, secure bool) *env {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	hasher, err := auth.NewHasher(auth.Params{MemoryKiB: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	st := newFakeStore()
	svc, err := auth.NewService(st, hasher, time.Hour, logger, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	lim := &fakeLimiter{counts: map[string]int64{}}
	ah := auth.NewHandler(svc, lim, logger, auth.HandlerConfig{CookieSecure: secure})
	h := server.NewHandler(server.Deps{Logger: logger, Mount: []func(*http.ServeMux){ah.Mount}})
	return &env{h: h, store: st, limiter: lim, svc: svc, hasher: hasher}
}

func (e *env) do(method, path, body, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sa_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "sa_session" {
			return c
		}
	}
	return nil
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	return b.Error.Code
}

const regBody = `{"email":"ada@example.com","password":"correct horse battery","name":"Ada"}`

// ---- tests

func TestRegister_Success(t *testing.T) {
	e := newEnv(t, false)
	rec := e.do(http.MethodPost, "/v1/auth/register", regBody, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "argon2") || strings.Contains(body, "password") {
		t.Fatalf("response leaks credentials: %s", body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Value == "" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure || c.Path != "/" {
		t.Fatalf("bad cookie: %+v", c)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("auth responses must not be cached")
	}
	// stored hash is argon2id, never the plaintext
	if h := e.store.users["ada@example.com"].hash; !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("stored hash = %q", h)
	}
}

func TestRegister_SecureCookieWhenConfigured(t *testing.T) {
	e := newEnv(t, true)
	rec := e.do(http.MethodPost, "/v1/auth/register", regBody, "")
	if c := sessionCookie(rec); c == nil || !c.Secure {
		t.Fatalf("cookie must be Secure: %+v", c)
	}
}

func TestRegister_Validation(t *testing.T) {
	e := newEnv(t, false)
	rec := e.do(http.MethodPost, "/v1/auth/register", `{"email":"nope","password":"short","name":""}`, "")
	if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec) != "validation_failed" {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, f := range []string{`"email"`, `"password"`, `"name"`} {
		if !strings.Contains(rec.Body.String(), f) {
			t.Errorf("missing field %s in %s", f, rec.Body.String())
		}
	}
}

func TestRegister_DuplicateEmailIsCaseInsensitive(t *testing.T) {
	e := newEnv(t, false)
	e.do(http.MethodPost, "/v1/auth/register", regBody, "")
	rec := e.do(http.MethodPost, "/v1/auth/register",
		`{"email":"ADA@example.com","password":"another long password","name":"Ada 2"}`, "")
	if rec.Code != http.StatusConflict || errCode(t, rec) != "email_taken" {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegister_RequestShape(t *testing.T) {
	e := newEnv(t, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(regBody))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type: code=%d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/v1/auth/register", `{"email":"a@b.co","password":"0123456789","name":"A","role":"admin"}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field (mass assignment): code=%d", rec.Code)
	}
	big := `{"email":"a@b.co","password":"` + strings.Repeat("x", 9000) + `","name":"A"}`
	if rec := e.do(http.MethodPost, "/v1/auth/register", big, ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: code=%d", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/v1/auth/register", "", ""); rec.Code != http.StatusMethodNotAllowed || errCode(t, rec) != "method_not_allowed" {
		t.Fatalf("method: code=%d", rec.Code)
	}
}

func TestLogin_MeLogoutFlow(t *testing.T) {
	e := newEnv(t, false)
	e.do(http.MethodPost, "/v1/auth/register", regBody, "")

	rec := e.do(http.MethodPost, "/v1/auth/login", `{"email":"Ada@Example.com","password":"correct horse battery"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login code=%d body=%s", rec.Code, rec.Body.String())
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no cookie on login")
	}

	me := e.do(http.MethodGet, "/v1/auth/me", "", c.Value)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), "ada@example.com") {
		t.Fatalf("me code=%d body=%s", me.Code, me.Body.String())
	}

	out := e.do(http.MethodPost, "/v1/auth/logout", "", c.Value)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout code=%d", out.Code)
	}
	if cleared := sessionCookie(out); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %+v", cleared)
	}
	if after := e.do(http.MethodGet, "/v1/auth/me", "", c.Value); after.Code != http.StatusUnauthorized {
		t.Fatalf("session still valid after logout: %d", after.Code)
	}
}

func TestLogin_FailuresAreIndistinguishable(t *testing.T) {
	e := newEnv(t, false)
	e.do(http.MethodPost, "/v1/auth/register", regBody, "")
	wrong := e.do(http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"wrong password!"}`, "")
	unknown := e.do(http.MethodPost, "/v1/auth/login", `{"email":"ghost@example.com","password":"wrong password!"}`, "")
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("codes %d / %d", wrong.Code, unknown.Code)
	}
	strip := func(s string) string { // request ids differ per request
		var m map[string]map[string]string
		_ = json.Unmarshal([]byte(s), &m)
		delete(m["error"], "request_id")
		b, _ := json.Marshal(m)
		return string(b)
	}
	if strip(wrong.Body.String()) != strip(unknown.Body.String()) {
		t.Fatalf("responses differ:\n%s\n%s", wrong.Body.String(), unknown.Body.String())
	}
	if sessionCookie(wrong) != nil || sessionCookie(unknown) != nil {
		t.Fatal("failed login must not set a cookie")
	}
}

func TestMe_RejectsMissingAndForgedCookies(t *testing.T) {
	e := newEnv(t, false)
	if rec := e.do(http.MethodGet, "/v1/auth/me", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", rec.Code)
	}
	for _, bad := range []string{"garbage", strings.Repeat("A", 43), strings.Repeat("!", 43)} {
		if rec := e.do(http.MethodGet, "/v1/auth/me", "", bad); rec.Code != http.StatusUnauthorized {
			t.Fatalf("forged cookie %q: %d", bad, rec.Code)
		}
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	e := newEnv(t, false)
	rec := e.do(http.MethodPost, "/v1/auth/register", regBody, "")
	token := sessionCookie(rec).Value
	for k := range e.store.expiry {
		e.store.expiry[k] = time.Now().Add(-time.Minute)
	}
	if rec := e.do(http.MethodGet, "/v1/auth/me", "", token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session accepted: %d", rec.Code)
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t, false)
	// register: 5 per IP per hour
	for i := 0; i < 5; i++ {
		body := `{"email":"u` + string(rune('a'+i)) + `@example.com","password":"0123456789","name":"U"}`
		if rec := e.do(http.MethodPost, "/v1/auth/register", body, ""); rec.Code != http.StatusCreated {
			t.Fatalf("register %d: %d", i, rec.Code)
		}
	}
	rec := e.do(http.MethodPost, "/v1/auth/register", `{"email":"x@example.com","password":"0123456789","name":"X"}`, "")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "42" || errCode(t, rec) != "rate_limited" {
		t.Fatalf("register limit: code=%d headers=%v", rec.Code, rec.Header())
	}

	// login: 10 attempts per account per 15 minutes
	e2 := newEnv(t, false)
	e2.do(http.MethodPost, "/v1/auth/register", regBody, "")
	for i := 0; i < 10; i++ {
		if rec := e2.do(http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"wrong password!"}`, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	rec = e2.do(http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"correct horse battery"}`, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("account limit not enforced: %d", rec.Code)
	}
}

func TestRateLimiterDown_FailsClosed(t *testing.T) {
	e := newEnv(t, false)
	e.limiter.err = errors.New("redis down")
	for _, path := range []string{"/v1/auth/register", "/v1/auth/login"} {
		rec := e.do(http.MethodPost, path, regBody, "")
		if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "rate_limiter_unavailable" {
			t.Fatalf("%s: code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestLogin_UpgradesWeakHash(t *testing.T) {
	e := newEnv(t, false)
	weak, _ := auth.NewHasher(auth.Params{MemoryKiB: 32, Time: 1, Parallelism: 1})
	old, _ := weak.Hash(context.Background(), "correct horse battery")
	if _, err := e.store.CreateUser(context.Background(), "ada@example.com", old, "Ada"); err != nil {
		t.Fatal(err)
	}
	rec := e.do(http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"correct horse battery"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	if now := e.store.users["ada@example.com"].hash; now == old || !strings.Contains(now, "m=64,t=1,p=1") {
		t.Fatalf("hash not upgraded: %s", now)
	}
}

func TestAuthenticatedMiddleware(t *testing.T) {
	e := newEnv(t, false)
	rec := e.do(http.MethodPost, "/v1/auth/register", regBody, "")
	token := sessionCookie(rec).Value

	ah := auth.NewHandler(e.svc, e.limiter, slog.New(slog.NewJSONHandler(io.Discard, nil)), auth.HandlerConfig{})
	protected := ah.Authenticated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok || u.Email != "ada@example.com" {
			t.Errorf("user not in context: %+v ok=%v", u, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "sa_session", Value: token})
	out := httptest.NewRecorder()
	protected.ServeHTTP(out, req)
	if out.Code != http.StatusNoContent {
		t.Fatalf("signed-in: %d", out.Code)
	}
	anon := httptest.NewRecorder()
	protected.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/x", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", anon.Code)
	}
}

var _ ratelimit.Store = (*fakeLimiter)(nil)
