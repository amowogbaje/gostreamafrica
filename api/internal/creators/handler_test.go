package creators

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
)

// memLimiter is an in-memory ratelimit.Store: it counts hits per key.
type memLimiter struct {
	mu sync.Mutex
	n  map[string]int64
}

func (m *memLimiter) Hit(_ context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int64{}
	}
	m.n[key]++
	return m.n[key], window, nil
}

// testAuth stands in for the JWT middleware: X-Test-User and X-Test-Role become the Principal.
func testAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.Header.Get("X-Test-User")
		if user == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"authentication required"}}`))
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: user, Role: auth.Role(r.Header.Get("X-Test-Role"))})))
	})
}

type httpEnv struct {
	*env
	h http.Handler
}

func newHTTP(t *testing.T, mods ...func(*Config)) *httpEnv {
	t.Helper()
	e := newEnv(t, mods...)
	mux := http.NewServeMux()
	NewHandler(e.svc, testAuth, &memLimiter{}, quietLogger()).Mount(mux)
	return &httpEnv{env: e, h: mux}
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("response is not the expected JSON (%v): %s", err, r.body)
	}
}

func (r resp) errCode(t *testing.T) string {
	t.Helper()
	var b struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Fields  map[string]string `json:"fields"`
		} `json:"error"`
	}
	r.json(t, &b)
	return b.Error.Code
}

type who struct{ user, role string }

var (
	asCreator = who{creatorID, "creator"}
	asOther   = who{otherID, "creator"}
	asAdmin   = who{adminID, "admin"}
	asViewer  = who{creatorID, "viewer"}
	nobody    = who{}
)

func (e *httpEnv) do(method, path string, w who, body any) resp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rdr = strings.NewReader(b)
		default:
			raw, _ := json.Marshal(b)
			rdr = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if w.user != "" {
		req.Header.Set("X-Test-User", w.user)
		req.Header.Set("X-Test-Role", w.role)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func createBody(title string, size int64) map[string]any {
	return map[string]any{"title_id": title, "filename": "film.mp4", "content_type": "video/mp4", "size_bytes": size}
}

func TestHTTP_AccessControl(t *testing.T) {
	e := newHTTP(t)
	u := e.create(10 * mib).Upload
	routes := []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/creator/uploads", createBody(titleA, 10*mib)},
		{"GET", "/v1/creator/uploads/" + u.ID, nil},
		{"DELETE", "/v1/creator/uploads/" + u.ID, nil},
		{"POST", "/v1/creator/uploads/" + u.ID + "/parts", map[string]any{"part_numbers": []int{1}}},
		{"POST", "/v1/creator/uploads/" + u.ID + "/complete", nil},
	}
	for _, r := range routes {
		t.Run(r.method+" "+strings.TrimPrefix(strings.Replace(r.path, u.ID, "{id}", 1), "/v1/creator/"), func(t *testing.T) {
			if got := e.do(r.method, r.path, nobody, r.body); got.code != 401 {
				t.Errorf("anonymous: %d, want 401", got.code)
			}
			for _, viewer := range []who{asViewer, {creatorID, ""}, {creatorID, "superuser"}} {
				got := e.do(r.method, r.path, viewer, r.body)
				if got.code != 403 || got.errCode(t) != "forbidden" {
					t.Errorf("role %q: %d %s, want 403 forbidden", viewer.role, got.code, got.body)
				}
			}
		})
	}
	if got := e.do("PUT", "/v1/creator/uploads", asCreator, nil); got.code != 405 {
		t.Errorf("PUT: %d, want 405", got.code)
	}
	if got := e.do("GET", "/v1/creator/uploads", asCreator, nil); got.code != 405 {
		t.Errorf("GET list is not offered: %d", got.code)
	}
}

func TestHTTP_CreateGetPartsCompleteDeleteFlow(t *testing.T) {
	e := newHTTP(t)

	// create
	got := e.do("POST", "/v1/creator/uploads", asCreator, createBody(titleA, 200*mib))
	if got.code != 201 || got.header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(got.header.Get("Content-Type"), "application/json") {
		t.Fatalf("create: %d %v %s", got.code, got.header, got.body)
	}
	var created struct {
		Upload struct {
			ID        string `json:"id"`
			TitleID   string `json:"title_id"`
			Status    string `json:"status"`
			PartCount int    `json:"part_count"`
		} `json:"upload"`
		Parts []struct {
			PartNumber int    `json:"part_number"`
			URL        string `json:"url"`
			SizeBytes  int64  `json:"size_bytes"`
		} `json:"parts"`
		URLsExpireAt string `json:"urls_expire_at"`
	}
	got.json(t, &created)
	up := created.Upload
	if up.ID == "" || up.Status != "initiated" || up.TitleID != titleA || up.PartCount != 4 || len(created.Parts) != 4 || created.URLsExpireAt == "" {
		t.Fatalf("created: %s", got.body)
	}
	if !strings.Contains(string(got.body), `"part_size_bytes":67108864`) || strings.Contains(string(got.body), "s3_upload_id") ||
		strings.Contains(string(got.body), "object_key") || strings.Contains(string(got.body), "bucket") {
		t.Fatalf("response leaks storage internals or lacks the part size: %s", got.body)
	}

	// get while unfinished: stored_parts is present, even when empty
	got = e.do("GET", "/v1/creator/uploads/"+up.ID, asCreator, nil)
	if got.code != 200 || !strings.Contains(string(got.body), `"stored_parts":[]`) || got.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("get: %d %s", got.code, got.body)
	}
	e.objs.putPart(e.store.uploads[up.ID].S3UploadID, 1, 64*mib)
	got = e.do("GET", "/v1/creator/uploads/"+up.ID, asCreator, nil)
	if !strings.Contains(string(got.body), `"stored_parts":[{"part_number":1,"size_bytes":67108864}]`) {
		t.Fatalf("stored parts: %s", got.body)
	}

	// more part URLs
	got = e.do("POST", "/v1/creator/uploads/"+up.ID+"/parts", asCreator, map[string]any{"part_numbers": []int{4, 3}})
	var parts struct {
		Parts []struct {
			PartNumber int `json:"part_number"`
		}
		URLsExpireAt string `json:"urls_expire_at"`
	}
	got.json(t, &parts)
	if got.code != 200 || len(parts.Parts) != 2 || parts.Parts[0].PartNumber != 3 || parts.URLsExpireAt == "" || got.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("parts: %d %s", got.code, got.body)
	}

	// complete too early: the 422 names what is missing
	got = e.do("POST", "/v1/creator/uploads/"+up.ID+"/complete", asCreator, nil)
	if got.code != 422 || got.errCode(t) != "upload_incomplete" || !strings.Contains(string(got.body), "2, 3, 4") {
		t.Fatalf("early complete: %d %s", got.code, got.body)
	}

	// complete for real
	e.objs.putAllParts(e.store.uploads[up.ID])
	got = e.do("POST", "/v1/creator/uploads/"+up.ID+"/complete", asCreator, nil)
	var done struct {
		Status string `json:"status"`
		Job    struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"job"`
		CompletedAt string `json:"completed_at"`
	}
	got.json(t, &done)
	if got.code != 202 || done.Status != "uploaded" || done.Job.State != "queued" || done.Job.ID == "" || done.CompletedAt == "" {
		t.Fatalf("complete: %d %s", got.code, got.body)
	}
	if strings.Contains(string(got.body), "stored_parts") {
		t.Fatalf("a finished upload has no stored_parts: %s", got.body)
	}
	if again := e.do("POST", "/v1/creator/uploads/"+up.ID+"/complete", asCreator, nil); again.code != 202 {
		t.Fatalf("calling complete twice: %d", again.code)
	}

	// cancelling a completed upload is refused; the state is still readable
	if del := e.do("DELETE", "/v1/creator/uploads/"+up.ID, asCreator, nil); del.code != 409 || del.errCode(t) != "upload_not_cancellable" {
		t.Fatalf("delete after complete: %d %s", del.code, del.body)
	}
	if g := e.do("GET", "/v1/creator/uploads/"+up.ID, asAdmin, nil); g.code != 200 || !strings.Contains(string(g.body), `"state":"queued"`) {
		t.Fatalf("admin get: %d %s", g.code, g.body)
	}
}

func TestHTTP_Delete(t *testing.T) {
	e := newHTTP(t)
	u := e.create(10 * mib).Upload
	if got := e.do("DELETE", "/v1/creator/uploads/"+u.ID, asOther, nil); got.code != 404 || got.errCode(t) != "upload_not_found" {
		t.Fatalf("someone else's upload: %d %s", got.code, got.body)
	}
	for i := 0; i < 2; i++ { // cancelling twice is fine
		got := e.do("DELETE", "/v1/creator/uploads/"+u.ID, asCreator, nil)
		if got.code != 204 || len(got.body) != 0 || got.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("delete #%d: %d %q", i+1, got.code, got.body)
		}
	}
	if got := e.do("GET", "/v1/creator/uploads/"+u.ID, asCreator, nil); !strings.Contains(string(got.body), `"status":"aborted"`) {
		t.Fatalf("%s", got.body)
	}
}

func TestHTTP_ErrorMapping(t *testing.T) {
	e := newHTTP(t)
	other := uuidN(7)
	e.addTitle(other, otherID, "movie", false)

	type c struct {
		name         string
		method, path string
		who          who
		body         any
		status       int
		code         string
	}
	u := e.create(10 * mib).Upload
	cases := []c{
		{"invalid json", "POST", "/v1/creator/uploads", asCreator, `{"title_id":`, 400, "invalid_json"},
		{"unknown field", "POST", "/v1/creator/uploads", asCreator, `{"title_id":"x","bogus":1}`, 400, ""},
		{"validation lists every bad field", "POST", "/v1/creator/uploads", asCreator, map[string]any{"title_id": "x", "filename": "a.avi", "size_bytes": 0}, 422, "validation_failed"},
		{"someone else's title", "POST", "/v1/creator/uploads", asCreator, createBody(other, 10*mib), 404, "title_not_found"},
		{"title already has an upload", "POST", "/v1/creator/uploads", asCreator, createBody(titleA, 10*mib), 409, "upload_in_progress"},
		{"unknown upload", "GET", "/v1/creator/uploads/" + uuidN(404), asCreator, nil, 404, "upload_not_found"},
		{"garbage id", "GET", "/v1/creator/uploads/garbage", asCreator, nil, 404, "upload_not_found"},
		{"part out of range", "POST", "/v1/creator/uploads/" + u.ID + "/parts", asCreator, map[string]any{"part_numbers": []int{9}}, 422, "validation_failed"},
		{"no parts asked for", "POST", "/v1/creator/uploads/" + u.ID + "/parts", asCreator, map[string]any{"part_numbers": []int{}}, 422, "validation_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := e.do(tc.method, tc.path, tc.who, tc.body)
			if got.code != tc.status || (tc.code != "" && got.errCode(t) != tc.code) {
				t.Fatalf("%d %s, want %d %s", got.code, got.body, tc.status, tc.code)
			}
			var body struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			got.json(t, &body)
			if body.Error.Code == "" || body.Error.Message == "" {
				t.Fatalf("every error needs a code and a message: %s", got.body)
			}
		})
	}

	t.Run("validation errors carry per-field messages", func(t *testing.T) {
		got := e.do("POST", "/v1/creator/uploads", asCreator, map[string]any{"title_id": "x", "filename": "a.avi", "size_bytes": 0})
		var body struct {
			Error struct{ Fields map[string]string } `json:"error"`
		}
		got.json(t, &body)
		for _, f := range []string{"title_id", "filename", "size_bytes"} {
			if body.Error.Fields[f] == "" {
				t.Errorf("no message for %s: %s", f, got.body)
			}
		}
	})
}

func TestHTTP_StorageAndInternalFailuresAreOpaque(t *testing.T) {
	e := newHTTP(t)
	u := e.readyUpload(10 * mib)

	e.objs.failNext["ListParts"] = errors.New("dial tcp 10.0.0.5:8333: secret internal detail")
	got := e.do("POST", "/v1/creator/uploads/"+u.ID+"/complete", asCreator, nil)
	if got.code != 502 || got.errCode(t) != "storage_unavailable" || strings.Contains(string(got.body), "10.0.0.5") || strings.Contains(string(got.body), "secret") {
		t.Fatalf("%d %s", got.code, got.body)
	}

	e.scanner.err = errors.New("clamd at 10.0.0.9 refused")
	got = e.do("POST", "/v1/creator/uploads/"+u.ID+"/complete", asCreator, nil)
	if got.code != 503 || got.errCode(t) != "scan_unavailable" || strings.Contains(string(got.body), "10.0.0.9") {
		t.Fatalf("%d %s", got.code, got.body)
	}

	e.svc.store = createFails{Store: e.store, err: errors.New("pq: password authentication failed for user admin")}
	e.store.uploads = map[string]Upload{}
	got = e.do("POST", "/v1/creator/uploads", asCreator, createBody(titleA, 10*mib))
	if got.code != 500 || got.errCode(t) != "internal_error" || strings.Contains(string(got.body), "password") {
		t.Fatalf("%d %s", got.code, got.body)
	}
}

func TestHTTP_RejectionsAreReportedWithTheirCode(t *testing.T) {
	e := newHTTP(t)
	u := e.readyUpload(10 * mib)
	e.objs.setHead(u.S3UploadID, []byte("MZ this is a windows executable, not a film"))
	got := e.do("POST", "/v1/creator/uploads/"+u.ID+"/complete", asCreator, nil)
	if got.code != 422 || got.errCode(t) != RejectUnsupportedContainer {
		t.Fatalf("%d %s", got.code, got.body)
	}
	g := e.do("GET", "/v1/creator/uploads/"+u.ID, asCreator, nil)
	if !strings.Contains(string(g.body), `"status":"rejected"`) || !strings.Contains(string(g.body), `"reject_code":"unsupported_container"`) ||
		!strings.Contains(string(g.body), `"reject_reason"`) {
		t.Fatalf("%s", g.body)
	}
	if again := e.do("POST", "/v1/creator/uploads/"+u.ID+"/complete", asCreator, nil); again.code != 409 || again.errCode(t) != "upload_rejected" {
		t.Fatalf("%d %s", again.code, again.body)
	}
}

func TestHTTP_RateLimits(t *testing.T) {
	e := newHTTP(t)
	bad := `{}`
	var last resp
	for i := 1; i <= 11; i++ {
		last = e.do("POST", "/v1/creator/uploads", asCreator, bad)
		if i <= 10 && last.code == 429 {
			t.Fatalf("request %d was limited too early", i)
		}
	}
	if last.code != 429 {
		t.Fatalf("the 11th create in an hour must be limited, got %d", last.code)
	}
	if got := e.do("POST", "/v1/creator/uploads", asOther, bad); got.code == 429 {
		t.Fatal("limits are per creator")
	}
}
