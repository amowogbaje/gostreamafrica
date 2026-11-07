// Package contract checks that the HTTP handlers and docs/openapi.yaml describe the same API.
// It runs in CI on every build, so a handler cannot change without the spec following.
package contract

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"streamafrica/api/internal/apidocs"
	"streamafrica/api/internal/app"
	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/config"
)

// loadSpec loads the spec the API actually serves (embedded in the binary). A separate test in
// package apidocs fails when it differs from docs/openapi.yaml.
func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(apidocs.Spec)
	if err != nil {
		t.Fatalf("cannot load the embedded spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("the OpenAPI document is not valid: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return doc, router
}

func specOperations(doc *openapi3.T) map[string]bool {
	ops := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			ops[method+" "+path] = true
		}
	}
	return ops
}

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

func (m *fakeMailer) lastTo(to, subject string) (auth.Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].To == to && strings.Contains(m.sent[i].Subject, subject) {
			return m.sent[i], true
		}
	}
	return auth.Message{}, false
}

func testConfig(s3Addr string) config.Config {
	return config.Config{
		Env: "local", S3Addr: s3Addr,
		JWTSecret: []byte(strings.Repeat("k", 40)), AccessTTL: 15 * time.Minute,
		RefreshTTL: 30 * 24 * time.Hour, RefreshAbsoluteTTL: 90 * 24 * time.Hour,
		RefreshCookiePath: "/v1/auth", AllowedOrigins: []string{"http://localhost:3000"},
		WebBaseURL: "http://localhost:8088", Argon2MemoryKiB: 64, Argon2Time: 1, Argon2Parallelism: 1,
		CatalogHomeTTL: time.Minute, CatalogGenresTTL: time.Minute, APIDocsEnabled: true,
		S3Endpoint: "http://" + s3Addr, S3Region: "us-east-1", S3AccessKey: "test-key", S3SecretKey: "test-secret",
		S3BucketRaw: "media-raw", S3BucketHLS: "media-hls", S3BucketPublic: "media-public",
		UploadMaxBytes: 8 << 30, UploadPartBytes: 64 << 20, UploadURLTTL: 15 * time.Minute, UploadAbandonAfter: 24 * time.Hour,
		UploadSweepEvery: time.Hour, UploadMaxActive: 3, UploadDailyBytes: 32 << 30,
		PlaybackSigningKey: "contract-test-signing-key-0123456789ab", PlaybackBaseURL: "http://localhost:8088", PlaybackTokenTTL: 4 * time.Hour,
		UploadMaxDurationSecs: 4 * 3600, UploadMaxWidth: 3840, UploadMaxHeight: 2160,
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestSpecIsValid(t *testing.T) {
	doc, _ := loadSpec(t)
	if n := len(specOperations(doc)); n < 20 {
		t.Fatalf("only %d operations in the spec", n)
	}
}

// TestRoutesMatchSpec fails when a route exists in code but not in the spec, or the other way
// round. It needs no database: the routes are read while the server is being assembled.
func TestRoutesMatchSpec(t *testing.T) {
	doc, _ := loadSpec(t)
	inCode := map[string]bool{}
	_, err := app.New(app.Options{
		Config: testConfig("127.0.0.1:1"), Logger: quiet(), Mailer: &fakeMailer{},
		OnRoute: func(path string, methods []string) {
			for _, m := range methods {
				if m != http.MethodHead { // HEAD is implied by GET
					inCode[m+" "+path] = true
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inSpec := specOperations(doc)

	var missingFromSpec, missingFromCode []string
	for op := range inCode {
		if !inSpec[op] {
			missingFromSpec = append(missingFromSpec, op)
		}
	}
	for op := range inSpec {
		if !inCode[op] {
			missingFromCode = append(missingFromCode, op)
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(missingFromCode)
	if len(missingFromSpec) > 0 {
		t.Errorf("routes in the code but NOT documented in docs/openapi.yaml:\n  %s", strings.Join(missingFromSpec, "\n  "))
	}
	if len(missingFromCode) > 0 {
		t.Errorf("operations documented in docs/openapi.yaml but NOT implemented:\n  %s", strings.Join(missingFromCode, "\n  "))
	}
	if len(inCode) < 20 {
		t.Fatalf("found only %d routes in code", len(inCode))
	}
}

// TestDocsAreServedOnlyWhenEnabled checks the toggle through the real wiring, and that the docs
// pages are not counted as API routes (they are not part of the contract).
func TestDocsAreServedOnlyWhenEnabled(t *testing.T) {
	build := func(enabled bool) http.Handler {
		cfg := testConfig("127.0.0.1:1")
		cfg.APIDocsEnabled = enabled
		a, err := app.New(app.Options{Config: cfg, Logger: quiet(), Mailer: &fakeMailer{}})
		if err != nil {
			t.Fatal(err)
		}
		return a.Handler
	}
	status := func(h http.Handler, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	on, off := build(true), build(false)
	for _, p := range []string{"/docs/", "/docs/openapi.yaml", "/docs/openapi.json", "/docs/swagger-ui-bundle.js"} {
		if got := status(on, p); got != 200 {
			t.Errorf("enabled: GET %s = %d", p, got)
		}
		if got := status(off, p); got != 404 {
			t.Errorf("disabled: GET %s = %d, want 404 (docs must be off unless enabled)", p, got)
		}
	}
	if got := status(off, "/docs"); got != 404 {
		t.Errorf("disabled: GET /docs = %d", got)
	}
}
