package contract

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/api/internal/app"
	"streamafrica/api/internal/testdb"
)

type stack struct {
	t        *testing.T
	app      *app.App
	router   routers.Router
	pool     *pgxpool.Pool
	mail     *fakeMailer
	covered  map[string]bool
	statuses map[string]bool
}

type call struct {
	method, path, body string
	bearer             string
	cookie             string
	headers            map[string]string
	badRequest         bool // the request deliberately violates the spec, so only the response is validated
	wantStatus         int
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomIP() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("10.%d.%d.%d:5000", b[0], b[1], b[2])
}

// do sends the request through the real handler and checks both the request and the response
// against docs/openapi.yaml. It returns the recorder for further assertions.
func (s *stack) do(c call) *httptest.ResponseRecorder {
	s.t.Helper()
	var rd *bytes.Reader
	if c.body != "" {
		rd = bytes.NewReader([]byte(c.body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(c.method, "http://localhost:8080"+c.path, rd)
	req.RemoteAddr = randomIP() // every call gets its own IP so IP rate limits do not interfere
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sa_refresh", Value: c.cookie})
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	route, params, err := s.router.FindRoute(req)
	if err != nil {
		s.t.Fatalf("%s %s is not in the spec: %v", c.method, c.path, err)
	}
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if !c.badRequest {
		if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
			s.t.Fatalf("%s %s: the test request does not match the spec: %v", c.method, c.path, err)
		}
	}

	rec := httptest.NewRecorder()
	s.app.Handler.ServeHTTP(rec, req)
	if c.wantStatus != 0 && rec.Code != c.wantStatus {
		s.t.Fatalf("%s %s: status %d, want %d: %s", c.method, c.path, rec.Code, c.wantStatus, rec.Body.String())
	}

	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: rec.Code, Header: rec.Header(),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	out.SetBodyBytes(rec.Body.Bytes())
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		s.t.Fatalf("%s %s -> %d does not match the spec: %v\nbody: %s", c.method, c.path, rec.Code, err, rec.Body.String())
	}
	s.covered[route.Method+" "+route.Path] = true
	s.statuses[fmt.Sprintf("%s %s %d", route.Method, route.Path, rec.Code)] = true
	return rec
}

func jsonOf[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	return v
}

type authResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID string `json:"id"`
	} `json:"user"`
}

func cookieOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "sa_refresh" && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func tokenIn(t *testing.T, m *fakeMailer, to, subject string) string {
	t.Helper()
	msg, ok := m.lastTo(to, subject)
	if !ok {
		t.Fatalf("no %q email to %s", subject, to)
	}
	i := strings.Index(msg.Body, "#token=")
	rest := msg.Body[i+len("#token="):]
	if j := strings.IndexAny(rest, " \r\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func (s *stack) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("%v\n%s", err, sql)
	}
}

func (s *stack) scan(sql string, args ...any) string {
	s.t.Helper()
	var v string
	if err := s.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		s.t.Fatalf("%v\n%s", err, sql)
	}
	return v
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := testdb.Pool(t)
	rdb := testdb.Redis(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0") // stands in for object storage so /readyz can be ok
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	mail := &fakeMailer{}
	application, err := app.New(app.Options{
		Config: testConfig(ln.Addr().String()), Pool: pool, Redis: rdb, Logger: quiet(),
		Mailer: mail, SyncEmail: true, CacheNamespace: "contract-" + randHex(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, router := loadSpec(t)
	return &stack{t: t, app: application, router: router, pool: pool, mail: mail,
		covered: map[string]bool{}, statuses: map[string]bool{}}
}

const pw = "correct horse battery"

func reg(email string) string {
	return `{"email":"` + email + `","password":"` + pw + `","display_name":"Contract Tester"}`
}

// TestResponsesMatchSpec drives every documented operation (success and error cases) through the
// production wiring against real PostgreSQL and Redis. Every request and every response is
// validated against docs/openapi.yaml, so a handler that returns a field the spec does not
// describe (or omits one it promises) fails here.
func TestResponsesMatchSpec(t *testing.T) {
	s := newStack(t)
	doc, _ := loadSpec(t)

	// ---- health
	s.do(call{method: "GET", path: "/healthz", wantStatus: 200})
	s.do(call{method: "GET", path: "/readyz", wantStatus: 200})

	// ---- auth: register, verify, login, refresh, logout
	email := "ada-" + randHex(4) + "@example.com"
	rec := s.do(call{method: "POST", path: "/v1/auth/register", body: reg(email), wantStatus: 201})
	ada := jsonOf[authResp](t, rec)
	adaRefresh := cookieOf(rec)
	if ada.AccessToken == "" || adaRefresh == "" || ada.RefreshToken != "" {
		t.Fatalf("register: %+v cookie=%q", ada, adaRefresh)
	}
	s.do(call{method: "POST", path: "/v1/auth/register", body: reg(email), wantStatus: 409})
	s.do(call{method: "POST", path: "/v1/auth/register", body: `{"email":"nope","password":"short","display_name":""}`, badRequest: true, wantStatus: 422})
	s.do(call{method: "POST", path: "/v1/auth/register", body: `{"email":"x@y.co","password":"` + pw + `","display_name":"X","role":"admin"}`, badRequest: true, wantStatus: 400})
	s.do(call{method: "POST", path: "/v1/auth/register", body: reg("z@example.com"), headers: map[string]string{"Content-Type": "text/plain"}, badRequest: true, wantStatus: 415})

	verifyTok := tokenIn(t, s.mail, email, "Confirm")
	s.do(call{method: "POST", path: "/v1/auth/verify-email", body: `{"token":"` + verifyTok + `"}`, wantStatus: 204})
	s.do(call{method: "POST", path: "/v1/auth/verify-email", body: `{"token":"` + verifyTok + `"}`, wantStatus: 400})
	s.do(call{method: "POST", path: "/v1/auth/resend-verification", body: `{"email":"` + email + `"}`, wantStatus: 202})
	s.do(call{method: "POST", path: "/v1/auth/resend-verification", body: `{"email":""}`, wantStatus: 422})

	s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + email + `","password":"wrong password!"}`, wantStatus: 401})
	s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"","password":""}`, wantStatus: 422})
	rec = s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + email + `","password":"` + pw + `"}`, wantStatus: 200})
	adaRefresh = cookieOf(rec)
	ada = jsonOf[authResp](t, rec)

	// refresh: cookie (web), reuse, bad origin
	good := map[string]string{"Origin": "http://localhost:3000"}
	rec = s.do(call{method: "POST", path: "/v1/auth/refresh", cookie: adaRefresh, headers: good, wantStatus: 200})
	next := cookieOf(rec)
	if jsonOf[authResp](t, rec).RefreshToken != "" {
		t.Fatal("a cookie client must not receive the refresh token in the body")
	}
	s.do(call{method: "POST", path: "/v1/auth/refresh", cookie: adaRefresh, headers: good, wantStatus: 401}) // reuse
	s.do(call{method: "POST", path: "/v1/auth/refresh", cookie: next, headers: good, wantStatus: 401})       // family is dead
	s.do(call{method: "POST", path: "/v1/auth/refresh", wantStatus: 401})

	// body transport (mobile)
	rec = s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + email + `","password":"` + pw + `"}`,
		headers: map[string]string{"X-Auth-Transport": "body"}, wantStatus: 200})
	mobile := jsonOf[authResp](t, rec)
	if mobile.RefreshToken == "" || cookieOf(rec) != "" {
		t.Fatalf("body transport: %+v", mobile)
	}
	rec = s.do(call{method: "POST", path: "/v1/auth/refresh", body: `{"refresh_token":"` + mobile.RefreshToken + `"}`, wantStatus: 200})
	mobile = jsonOf[authResp](t, rec)
	if mobile.RefreshToken == "" {
		t.Fatal("body refresh must return the new refresh token")
	}

	// fresh web session for the rest of the run
	rec = s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + email + `","password":"` + pw + `"}`, wantStatus: 200})
	adaRefresh = cookieOf(rec)
	ada = jsonOf[authResp](t, rec)
	s.do(call{method: "POST", path: "/v1/auth/logout", cookie: adaRefresh, headers: map[string]string{"Origin": "https://evil.example"}, wantStatus: 403})
	s.do(call{method: "POST", path: "/v1/auth/logout", cookie: adaRefresh, headers: good, wantStatus: 204})
	s.do(call{method: "POST", path: "/v1/auth/logout-all", wantStatus: 401})
	rec = s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + email + `","password":"` + pw + `"}`, wantStatus: 200})
	ada = jsonOf[authResp](t, rec)
	s.do(call{method: "POST", path: "/v1/auth/logout-all", bearer: ada.AccessToken, wantStatus: 204})
	rec = s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + email + `","password":"` + pw + `"}`, wantStatus: 200})
	ada = jsonOf[authResp](t, rec)

	// password reset
	s.do(call{method: "POST", path: "/v1/auth/forgot-password", body: `{"email":"` + email + `"}`, wantStatus: 202})
	s.do(call{method: "POST", path: "/v1/auth/forgot-password", body: `{"email":"ghost-` + randHex(3) + `@example.com"}`, wantStatus: 202})
	resetTok := tokenIn(t, s.mail, email, "Reset")
	s.do(call{method: "POST", path: "/v1/auth/reset-password", body: `{"token":"` + resetTok + `","new_password":"password123"}`, badRequest: true, wantStatus: 422})
	s.do(call{method: "POST", path: "/v1/auth/reset-password", body: `{"token":"` + resetTok + `","new_password":"a brand new passphrase"}`, wantStatus: 204})
	s.do(call{method: "POST", path: "/v1/auth/reset-password", body: `{"token":"` + resetTok + `","new_password":"another good passphrase"}`, wantStatus: 400})
	rec = s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + email + `","password":"a brand new passphrase"}`, wantStatus: 200})
	ada = jsonOf[authResp](t, rec)

	// ---- /v1/me
	s.do(call{method: "GET", path: "/v1/me", wantStatus: 401})
	s.do(call{method: "GET", path: "/v1/me", bearer: ada.AccessToken, wantStatus: 200})
	s.do(call{method: "PATCH", path: "/v1/me", body: `{"display_name":"Ada L.","country":"NG","preferred_language":"yo","avatar_url":"https://cdn.example.com/a.png"}`, bearer: ada.AccessToken, wantStatus: 200})
	s.do(call{method: "PATCH", path: "/v1/me", body: `{"avatar_url":null,"country":null}`, bearer: ada.AccessToken, wantStatus: 200})
	s.do(call{method: "PATCH", path: "/v1/me", body: `{"country":"nigeria"}`, bearer: ada.AccessToken, badRequest: true, wantStatus: 422})
	s.do(call{method: "PATCH", path: "/v1/me", body: `{"role":"admin"}`, bearer: ada.AccessToken, badRequest: true, wantStatus: 400})

	// ---- catalog fixtures (SQL): a creator, published/draft titles, a series with episodes
	creatorEmail := "creator-" + randHex(4) + "@example.com"
	rec = s.do(call{method: "POST", path: "/v1/auth/register", body: reg(creatorEmail), wantStatus: 201})
	creatorID := jsonOf[authResp](t, rec).User.ID
	s.exec(`UPDATE users SET role = 'creator' WHERE id = $1::uuid`, creatorID)
	rec = s.do(call{method: "POST", path: "/v1/auth/login", body: `{"email":"` + creatorEmail + `","password":"` + pw + `"}`, wantStatus: 200})
	creator := jsonOf[authResp](t, rec)

	mk := func(slug, typ, lang, status string, days int, genres ...string) string {
		id := s.scan(`INSERT INTO titles (type, slug, name, synopsis, original_language, status, creator_id, published_at, release_year, maturity_rating, runtime_minutes, poster_url)
		  VALUES ($1::title_type, $2, $3, 'A story about Lagos and family.', $4, $5::title_status, $6::uuid,
		          CASE WHEN $5 = 'published' THEN now() - make_interval(days => $7) END, 2024, '12', CASE WHEN $1 = 'movie' THEN 100 END, 'https://cdn.example.com/p.jpg')
		  RETURNING id::text`, typ, slug, "Title "+slug, lang, status, creatorID, days)
		for _, g := range genres {
			s.exec(`INSERT INTO title_genres (title_id, genre_id) SELECT $1::uuid, id FROM genres WHERE slug = $2`, id, g)
		}
		return id
	}
	movie := mk("lagos-movie", "movie", "en", "published", 1, "drama", "thriller")
	mk("yoruba-movie", "movie", "yo", "published", 2, "epic-historical")
	series := mk("hustle-series", "series", "pcm", "published", 3, "comedy")
	draft := mk("secret-draft", "movie", "en", "draft", 0, "drama")
	// credits, media and subtitles on the movie; seasons and episodes on the series
	person := s.scan(`INSERT INTO people (name, photo_url) VALUES ('Dara Director', 'https://cdn.example.com/d.jpg') RETURNING id::text`)
	s.exec(`INSERT INTO title_people (title_id, person_id, role, billing_order) VALUES ($1::uuid, $2::uuid, 'director', 0)`, movie, person)
	s.exec(`INSERT INTO title_people (title_id, person_id, role, character_name, billing_order) VALUES ($1::uuid, $2::uuid, 'cast', 'Femi', 0)`, movie, person)
	s.exec(`INSERT INTO media_assets (title_id, status, hls_manifest_key, duration_seconds) VALUES ($1::uuid, 'ready', 'k', 6000)`, movie)
	s.exec(`INSERT INTO subtitles (asset_id, language, label, vtt_url) SELECT id, 'en', 'English', 'https://cdn.example.com/en.vtt' FROM media_assets WHERE title_id = $1::uuid`, movie)
	season := s.scan(`INSERT INTO seasons (title_id, number, name) VALUES ($1::uuid, 1, 'Season 1') RETURNING id::text`, series)
	episode := s.scan(`INSERT INTO episodes (season_id, title_id, number, name, runtime_minutes, air_date) VALUES ($1::uuid, $2::uuid, 1, 'Pilot', 30, '2024-05-01') RETURNING id::text`, season, series)
	s.exec(`INSERT INTO seasons (title_id, number) VALUES ($1::uuid, 2)`, series) // an empty season

	// ---- catalog: list
	rec = s.do(call{method: "GET", path: "/v1/titles", wantStatus: 200})
	if strings.Contains(rec.Body.String(), "secret-draft") {
		t.Fatal("a draft leaked into the public list")
	}
	etag := rec.Header().Get("ETag")
	s.do(call{method: "GET", path: "/v1/titles", headers: map[string]string{"If-None-Match": etag}, wantStatus: 304})
	page := jsonOf[struct {
		Items      []struct{ Slug string } `json:"items"`
		NextCursor *string                 `json:"next_cursor"`
	}](t, s.do(call{method: "GET", path: "/v1/titles?limit=2&sort=newest", wantStatus: 200}))
	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("page 1: %+v", page)
	}
	s.do(call{method: "GET", path: "/v1/titles?limit=2&sort=newest&cursor=" + *page.NextCursor, wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles?type=series&genre=comedy&language=pcm", wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles?q=lagos&sort=relevance", wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles?sort=name", wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles", bearer: creator.AccessToken, wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles?limit=0", badRequest: true, wantStatus: 422})
	s.do(call{method: "GET", path: "/v1/titles?cursor=garbage", wantStatus: 400})
	s.do(call{method: "GET", path: "/v1/titles", bearer: "not.a.jwt", wantStatus: 401})

	// ---- catalog: detail
	rec = s.do(call{method: "GET", path: "/v1/titles/lagos-movie", wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles/lagos-movie", headers: map[string]string{"If-None-Match": rec.Header().Get("ETag")}, wantStatus: 304})
	s.do(call{method: "GET", path: "/v1/titles/lagos-movie", bearer: ada.AccessToken, wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles/hustle-series", wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles/secret-draft", wantStatus: 404})
	s.do(call{method: "GET", path: "/v1/titles/secret-draft", bearer: ada.AccessToken, wantStatus: 404})
	s.do(call{method: "GET", path: "/v1/titles/secret-draft", bearer: creator.AccessToken, wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/titles/Not_A_Slug", badRequest: true, wantStatus: 404})
	s.do(call{method: "GET", path: "/v1/titles/lagos-movie", bearer: "x.y.z", wantStatus: 401})

	// ---- catalog: genres, home
	rec = s.do(call{method: "GET", path: "/v1/genres", wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/genres", headers: map[string]string{"If-None-Match": rec.Header().Get("ETag")}, wantStatus: 304})
	s.do(call{method: "GET", path: "/v1/home", wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/home", bearer: ada.AccessToken, wantStatus: 200})
	s.do(call{method: "GET", path: "/v1/home", bearer: "x.y.z", wantStatus: 401})

	// ---- library: watchlist
	s.do(call{method: "GET", path: "/v1/me/watchlist", wantStatus: 401})
	wl := `{"title_id":"` + movie + `"}`
	s.do(call{method: "POST", path: "/v1/me/watchlist", body: wl, bearer: ada.AccessToken, wantStatus: 201})
	s.do(call{method: "POST", path: "/v1/me/watchlist", body: wl, bearer: ada.AccessToken, wantStatus: 200})
	s.do(call{method: "POST", path: "/v1/me/watchlist", body: `{"title_id":"` + draft + `"}`, bearer: ada.AccessToken, wantStatus: 404})
	s.do(call{method: "POST", path: "/v1/me/watchlist", body: `{"title_id":"nope"}`, bearer: ada.AccessToken, badRequest: true, wantStatus: 422})
	s.do(call{method: "POST", path: "/v1/me/watchlist", body: `{"title_id":"` + series + `"}`, bearer: ada.AccessToken, wantStatus: 201})
	rec = s.do(call{method: "GET", path: "/v1/me/watchlist?limit=1", bearer: ada.AccessToken, wantStatus: 200})
	if cur := jsonOf[struct {
		NextCursor *string `json:"next_cursor"`
	}](t, rec).NextCursor; cur != nil {
		s.do(call{method: "GET", path: "/v1/me/watchlist?limit=1&cursor=" + *cur, bearer: ada.AccessToken, wantStatus: 200})
	} else {
		t.Fatal("expected a second watchlist page")
	}
	s.do(call{method: "GET", path: "/v1/me/watchlist?cursor=junk", bearer: ada.AccessToken, wantStatus: 400})
	s.do(call{method: "GET", path: "/v1/me/watchlist?limit=99", bearer: ada.AccessToken, badRequest: true, wantStatus: 422})
	s.do(call{method: "DELETE", path: "/v1/me/watchlist/" + series, bearer: ada.AccessToken, wantStatus: 204})
	s.do(call{method: "DELETE", path: "/v1/me/watchlist/" + series, bearer: ada.AccessToken, wantStatus: 204})
	s.do(call{method: "DELETE", path: "/v1/me/watchlist/not-a-uuid", bearer: ada.AccessToken, badRequest: true, wantStatus: 422})
	s.do(call{method: "DELETE", path: "/v1/me/watchlist/" + series, wantStatus: 401})
	// a full watchlist
	fullEmail := "full-" + randHex(4) + "@example.com"
	full := jsonOf[authResp](t, s.do(call{method: "POST", path: "/v1/auth/register", body: reg(fullEmail), wantStatus: 201}))
	s.exec(`INSERT INTO titles (type, slug, name, status, creator_id, published_at)
	        SELECT 'movie', 'bulk-' || n, 'bulk ' || n, 'published', $1::uuid, now() FROM generate_series(1, 500) n`, creatorID)
	s.exec(`INSERT INTO watchlist (user_id, title_id) SELECT $1::uuid, id FROM titles WHERE slug LIKE 'bulk-%'`, full.User.ID)
	s.do(call{method: "POST", path: "/v1/me/watchlist", body: wl, bearer: full.AccessToken, wantStatus: 409})

	// ---- library: progress and continue watching
	s.do(call{method: "PUT", path: "/v1/me/progress", badRequest: true, wantStatus: 401}) // no body: only the 401 shape is checked
	s.do(call{method: "PUT", path: "/v1/me/progress", body: `{"title_id":"` + movie + `","position_seconds":1234.5,"duration_seconds":6000}`, bearer: ada.AccessToken, wantStatus: 200})
	rec = s.do(call{method: "PUT", path: "/v1/me/progress", body: `{"title_id":"` + movie + `","position_seconds":5900,"duration_seconds":6000}`, bearer: ada.AccessToken, wantStatus: 200})
	if !strings.Contains(rec.Body.String(), `"completed":true`) {
		t.Fatalf("95%% must complete: %s", rec.Body.String())
	}
	s.do(call{method: "PUT", path: "/v1/me/progress", body: `{"title_id":"` + series + `","episode_id":"` + episode + `","position_seconds":600,"duration_seconds":1800}`, bearer: ada.AccessToken, wantStatus: 200})
	s.do(call{method: "PUT", path: "/v1/me/progress", body: `{"title_id":"` + series + `","position_seconds":1,"duration_seconds":10}`, bearer: ada.AccessToken, wantStatus: 422})
	s.do(call{method: "PUT", path: "/v1/me/progress", body: `{"title_id":"` + draft + `","position_seconds":1,"duration_seconds":10}`, bearer: ada.AccessToken, wantStatus: 404})
	s.do(call{method: "PUT", path: "/v1/me/progress", body: `{"title_id":"` + movie + `","position_seconds":-1,"duration_seconds":0}`, bearer: ada.AccessToken, badRequest: true, wantStatus: 422})
	s.do(call{method: "PUT", path: "/v1/me/progress", body: `{"title_id":"` + movie + `","position_seconds":1,"duration_seconds":10,"completed":true}`, bearer: ada.AccessToken, badRequest: true, wantStatus: 400})
	rec = s.do(call{method: "GET", path: "/v1/me/continue-watching", bearer: ada.AccessToken, wantStatus: 200})
	if !strings.Contains(rec.Body.String(), "hustle-series") || strings.Contains(rec.Body.String(), "lagos-movie") {
		t.Fatalf("continue watching must hold the unfinished series only: %s", rec.Body.String())
	}
	s.do(call{method: "GET", path: "/v1/me/continue-watching?limit=500", bearer: ada.AccessToken, badRequest: true, wantStatus: 422})
	s.do(call{method: "GET", path: "/v1/me/continue-watching", wantStatus: 401})
	rec = s.do(call{method: "GET", path: "/v1/home", bearer: ada.AccessToken, wantStatus: 200})
	if !strings.Contains(rec.Body.String(), `"key":"continue-watching"`) {
		t.Fatalf("home for a signed-in viewer must start with continue watching: %s", rec.Body.String())
	}
	s.do(call{method: "GET", path: "/v1/titles/hustle-series", bearer: ada.AccessToken, wantStatus: 200}) // viewer state with episode progress

	// throttle: 30 per minute per user, then 429 with Retry-After
	body := `{"title_id":"` + movie + `","position_seconds":10,"duration_seconds":6000}`
	throttled := 0
	for i := 0; i < 32; i++ {
		rec := s.do(call{method: "PUT", path: "/v1/me/progress", body: body, bearer: full.AccessToken})
		if rec.Code == 429 {
			throttled++
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		}
	}
	if throttled == 0 {
		t.Fatal("the progress endpoint was never throttled")
	}

	// ---- every operation in the spec must have been exercised at least once
	for op := range specOperations(doc) {
		if !s.covered[op] {
			t.Errorf("the contract test never exercised %s: add a call for it", op)
		}
	}
	t.Logf("exercised %d operations and %d distinct (operation, status) pairs", len(s.covered), len(s.statuses))
}
