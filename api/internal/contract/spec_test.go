// Package contract checks that the HTTP handlers and docs/openapi.yaml describe the same API.
// It runs in CI on every build, so a handler cannot change without the spec following.
package contract

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"streamafrica/api/internal/app"
	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/config"
)

// specPath finds docs/openapi.yaml. In the dev container only ./api is mounted next to /docs, so
// OPENAPI_SPEC points there.
func specPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("OPENAPI_SPEC"); p != "" {
		return p
	}
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "openapi.yaml")
}

func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specPath(t))
	if err != nil {
		t.Fatalf("cannot load the spec (set OPENAPI_SPEC if docs/ is not next to api/): %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("docs/openapi.yaml is not a valid OpenAPI 3 document: %v", err)
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
		CatalogHomeTTL: time.Minute, CatalogGenresTTL: time.Minute,
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
