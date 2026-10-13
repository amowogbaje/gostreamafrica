package catalog

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
	"sync/atomic"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/server"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef-test")

// fakeStore records what it was asked and returns canned data.
type fakeStore struct {
	mu          sync.Mutex
	lastViewer  Viewer
	lastList    ListParams
	rows        []listRow
	detailErr   error
	detail      TitleDetail
	titleType   string
	titleErr    error
	episodeOK   bool
	watchAdded  bool
	watchErr    error
	progressIn  ProgressInput
	genresCalls atomic.Int32
	homeCalls   atomic.Int32
	homeDelay   time.Duration
}

func (f *fakeStore) ListTitles(_ context.Context, v Viewer, p ListParams) ([]listRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastViewer, f.lastList = v, p
	return f.rows, nil
}
func (f *fakeStore) TitleBySlug(_ context.Context, v Viewer, _ string) (TitleDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastViewer = v
	return f.detail, f.detailErr
}
func (f *fakeStore) TitleType(context.Context, Viewer, string) (string, error) {
	return f.titleType, f.titleErr
}
func (f *fakeStore) EpisodeBelongsTo(context.Context, string, string) (bool, error) {
	return f.episodeOK, nil
}
func (f *fakeStore) Genres(context.Context) ([]GenreCount, error) {
	f.genresCalls.Add(1)
	return []GenreCount{{Slug: "drama", Name: "Drama", TitleCount: 3}}, nil
}
func (f *fakeStore) HomePublic(context.Context, time.Time, int, int) (homePublic, error) {
	f.homeCalls.Add(1)
	time.Sleep(f.homeDelay)
	return homePublic{New: []TitleSummary{summary("a")}}, nil
}
func (f *fakeStore) Watchlist(context.Context, Viewer, *cursor, int) ([]watchRow, error) {
	return nil, nil
}
func (f *fakeStore) WatchlistAdd(context.Context, string, string) (time.Time, bool, error) {
	return time.Unix(1700000000, 0).UTC(), f.watchAdded, f.watchErr
}
func (f *fakeStore) WatchlistRemove(context.Context, string, string) error { return nil }
func (f *fakeStore) UpsertProgress(_ context.Context, in ProgressInput) (Progress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.progressIn = in
	return Progress{TitleID: in.TitleID, EpisodeID: in.EpisodeID, PositionSeconds: in.PositionSeconds,
		DurationSeconds: in.DurationSeconds, Completed: in.Completed, UpdatedAt: time.Unix(1700000000, 0).UTC()}, nil
}
func (f *fakeStore) ContinueWatching(context.Context, Viewer, int) ([]ContinueItem, error) {
	return []ContinueItem{}, nil
}

func summary(slug string) TitleSummary {
	return TitleSummary{TitleCore: TitleCore{ID: someID, Slug: slug, Type: "movie", Name: slug, Status: "published"}, Genres: []string{}}
}

type counterLimiter struct {
	mu     sync.Mutex
	counts map[string]int64
	err    error
}

func (l *counterLimiter) Hit(_ context.Context, key string, _ time.Duration) (int64, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return 0, 0, l.err
	}
	l.counts[key]++
	return l.counts[key], 10 * time.Second, nil
}

type rig struct {
	t       *testing.T
	h       http.Handler
	store   *fakeStore
	tokens  *auth.TokenIssuer
	limiter *counterLimiter
	svc     *Service
}

func newRig(t *testing.T) *rig {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	tokens, err := auth.NewTokenIssuer(testSecret, 15*time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{titleType: "movie", watchAdded: true, episodeOK: true}
	lim := &counterLimiter{counts: map[string]int64{}}
	svc := NewService(st, NoCache{}, logger, time.Now, Config{})
	h := NewHandler(svc, AuthMiddleware{Required: auth.Authenticate(tokens), Optional: auth.OptionalAuthenticate(tokens)}, lim, logger)
	return &rig{t: t, h: server.NewHandler(server.Deps{Logger: logger, Mount: []func(httpx.Mux){h.Mount}}),
		store: st, tokens: tokens, limiter: lim, svc: svc}
}

func (r *rig) token(userID string, role auth.Role) string {
	tok, _, err := r.tokens.Issue(userID, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return tok
}

func (r *rig) do(method, path, body, bearer string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

type errBody struct {
	Error struct {
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	} `json:"error"`
}

func errOf(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return e
}

func TestList_Validation(t *testing.T) {
	r := newRig(t)
	for path, field := range map[string]string{
		"/v1/titles?limit=0":                       "limit",
		"/v1/titles?limit=51":                      "limit",
		"/v1/titles?limit=abc":                     "limit",
		"/v1/titles?type=documentary":              "type",
		"/v1/titles?genre=Drama":                   "genre",
		"/v1/titles?language=English":              "language",
		"/v1/titles?sort=relevance":                "sort",
		"/v1/titles?sort=popular":                  "sort",
		"/v1/titles?q=" + strings.Repeat("a", 101): "q",
	} {
		rec := r.do(get, path, "", "")
		if rec.Code != http.StatusUnprocessableEntity || errOf(t, rec).Error.Code != "validation_failed" || errOf(t, rec).Error.Fields[field] == "" {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	bad := cursor{Kind: "titles", Sort: SortName, Name: "x", ID: someID}.encode()
	for _, path := range []string{"/v1/titles?cursor=garbage", "/v1/titles?cursor=" + bad} {
		if rec := r.do(get, path, "", ""); rec.Code != http.StatusBadRequest || errOf(t, rec).Error.Code != "invalid_cursor" {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

const (
	get  = http.MethodGet
	post = http.MethodPost
	put  = http.MethodPut
	del  = http.MethodDelete
)

func TestList_ShapeAndPagination(t *testing.T) {
	r := newRig(t)
	pub := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mk := func(slug string) listRow {
		s := summary(slug)
		s.ID = someID
		s.PublishedAt = &pub
		return listRow{TitleSummary: s}
	}
	// limit=2 and the store returns 3 rows: the page has 2 items and a cursor
	r.store.rows = []listRow{mk("a"), mk("b"), mk("c")}
	rec := r.do(get, "/v1/titles?limit=2", "", "")
	var page Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if len(page.Items) != 2 || page.NextCursor == nil || r.store.lastList.Limit != 2 {
		t.Fatalf("page=%+v limit passed=%d", page, r.store.lastList.Limit)
	}
	// the cursor is accepted for the next request and handed to the store
	rec = r.do(get, "/v1/titles?limit=2&cursor="+*page.NextCursor, "", "")
	if rec.Code != 200 || r.store.lastList.After == nil || r.store.lastList.After.ID != someID || r.store.lastList.After.TS == nil {
		t.Fatalf("cursor not forwarded: %d %+v", rec.Code, r.store.lastList.After)
	}
	// exactly `limit` rows: no cursor
	r.store.rows = []listRow{mk("a"), mk("b")}
	rec = r.do(get, "/v1/titles?limit=2", "", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Items) != 2 || page.NextCursor != nil {
		t.Fatalf("a full last page must not advertise another page: %+v", page)
	}
	// empty result renders [] and null, never null items
	r.store.rows = nil
	rec = r.do(get, "/v1/titles", "", "")
	if body := rec.Body.String(); !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"next_cursor":null`) {
		t.Fatalf("empty page: %s", body)
	}
}

func TestVisibilityIsPassedToTheStore(t *testing.T) {
	r := newRig(t)
	r.do(get, "/v1/titles", "", "")
	if r.store.lastViewer.Authenticated() {
		t.Fatalf("anonymous request reached the store as %+v", r.store.lastViewer)
	}
	r.do(get, "/v1/titles", "", r.token("u-1", auth.RoleCreator))
	if v := r.store.lastViewer; v.UserID != "u-1" || v.Role != auth.RoleCreator {
		t.Fatalf("viewer = %+v", v)
	}
	r.do(get, "/v1/titles", "", r.token("u-2", auth.RoleAdmin))
	if v := r.store.lastViewer; v.UserID != "u-2" || v.Role != auth.RoleAdmin {
		t.Fatalf("viewer = %+v", v)
	}
}

func TestPublicEndpointsRejectBadTokensButAllowNone(t *testing.T) {
	r := newRig(t)
	if rec := r.do(get, "/v1/titles", "", ""); rec.Code != 200 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	if rec := r.do(get, "/v1/titles", "", "not.a.jwt"); rec.Code != http.StatusUnauthorized || errOf(t, rec).Error.Code != "invalid_token" {
		t.Fatalf("bad token on a public endpoint must be a 401: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(get, "/v1/home", "", "not.a.jwt"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("home: %d", rec.Code)
	}
}

func TestCachingHeaders(t *testing.T) {
	r := newRig(t)
	tok := r.token("u-1", auth.RoleViewer)
	cases := []struct {
		path, anon, authed string
	}{
		{"/v1/titles", ccList, ccPrivate},
		{"/v1/home", ccHome, ccPrivate},
	}
	for _, c := range cases {
		anon, signed := r.do(get, c.path, "", ""), r.do(get, c.path, "", tok)
		if anon.Header().Get("Cache-Control") != c.anon || signed.Header().Get("Cache-Control") != c.authed {
			t.Errorf("%s: anon=%q signed=%q", c.path, anon.Header().Get("Cache-Control"), signed.Header().Get("Cache-Control"))
		}
		if !strings.Contains(anon.Header().Get("Vary"), "Authorization") || anon.Header().Get("ETag") == "" {
			t.Errorf("%s: missing Vary or ETag: %v", c.path, anon.Header())
		}
	}
	if rec := r.do(get, "/v1/genres", "", tok); rec.Header().Get("Cache-Control") != ccGenres {
		t.Errorf("genres: %q", rec.Header().Get("Cache-Control"))
	}
	for _, path := range []string{"/v1/me/watchlist", "/v1/me/continue-watching"} {
		if cc := r.do(get, path, "", tok).Header().Get("Cache-Control"); cc != ccNoStore {
			t.Errorf("%s: %q", path, cc)
		}
	}
}

func TestETagAndConditionalGet(t *testing.T) {
	r := newRig(t)
	for _, path := range []string{"/v1/genres", "/v1/home", "/v1/titles"} {
		first := r.do(get, path, "", "")
		etag := first.Header().Get("ETag")
		if first.Code != 200 || etag == "" {
			t.Fatalf("%s: %d etag=%q", path, first.Code, etag)
		}
		for _, hdr := range []string{etag, "W/" + etag, "*", `"nope", ` + etag} {
			rec := r.do(get, path, "", "", "If-None-Match", hdr)
			if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") == "" {
				t.Errorf("%s with If-None-Match %q: %d body=%q", path, hdr, rec.Code, rec.Body.String())
			}
		}
		if rec := r.do(get, path, "", "", "If-None-Match", `"stale"`); rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s with a stale ETag must return the body: %d", path, rec.Code)
		}
	}
	// the ETag changes when the body changes
	r.store.rows = []listRow{{TitleSummary: summary("new")}}
	if r.do(get, "/v1/titles", "", "").Header().Get("ETag") == r.do(get, "/v1/genres", "", "").Header().Get("ETag") {
		t.Error("different bodies share an ETag")
	}
}

func TestTitleDetail(t *testing.T) {
	r := newRig(t)
	for _, slug := range []string{"Upper", "has_underscore", "with space", strings.Repeat("a", 121), "-lead", "trail-", "a--b"} {
		before := r.store.lastViewer
		rec := r.do(get, "/v1/titles/"+strings.ReplaceAll(slug, " ", "%20"), "", "")
		if rec.Code != 404 || errOf(t, rec).Error.Code != "title_not_found" {
			t.Errorf("%q: %d %s", slug, rec.Code, rec.Body.String())
		}
		_ = before
	}
	r.store.detailErr = ErrNotFound
	if rec := r.do(get, "/v1/titles/ghost", "", ""); rec.Code != 404 || errOf(t, rec).Error.Code != "title_not_found" {
		t.Errorf("unknown: %d", rec.Code)
	}
	r.store.detailErr = nil
	r.store.detail = TitleDetail{TitleCore: TitleCore{ID: someID, Slug: "ok", Type: "movie", Name: "OK", Status: "published"},
		Genres: []Genre{}, Directors: []Credit{}, Writers: []Credit{}, Cast: []Credit{}, Subtitles: []Subtitle{}, Seasons: []Season{}}
	rec := r.do(get, "/v1/titles/ok", "", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != ccDetail || strings.Contains(rec.Body.String(), `"viewer"`) {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestWatchlistEndpoints(t *testing.T) {
	r := newRig(t)
	tok := r.token("u-1", auth.RoleViewer)
	for _, c := range []struct{ method, path, body string }{
		{get, "/v1/me/watchlist", ""}, {post, "/v1/me/watchlist", `{"title_id":"` + someID + `"}`},
		{del, "/v1/me/watchlist/" + someID, ""}, {get, "/v1/me/continue-watching", ""},
		{put, "/v1/me/progress", `{}`},
	} {
		if rec := r.do(c.method, c.path, c.body, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d", c.method, c.path, rec.Code)
		}
	}
	if rec := r.do(post, "/v1/me/watchlist", `{"title_id":"nope"}`, tok); rec.Code != 422 || errOf(t, rec).Error.Fields["title_id"] == "" {
		t.Errorf("bad id: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(post, "/v1/me/watchlist", `{"title_id":"`+someID+`","extra":1}`, tok); rec.Code != 400 {
		t.Errorf("unknown field: %d", rec.Code)
	}
	rec := r.do(post, "/v1/me/watchlist", `{"title_id":"`+someID+`"}`, tok)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"added_at"`) {
		t.Errorf("created: %d %s", rec.Code, rec.Body.String())
	}
	r.store.watchAdded = false
	if rec := r.do(post, "/v1/me/watchlist", `{"title_id":"`+someID+`"}`, tok); rec.Code != 200 {
		t.Errorf("already there must be 200: %d", rec.Code)
	}
	r.store.titleErr = ErrNotFound
	if rec := r.do(post, "/v1/me/watchlist", `{"title_id":"`+someID+`"}`, tok); rec.Code != 404 {
		t.Errorf("unknown/invisible title: %d", rec.Code)
	}
	r.store.titleErr, r.store.watchErr = nil, ErrWatchlistFull
	if rec := r.do(post, "/v1/me/watchlist", `{"title_id":"`+someID+`"}`, tok); rec.Code != 409 || errOf(t, rec).Error.Code != "watchlist_full" {
		t.Errorf("full: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.do(del, "/v1/me/watchlist/not-a-uuid", "", tok); rec.Code != 422 {
		t.Errorf("delete bad id: %d", rec.Code)
	}
	if rec := r.do(del, "/v1/me/watchlist/"+someID, "", tok); rec.Code != 204 || rec.Body.Len() != 0 {
		t.Errorf("delete: %d", rec.Code)
	}
	if rec := r.do(get, "/v1/me/watchlist?cursor=junk", "", tok); rec.Code != 400 {
		t.Errorf("bad cursor: %d", rec.Code)
	}
	if rec := r.do(get, "/v1/me/watchlist?limit=99", "", tok); rec.Code != 422 {
		t.Errorf("bad limit: %d", rec.Code)
	}
}

func TestProgressEndpoint(t *testing.T) {
	r := newRig(t)
	tok := r.token("u-1", auth.RoleViewer)
	send := func(body string) *httptest.ResponseRecorder { return r.do(put, "/v1/me/progress", body, tok) }
	tid := `"title_id":"` + someID + `"`

	for name, c := range map[string]struct{ body, field string }{
		"no title":          {`{"position_seconds":1,"duration_seconds":100}`, "title_id"},
		"bad episode id":    {`{` + tid + `,"episode_id":"x","position_seconds":1,"duration_seconds":100}`, "episode_id"},
		"missing position":  {`{` + tid + `,"duration_seconds":100}`, "position_seconds"},
		"negative position": {`{` + tid + `,"position_seconds":-1,"duration_seconds":100}`, "position_seconds"},
		"missing duration":  {`{` + tid + `,"position_seconds":1}`, "duration_seconds"},
		"zero duration":     {`{` + tid + `,"position_seconds":0,"duration_seconds":0}`, "duration_seconds"},
		"huge duration":     {`{` + tid + `,"position_seconds":0,"duration_seconds":999999}`, "duration_seconds"},
	} {
		rec := send(c.body)
		if rec.Code != 422 || errOf(t, rec).Error.Fields[c.field] == "" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := send(`{` + tid + `,"position_seconds":1,"duration_seconds":100,"completed":true}`); rec.Code != 400 {
		t.Errorf("a client must not be able to set completed: %d", rec.Code)
	}
	if rec := send(`{` + tid + `,"position_seconds":"1","duration_seconds":100}`); rec.Code != 400 {
		t.Errorf("string number: %d", rec.Code)
	}

	// decimals are accepted and floored; the title id and episode id are echoed
	rec := send(`{` + tid + `,"position_seconds":12.9,"duration_seconds":100.4}`)
	var p Progress
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 200 || p.PositionSeconds != 12 || p.DurationSeconds != 100 || p.EpisodeID != nil || p.Completed {
		t.Fatalf("%d %+v", rec.Code, p)
	}
	if r.store.progressIn.UserID != "u-1" {
		t.Fatalf("progress stored for %q", r.store.progressIn.UserID)
	}
	if rec.Header().Get("Cache-Control") != ccNoStore {
		t.Error("progress responses must not be cached")
	}

	r.store.titleType = "series"
	if rec := send(`{` + tid + `,"position_seconds":1,"duration_seconds":100}`); rec.Code != 422 || errOf(t, rec).Error.Fields["episode_id"] == "" {
		t.Errorf("series without an episode: %d %s", rec.Code, rec.Body.String())
	}
	r.store.titleType = "movie"
	if rec := send(`{` + tid + `,"episode_id":"` + someID + `","position_seconds":1,"duration_seconds":100}`); rec.Code != 422 {
		t.Errorf("movie with an episode: %d", rec.Code)
	}
	r.store.titleType, r.store.episodeOK = "series", false
	if rec := send(`{` + tid + `,"episode_id":"` + someID + `","position_seconds":1,"duration_seconds":100}`); rec.Code != 422 {
		t.Errorf("episode of another title: %d", rec.Code)
	}
	r.store.titleErr = ErrNotFound
	if rec := send(`{` + tid + `,"position_seconds":1,"duration_seconds":100}`); rec.Code != 404 {
		t.Errorf("invisible title: %d", rec.Code)
	}
}

func TestSaveProgress_ClampAndCompletion(t *testing.T) {
	r := newRig(t)
	v := Viewer{UserID: "u-1", Role: auth.RoleViewer}
	cases := []struct {
		pos, dur  int
		wantPos   int
		completed bool
	}{
		{0, 1000, 0, false},
		{949, 1000, 949, false},
		{950, 1000, 950, true}, // exactly 95%
		{951, 1000, 951, true},
		{1000, 1000, 1000, true},
		{5000, 1000, 1000, true}, // beyond the end is clamped, not rejected
		{-5, 1000, 0, false},
		{94, 99, 94, false}, // 94.9%
		{95, 100, 95, true},
		{1, 1, 1, true},
	}
	for _, c := range cases {
		got, err := r.svc.SaveProgress(context.Background(), v, ProgressUpdate{TitleID: someID, PositionSeconds: c.pos, DurationSeconds: c.dur})
		if err != nil || got.PositionSeconds != c.wantPos || got.Completed != c.completed {
			t.Errorf("pos=%d dur=%d: got %+v err=%v, want pos=%d completed=%v", c.pos, c.dur, got, err, c.wantPos, c.completed)
		}
	}
}

func TestProgressIsThrottledPerUser(t *testing.T) {
	r := newRig(t)
	a, b := r.token("user-a", auth.RoleViewer), r.token("user-b", auth.RoleViewer)
	body := `{"title_id":"` + someID + `","position_seconds":1,"duration_seconds":100}`
	for i := 0; i < 30; i++ {
		if rec := r.do(put, "/v1/me/progress", body, a); rec.Code != 200 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := r.do(put, "/v1/me/progress", body, a)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "10" || errOf(t, rec).Error.Code != "rate_limited" {
		t.Fatalf("31st request: %d %v", rec.Code, rec.Header())
	}
	if rec := r.do(put, "/v1/me/progress", body, b); rec.Code != 200 {
		t.Fatalf("another user must not be throttled: %d", rec.Code)
	}
	r.limiter.err = errors.New("redis down")
	if rec := r.do(put, "/v1/me/progress", body, b); rec.Code != 503 {
		t.Fatalf("limiter down must fail closed: %d", rec.Code)
	}
}

func TestMethodsAndRoutes(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, path string }{
		{post, "/v1/titles"}, {del, "/v1/titles/x"}, {post, "/v1/genres"}, {put, "/v1/home"},
		{put, "/v1/me/watchlist"}, {get, "/v1/me/watchlist/" + someID}, {post, "/v1/me/progress"}, {post, "/v1/me/continue-watching"},
	} {
		tok := r.token("u", auth.RoleViewer)
		if rec := r.do(c.method, c.path, "", tok); rec.Code != http.StatusMethodNotAllowed || errOf(t, rec).Error.Code != "method_not_allowed" {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
}

func TestHomeIsBuiltOncePerBurst(t *testing.T) {
	r := newRig(t)
	r.store.homeDelay = 80 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := r.do(get, "/v1/home", "", ""); rec.Code != 200 {
				t.Errorf("home: %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if n := r.store.homeCalls.Load(); n != 1 {
		t.Fatalf("25 simultaneous cold requests hit the database %d times, want 1", n)
	}
}
