package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/ratelimit"
	"streamafrica/api/internal/platform/server"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef-test")

const goodPassword = "correct horse battery"

// ---- clock

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---- fake mailer

type fakeMailer struct {
	mu   sync.Mutex
	sent []auth.Message
}

func (m *fakeMailer) Send(_ context.Context, msg auth.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// last returns the most recent message to `to` whose subject contains subject.
func (m *fakeMailer) last(to, subject string) (auth.Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if strings.EqualFold(m.sent[i].To, to) && strings.Contains(m.sent[i].Subject, subject) {
			return m.sent[i], true
		}
	}
	return auth.Message{}, false
}

// tokenIn extracts the token from a link of the form ...#token=XXXX.
func tokenIn(t *testing.T, msg auth.Message) string {
	t.Helper()
	i := strings.Index(msg.Body, "#token=")
	if i < 0 {
		t.Fatalf("no token link in email:\n%s", msg.Body)
	}
	rest := msg.Body[i+len("#token="):]
	if j := strings.IndexAny(rest, " \r\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// ---- fake limiter and tracker

type fakeLimiter struct {
	mu     sync.Mutex
	counts map[string]int64
	err    error
}

func newFakeLimiter() *fakeLimiter { return &fakeLimiter{counts: map[string]int64{}} }

func (l *fakeLimiter) Hit(_ context.Context, key string, _ time.Duration) (int64, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return 0, 0, l.err
	}
	l.counts[key]++
	return l.counts[key], 42 * time.Second, nil
}

type fakeTracker struct {
	mu        sync.Mutex
	fails     map[string]int
	err       error
	failed    int
	succeeded int
}

func newFakeTracker() *fakeTracker { return &fakeTracker{fails: map[string]int{}} }

func trackKey(email, ip string) string { return strings.ToLower(email) + "|" + ip }

func (f *fakeTracker) Locked(_ context.Context, email, ip string) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.fails[trackKey(email, ip)] >= 5 {
		return 30 * time.Second, nil
	}
	return 0, nil
}

func (f *fakeTracker) Failed(_ context.Context, email, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed++
	f.fails[trackKey(email, ip)]++
	return nil
}

func (f *fakeTracker) Succeeded(_ context.Context, email, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeeded++
	delete(f.fails, trackKey(email, ip))
	return nil
}

// ---- in-memory Store with the same semantics as PGStore

type memUser struct {
	u    auth.User
	hash string
	p    auth.Profile
}

type memRefresh struct {
	id, userID, family string
	expires, familyExp time.Time
	used, revoked      *time.Time
}

type memOTT struct {
	userID  string
	purpose auth.TokenPurpose
	expires time.Time
	used    bool
}

type memStore struct {
	mu      sync.Mutex
	clock   *testClock
	byEmail map[string]*memUser
	byID    map[string]*memUser
	refresh map[string]*memRefresh
	otts    map[string]*memOTT
	seq     int
}

func newMemStore(c *testClock) *memStore {
	return &memStore{clock: c, byEmail: map[string]*memUser{}, byID: map[string]*memUser{},
		refresh: map[string]*memRefresh{}, otts: map[string]*memOTT{}}
}

func (s *memStore) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

func (s *memStore) CreateUser(_ context.Context, email, hash, name string) (auth.Me, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(email)
	if _, ok := s.byEmail[k]; ok {
		return auth.Me{}, auth.ErrEmailTaken
	}
	u := &memUser{
		u:    auth.User{ID: s.nextID("user"), Email: email, Role: auth.RoleViewer, Status: auth.StatusActive, CreatedAt: s.clock.Now()},
		hash: hash,
		p:    auth.Profile{DisplayName: name, PreferredLanguage: "en"},
	}
	s.byEmail[k], s.byID[u.u.ID] = u, u
	return auth.Me{User: u.u, Profile: u.p}, nil
}

func (s *memStore) UserByEmail(_ context.Context, email string) (auth.User, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byEmail[strings.ToLower(email)]
	if !ok {
		return auth.User{}, "", auth.ErrNotFound
	}
	return u.u, u.hash, nil
}

func (s *memStore) UserByID(_ context.Context, id string) (auth.Me, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return auth.Me{}, auth.ErrNotFound
	}
	return auth.Me{User: u.u, Profile: u.p}, nil
}

func (s *memStore) UpdatePasswordHash(_ context.Context, id, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[id].hash = hash
	return nil
}

func (s *memStore) UpdateProfile(_ context.Context, id string, p auth.ProfilePatch) (auth.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return auth.Profile{}, auth.ErrNotFound
	}
	if p.DisplayName != nil {
		u.p.DisplayName = *p.DisplayName
	}
	if p.AvatarURLSet {
		u.p.AvatarURL = p.AvatarURL
	}
	if p.CountrySet {
		u.p.Country = p.Country
	}
	if p.PreferredLanguage != nil {
		u.p.PreferredLanguage = *p.PreferredLanguage
	}
	return u.p, nil
}

func (s *memStore) InsertRefreshToken(_ context.Context, userID string, hash []byte, exp, famExp time.Time, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh[string(hash)] = &memRefresh{id: s.nextID("rt"), userID: userID, family: s.nextID("fam"), expires: exp, familyExp: famExp}
	return nil
}

func (s *memStore) RotateRefreshToken(_ context.Context, oldHash, newHash []byte, now time.Time, ttl time.Duration, _, _ string) (auth.RotateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.refresh[string(oldHash)]
	if !ok {
		return auth.RotateResult{}, auth.ErrNotFound
	}
	res := auth.RotateResult{UserID: old.userID, FamilyID: old.family}
	if old.revoked != nil {
		return res, auth.ErrRefreshInvalid
	}
	if old.used != nil {
		for _, r := range s.refresh {
			if r.family == old.family && r.revoked == nil {
				t := now
				r.revoked = &t
			}
		}
		return res, auth.ErrRefreshReused
	}
	if !old.expires.After(now) {
		return res, auth.ErrRefreshInvalid
	}
	exp := now.Add(ttl)
	if exp.After(old.familyExp) {
		exp = old.familyExp
	}
	if !exp.After(now) {
		return res, auth.ErrRefreshInvalid
	}
	t := now
	old.used = &t
	s.refresh[string(newHash)] = &memRefresh{id: s.nextID("rt"), userID: old.userID, family: old.family, expires: exp, familyExp: old.familyExp}
	res.ExpiresAt = exp
	return res, nil
}

func (s *memStore) RevokeFamilyOf(_ context.Context, hash []byte, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.refresh[string(hash)]
	if !ok {
		return nil
	}
	for _, r := range s.refresh {
		if r.family == old.family && r.revoked == nil {
			t := now
			r.revoked = &t
		}
	}
	return nil
}

func (s *memStore) RevokeAllRefreshTokens(_ context.Context, userID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.refresh {
		if r.userID == userID && r.revoked == nil {
			t := now
			r.revoked = &t
		}
	}
	return nil
}

func (s *memStore) IssueOneTimeToken(_ context.Context, userID string, purpose auth.TokenPurpose, hash []byte, exp time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.otts {
		if o.userID == userID && o.purpose == purpose {
			o.used = true
		}
	}
	s.otts[string(hash)] = &memOTT{userID: userID, purpose: purpose, expires: exp}
	return nil
}

func (s *memStore) consume(purpose auth.TokenPurpose, hash []byte, now time.Time) (string, error) {
	o, ok := s.otts[string(hash)]
	if !ok || o.purpose != purpose || o.used || !o.expires.After(now) {
		return "", auth.ErrInvalidToken
	}
	o.used = true
	return o.userID, nil
}

func (s *memStore) VerifyEmailWithToken(_ context.Context, hash []byte, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.consume(auth.PurposeEmailVerification, hash, now)
	if err != nil {
		return "", err
	}
	if u := s.byID[id]; u.u.EmailVerifiedAt == nil {
		t := now
		u.u.EmailVerifiedAt = &t
	}
	return id, nil
}

func (s *memStore) ResetPasswordWithToken(_ context.Context, hash []byte, now time.Time, newHash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.consume(auth.PurposePasswordReset, hash, now)
	if err != nil {
		return "", err
	}
	s.byID[id].hash = newHash
	for _, r := range s.refresh {
		if r.userID == id && r.revoked == nil {
			t := now
			r.revoked = &t
		}
	}
	return id, nil
}

// test helpers
func (s *memStore) setStatus(email string, st auth.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byEmail[strings.ToLower(email)].u.Status = st
}

func (s *memStore) setRole(email string, r auth.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byEmail[strings.ToLower(email)].u.Role = r
}

func (s *memStore) storedHash(email string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byEmail[strings.ToLower(email)].hash
}

// ---- harness

type harness struct {
	t       *testing.T
	h       http.Handler
	svc     *auth.Service
	tokens  *auth.TokenIssuer
	mail    *fakeMailer
	clock   *testClock
	ip      string
	store   auth.Store
	mem     *memStore    // nil when a real store is used
	tracker *fakeTracker // nil when a real tracker is used
	limiter *fakeLimiter // nil when a real limiter is used
}

type options struct {
	store     auth.Store
	tracker   auth.FailureTracker
	limiter   ratelimit.Store
	origins   []string
	secure    bool
	cookiePth string
	owners    map[string]string // title id -> creator id, for the ownership test route
}

func randomIP() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("10.%d.%d.%d", b[0], b[1], b[2])
}

func newHarness(t *testing.T, o options) *harness {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	clock := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	hasher, err := auth.NewHasher(auth.Params{MemoryKiB: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenIssuer(testSecret, 15*time.Minute, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	hn := &harness{t: t, mail: &fakeMailer{}, clock: clock, tokens: tokens, ip: randomIP()}

	if o.store == nil {
		hn.mem = newMemStore(clock)
		o.store = hn.mem
	}
	if o.tracker == nil {
		hn.tracker = newFakeTracker()
		o.tracker = hn.tracker
	}
	if o.limiter == nil {
		hn.limiter = newFakeLimiter()
		o.limiter = hn.limiter
	}
	if o.origins == nil {
		o.origins = []string{"http://localhost:3000"}
	}
	hn.store = o.store

	svc, err := auth.NewService(auth.Deps{
		Store: o.store, Hasher: hasher, Tokens: tokens, Tracker: o.tracker, Mailer: hn.mail,
		Logger: logger, Now: clock.Now,
		Config: auth.ServiceConfig{
			RefreshTTL: 30 * 24 * time.Hour, RefreshAbsoluteTTL: 90 * 24 * time.Hour,
			WebBaseURL: "http://localhost:8088", SyncEmail: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	hn.svc = svc
	handler := auth.NewHandler(svc, tokens, o.limiter, logger, auth.HandlerConfig{
		CookieSecure: o.secure, CookiePath: o.cookiePth, AllowedOrigins: o.origins,
	})

	lookup := func(_ context.Context, id string) (string, error) {
		if owner, ok := o.owners[id]; ok {
			return owner, nil
		}
		return "", auth.ErrNotFound
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	authn := handler.Authenticate()
	hn.h = server.NewHandler(server.Deps{Logger: logger, Mount: []func(httpx.Mux){
		handler.Mount,
		func(mux httpx.Mux) {
			mux.Handle("/test/admin", authn(auth.RequireRole(auth.RoleAdmin)(ok)))
			mux.Handle("/test/creator", authn(auth.RequireRole(auth.RoleCreator, auth.RoleAdmin)(ok)))
			mux.Handle("/test/title/{id}", authn(auth.RequireRole(auth.RoleCreator, auth.RoleAdmin)(
				auth.RequireTitleOwner(lookup, func(r *http.Request) string { return r.PathValue("id") })(ok))))
		},
	}})
	return hn
}

type reqOpt func(*http.Request)

func withBearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}
func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func withIP(ip string) reqOpt       { return func(r *http.Request) { r.RemoteAddr = ip + ":4000" } }
func withCookie(name, val string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: val}) }
}

func (h *harness) do(method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				h.t.Fatal(err)
			}
			rd = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = h.ip + ":4000"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

type authBody struct {
	AccessToken      string       `json:"access_token"`
	TokenType        string       `json:"token_type"`
	ExpiresIn        int          `json:"expires_in"`
	RefreshToken     string       `json:"refresh_token"`
	RefreshExpiresIn int          `json:"refresh_expires_in"`
	User             auth.User    `json:"user"`
	Profile          auth.Profile `json:"profile"`
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return v
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct {
		Error struct {
			Code   string            `json:"code"`
			Fields map[string]string `json:"fields"`
		} `json:"error"`
	}](t, rec).Error.Code
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func uniqueEmail(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b) + "@example.com"
}

// session is a signed-in test user.
type session struct {
	email, access, refresh string
	id                     string
}

// register creates and signs in a user (web transport: refresh token arrives as a cookie).
func (h *harness) register(email string) session {
	h.t.Helper()
	rec := h.do(http.MethodPost, "/v1/auth/register", map[string]string{"email": email, "password": goodPassword, "display_name": "Test User"})
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	b := decode[authBody](h.t, rec)
	c := cookieNamed(rec, "sa_refresh")
	if c == nil {
		h.t.Fatal("no refresh cookie after register")
	}
	return session{email: email, access: b.AccessToken, refresh: c.Value, id: b.User.ID}
}

func (h *harness) login(email, password string, opts ...reqOpt) *httptest.ResponseRecorder {
	return h.do(http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": password}, opts...)
}

func (h *harness) refreshWithCookie(token string, opts ...reqOpt) *httptest.ResponseRecorder {
	return h.do(http.MethodPost, "/v1/auth/refresh", nil, append([]reqOpt{withCookie("sa_refresh", token)}, opts...)...)
}
