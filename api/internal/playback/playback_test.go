package playback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/testdb"
)

const (
	testKey   = "0123456789abcdef0123456789abcdef"
	assetID   = "3f2b8c1e-7d5a-4b9e-9c3a-1d2e3f4a5b6c"
	titleID   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	ownerID   = "11111111-1111-4111-8111-111111111111"
	viewerID  = "22222222-2222-4222-8222-222222222222"
	adminID   = "33333333-3333-4333-8333-333333333333"
	fixedUnix = 1893456000
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestToken_MatchesNginxSecureLinkMD5(t *testing.T) {
	// Reference value computed independently: base64url(md5("<expires><asset> <key>")) with Python's hashlib.
	s := NewSigner("http://localhost:8088", testKey)
	if got := s.Token(assetID, fixedUnix); got != "_0AJ_9lGT5UpaJmkbt9MSw" {
		t.Fatalf("token %q does not match what nginx's secure_link_md5 computes", got)
	}
	if strings.ContainsAny(s.Token(assetID, fixedUnix), "+/=") {
		t.Fatal("tokens are URL-safe base64 without padding")
	}
}

func TestSigner_URLShape(t *testing.T) {
	s := NewSigner("http://localhost:8088/", testKey)
	exp := time.Unix(fixedUnix, 0)
	got := s.URL(assetID, exp, "360p/seg_00001.m4s")
	want := "http://localhost:8088/hls/_0AJ_9lGT5UpaJmkbt9MSw/" + strconv.Itoa(fixedUnix) + "/" + assetID + "/360p/seg_00001.m4s"
	if got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
	if s.URL(assetID, exp, "/master.m3u8") != "http://localhost:8088/hls/_0AJ_9lGT5UpaJmkbt9MSw/"+strconv.Itoa(fixedUnix)+"/"+assetID+"/master.m3u8" {
		t.Fatal("a leading slash on the file is dropped")
	}
	// relative playlist URLs resolve inside the signed prefix, so segments inherit the token
	base, _ := url.Parse(s.URL(assetID, exp, "master.m3u8"))
	rel, _ := base.Parse("360p/index.m3u8")
	if !strings.HasPrefix(rel.String(), "http://localhost:8088/hls/_0AJ_9lGT5UpaJmkbt9MSw/"+strconv.Itoa(fixedUnix)+"/"+assetID+"/360p/") {
		t.Fatalf("relative resolution lost the token: %s", rel)
	}
}

func TestVerify_ExpiryAndTampering(t *testing.T) {
	s := NewSigner("http://x", testKey)
	exp := int64(fixedUnix)
	good := s.Token(assetID, exp)
	before, after := time.Unix(exp-1, 0), time.Unix(exp+1, 0)
	if err := s.Verify(good, exp, assetID, before); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(good, exp, assetID, time.Unix(exp, 0)); err != nil {
		t.Fatalf("valid until the expiry second: %v", err)
	}
	if err := s.Verify(good, exp, assetID, after); !errors.Is(err, ErrExpired) {
		t.Fatalf("after expiry: %v", err)
	}
	flip := func(tok string) string {
		b := []byte(tok)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	cases := map[string]error{
		"a changed token":      s.Verify(flip(good), exp, assetID, before),
		"a longer expiry":      s.Verify(good, exp+86400, assetID, before),
		"another asset":        s.Verify(good, exp, "3f2b8c1e-7d5a-4b9e-9c3a-1d2e3f4a5b6d", before),
		"another key":          NewSigner("http://x", "fedcba9876543210fedcba9876543210").Verify(good, exp, assetID, before),
		"an empty token":       s.Verify("", exp, assetID, before),
		"a truncated token":    s.Verify(good[:10], exp, assetID, before),
		"an extended token":    s.Verify(good+"A", exp, assetID, before),
		"garbage":              s.Verify("!!!", exp, assetID, before),
		"expired AND tampered": s.Verify(flip(good), exp, assetID, after),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s must be ErrInvalid, got %v (a tampered token must never be reported as merely expired)", name, err)
		}
	}
}

// ---------------------------------------------------------------- service

type fakeStore struct {
	assets map[string]Asset
	err    error
}

func (f fakeStore) Ready(_ context.Context, id string) (Asset, error) {
	if f.err != nil {
		return Asset{}, f.err
	}
	a, ok := f.assets[id]
	if !ok {
		return Asset{}, ErrNotFound
	}
	return a, nil
}

type fakeEnt struct {
	allow bool
	err   error
	calls int
}

func (f *fakeEnt) CanWatch(context.Context, string, Title) (bool, error) {
	f.calls++
	return f.allow, f.err
}

func newSvc(status string, ent Entitlements) (*Service, time.Time) {
	now := time.Unix(fixedUnix-4*3600, 0).UTC() // a 4 h window that ends exactly at fixedUnix
	st := fakeStore{assets: map[string]Asset{titleID: {
		ID: assetID, DurationSeconds: 5400, Title: Title{ID: titleID, Status: status, CreatorID: ownerID},
		Subtitles: []SubtitleTrack{{"en", "EN", "subtitles/en.vtt"}, {"pt-BR", "PT-BR", "subtitles/pt-BR.vtt"}},
	}}}
	return NewService(st, ent, NewSigner("http://localhost:8088", testKey), 4*time.Hour, "http://localhost:8088/public", quiet(), func() time.Time { return now }), now
}

func TestService_PublishedTitle(t *testing.T) {
	ent := &fakeEnt{allow: true}
	svc, now := newSvc("published", ent)
	pb, err := svc.Playback(context.Background(), Viewer{UserID: viewerID}, titleID)
	if err != nil {
		t.Fatal(err)
	}
	exp := now.Add(4 * time.Hour).Truncate(time.Second)
	if !pb.ExpiresAt.Equal(exp) || pb.AssetID != assetID || pb.DurationSeconds != 5400 || ent.calls != 1 {
		t.Fatalf("%+v calls=%d", pb, ent.calls)
	}
	signer := NewSigner("http://localhost:8088", testKey)
	if pb.ManifestURL != signer.URL(assetID, exp, "master.m3u8") {
		t.Fatalf("%s", pb.ManifestURL)
	}
	if len(pb.Subtitles) != 2 || pb.Subtitles[1].Language != "pt-BR" || pb.Subtitles[1].URL != signer.URL(assetID, exp, "subtitles/pt-BR.vtt") {
		t.Fatalf("subtitle tracks are signed too: %+v", pb.Subtitles)
	}
	if pb.PosterURL != "http://localhost:8088/public/"+titleID+"/poster.jpg" || pb.ThumbnailsURL != "http://localhost:8088/public/"+titleID+"/thumbs.vtt" {
		t.Fatalf("%+v", pb)
	}
	if s := signer.Token(assetID, exp.Unix()); !strings.Contains(pb.ManifestURL, "/"+s+"/") {
		t.Fatal("manifest token")
	}
}

func TestService_Access(t *testing.T) {
	cases := []struct {
		name   string
		status string
		who    Viewer
		allow  bool
		want   error
		asked  bool // was the entitlement hook consulted?
	}{
		{"published, entitled", "published", Viewer{UserID: viewerID}, true, nil, true},
		{"published, not entitled", "published", Viewer{UserID: viewerID}, false, ErrNotEntitled, true},
		{"draft looks missing to a stranger", "draft", Viewer{UserID: viewerID}, true, ErrNotFound, false},
		{"unpublished looks missing", "unpublished", Viewer{UserID: viewerID}, true, ErrNotFound, false},
		{"processing looks missing", "processing", Viewer{UserID: viewerID}, true, ErrNotFound, false},
		{"owner previews a draft", "draft", Viewer{UserID: ownerID}, false, nil, false},
		{"owner previews an unpublished title", "unpublished", Viewer{UserID: ownerID}, false, nil, false},
		{"admin previews anything", "draft", Viewer{UserID: adminID, Admin: true}, false, nil, false},
		{"anonymous user id never matches an owner", "draft", Viewer{}, true, ErrNotFound, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ent := &fakeEnt{allow: c.allow}
			svc, _ := newSvc(c.status, ent)
			_, err := svc.Playback(context.Background(), c.who, titleID)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if (ent.calls > 0) != c.asked {
				t.Fatalf("entitlement hook consulted=%v, want %v", ent.calls > 0, c.asked)
			}
		})
	}
}

func TestService_Errors(t *testing.T) {
	svc, _ := newSvc("published", &fakeEnt{allow: true})
	if _, err := svc.Playback(context.Background(), Viewer{UserID: viewerID}, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	boom := errors.New("db down")
	svc.store = fakeStore{err: boom}
	if _, err := svc.Playback(context.Background(), Viewer{UserID: viewerID}, titleID); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	svc2, _ := newSvc("published", &fakeEnt{err: boom})
	if _, err := svc2.Playback(context.Background(), Viewer{UserID: viewerID}, titleID); !errors.Is(err, boom) {
		t.Fatalf("an entitlement failure is an error, not a refusal: %v", err)
	}
}

func TestService_NoSubtitlesIsAnEmptyList(t *testing.T) {
	svc, _ := newSvc("published", &fakeEnt{allow: true})
	a := svc.store.(fakeStore).assets[titleID]
	a.Subtitles = nil
	svc.store = fakeStore{assets: map[string]Asset{titleID: a}}
	pb, err := svc.Playback(context.Background(), Viewer{UserID: viewerID}, titleID)
	if err != nil || pb.Subtitles == nil || len(pb.Subtitles) != 0 {
		t.Fatalf("%+v %v", pb, err)
	}
}

func TestPublishedOnly(t *testing.T) {
	for status, want := range map[string]bool{"published": true, "draft": false, "unpublished": false, "processing": false} {
		if got, _ := (PublishedOnly{}).CanWatch(context.Background(), viewerID, Title{Status: status}); got != want {
			t.Errorf("%s: %v", status, got)
		}
	}
}

// ---------------------------------------------------------------- handler

type memLimiter struct {
	mu sync.Mutex
	n  map[string]int64
}

func (m *memLimiter) Hit(_ context.Context, key string, w time.Duration) (int64, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int64{}
	}
	m.n[key]++
	return m.n[key], w, nil
}

func testAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := r.Header.Get("X-Test-User")
		if u == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: u, Role: auth.Role(r.Header.Get("X-Test-Role"))})))
	})
}

func serve(t *testing.T, status string, ent Entitlements) http.Handler {
	t.Helper()
	svc, _ := newSvc(status, ent)
	mux := http.NewServeMux()
	NewHandler(svc, testAuth, &memLimiter{}, quiet()).Mount(mux)
	return mux
}

func get(h http.Handler, path, user, role string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != "" {
		req.Header.Set("X-Test-User", user)
		req.Header.Set("X-Test-Role", role)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct{ Error struct{ Code string } }
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return b.Error.Code
}

func TestHTTP(t *testing.T) {
	h := serve(t, "published", &fakeEnt{allow: true})
	if rec := get(h, "/v1/playback/"+titleID, "", ""); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	rec := get(h, "/v1/playback/"+titleID, viewerID, "viewer")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	var body struct {
		PlayableID, AssetID string                                  `json:"-"`
		ManifestURL         string                                  `json:"manifest_url"`
		ExpiresAt           string                                  `json:"expires_at"`
		Duration            int                                     `json:"duration_seconds"`
		Subtitles           []struct{ Language, Label, URL string } `json:"subtitles"`
		Poster              string                                  `json:"poster_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.ManifestURL, "http://localhost:8088/hls/") || !strings.HasSuffix(body.ManifestURL, "/master.m3u8") ||
		body.ExpiresAt != "2030-01-01T00:00:00Z" || body.Duration != 5400 || len(body.Subtitles) != 2 || body.Poster == "" {
		t.Fatalf("%s", rec.Body)
	}
	for _, id := range []string{"garbage", "00000000-0000-4000-8000-000000000000"} {
		rec := get(h, "/v1/playback/"+id, viewerID, "viewer")
		if rec.Code != 404 || errCode(t, rec) != "not_playable" {
			t.Errorf("%s: %d %s", id, rec.Code, rec.Body)
		}
	}
	denied := serve(t, "published", &fakeEnt{allow: false})
	if rec := get(denied, "/v1/playback/"+titleID, viewerID, "viewer"); rec.Code != 403 || errCode(t, rec) != "not_entitled" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	draft := serve(t, "draft", &fakeEnt{allow: true})
	if rec := get(draft, "/v1/playback/"+titleID, viewerID, "viewer"); rec.Code != 404 {
		t.Fatalf("a draft is invisible to strangers: %d", rec.Code)
	}
	if rec := get(draft, "/v1/playback/"+titleID, ownerID, "creator"); rec.Code != 200 {
		t.Fatalf("the owner previews: %d", rec.Code)
	}
	if rec := get(draft, "/v1/playback/"+titleID, adminID, "admin"); rec.Code != 200 {
		t.Fatalf("an admin previews: %d", rec.Code)
	}
	broken, _ := newSvc("published", &fakeEnt{allow: true})
	broken.store = fakeStore{err: errors.New("pq: password authentication failed for user admin")}
	mux := http.NewServeMux()
	NewHandler(broken, testAuth, &memLimiter{}, quiet()).Mount(mux)
	rec = get(mux, "/v1/playback/"+titleID, viewerID, "viewer")
	if rec.Code != 500 || errCode(t, rec) != "internal_error" || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("internal errors are opaque: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/v1/playback/"+titleID, viewerID, "viewer"); rec.Code != 200 {
		t.Fatal("still fine")
	}
	limited := 0
	for i := 0; i < 130; i++ {
		if get(h, "/v1/playback/"+titleID, adminID, "admin").Code == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("playback requests are rate limited per user")
	}
	if rec := httptest.NewRecorder(); true {
		req := httptest.NewRequest(http.MethodPost, "/v1/playback/"+titleID, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != 405 {
			t.Fatalf("POST: %d", rec.Code)
		}
	}
}

// ---------------------------------------------------------------- the real store

func hexN(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestPGStore(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	var user string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, role) VALUES ($1::citext, '!', 'creator') RETURNING id::text`, "p-"+hexN(4)+"@example.com").Scan(&user); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO profiles (user_id, display_name) VALUES ($1::uuid, 'p')`, user)
	mkTitle := func(typ, status string) string {
		var id string
		pub := "NULL"
		if status == "published" {
			pub = "now()"
		}
		if err := pool.QueryRow(ctx, `INSERT INTO titles (type, slug, name, original_language, status, creator_id, published_at)
			VALUES ($1::title_type, $2, $2, 'en', $3::title_status, $4::uuid, `+pub+`) RETURNING id::text`, typ, "t-"+hexN(4), status, user).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	store := NewPGStore(pool)

	movie := mkTitle("movie", "published")
	var asset string
	if err := pool.QueryRow(ctx, `INSERT INTO media_assets (title_id, status, hls_manifest_key, duration_seconds) VALUES ($1::uuid, 'ready', 'x/master.m3u8', 90) RETURNING id::text`, movie).Scan(&asset); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO subtitles (asset_id, language, label, vtt_url) VALUES ($1::uuid, 'pt-BR', 'PT-BR', $2), ($1::uuid, 'en', 'EN', $3)`,
		asset, asset+"/subtitles/pt-BR.vtt", asset+"/subtitles/en.vtt")
	a, err := store.Ready(ctx, movie)
	if err != nil || a.ID != asset || a.DurationSeconds != 90 || a.Title.ID != movie || a.Title.Status != "published" || a.Title.CreatorID != user {
		t.Fatalf("%+v %v", a, err)
	}
	if len(a.Subtitles) != 2 || a.Subtitles[0].Language != "en" || a.Subtitles[0].File != "subtitles/en.vtt" || a.Subtitles[1].File != "subtitles/pt-BR.vtt" {
		t.Fatalf("subtitle files are relative to the asset prefix: %+v", a.Subtitles)
	}

	// an episode is playable by its own id and takes the series' status
	series := mkTitle("series", "published")
	var season, episode string
	if err := pool.QueryRow(ctx, `INSERT INTO seasons (title_id, number) VALUES ($1::uuid, 1) RETURNING id::text`, series).Scan(&season); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO episodes (season_id, title_id, number, name) VALUES ($1::uuid, $2::uuid, 1, 'Pilot') RETURNING id::text`, season, series).Scan(&episode); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO media_assets (episode_id, status, hls_manifest_key, duration_seconds) VALUES ($1::uuid, 'ready', 'y/master.m3u8', 45)`, episode)
	if a, err := store.Ready(ctx, episode); err != nil || a.Title.ID != series || a.DurationSeconds != 45 {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := store.Ready(ctx, series); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a series itself has no video: %v", err)
	}

	// assets that are not ready, unknown ids and malformed ids
	processing := mkTitle("movie", "published")
	exec(`INSERT INTO media_assets (title_id, status) VALUES ($1::uuid, 'processing')`, processing)
	failed := mkTitle("movie", "published")
	exec(`INSERT INTO media_assets (title_id, status) VALUES ($1::uuid, 'failed')`, failed)
	for _, id := range []string{processing, failed, "00000000-0000-4000-8000-000000000000", "nope", ""} {
		if _, err := store.Ready(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Ready(%q) = %v", id, err)
		}
	}
}
