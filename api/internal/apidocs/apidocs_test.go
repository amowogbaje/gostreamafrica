package apidocs

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"streamafrica/api/internal/platform/httpx"
)

// SHA-256 of the unmodified files from npm package swagger-ui-dist 5.33.0 (published 2026-09-16).
// If you upgrade Swagger UI, replace the files in assets/ AND these two values together.
const (
	bundleSHA = "62df541529080464a7660adc793eab7128c6193ce3be24ddc1e0e0a4a63edc2f"
	cssSHA    = "1ac324f7dcd27e4b9386b4bd6421271ec147e922a22c05ba24b11515e9aa6321"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux
}

func get(h http.Handler, method, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVendoredAssetsAreTheExactUpstreamFiles(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if got := sum(h.files["swagger-ui-bundle.js"].raw); got != bundleSHA {
		t.Errorf("swagger-ui-bundle.js was changed or replaced: sha256 %s", got)
	}
	if got := sum(h.files["swagger-ui.css"].raw); got != cssSHA {
		t.Errorf("swagger-ui.css was changed or replaced: sha256 %s", got)
	}
}

func TestIndexPage(t *testing.T) {
	h := newHandler(t)
	rec := get(h, "GET", "/docs/")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != htmlType {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{`href="swagger-ui.css"`, `src="swagger-ui-bundle.js"`, `src="init.js"`, `id="swagger-ui"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	// the page must have NO inline script, so a strict CSP can forbid them
	if regexp.MustCompile(`<script(?:\s[^>]*)?>`).FindAllString(body, -1) == nil {
		t.Fatal("no script tags at all")
	}
	for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("inline script found: %s", tag)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(strings.SplitN(csp, "style-src", 2)[0], "unsafe-inline") {
		t.Errorf("CSP too permissive: %s", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" ||
		rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("security headers missing: %v", rec.Header())
	}
	if rec.Header().Get("Cache-Control") != cacheShort {
		t.Errorf("the page must revalidate: %q", rec.Header().Get("Cache-Control"))
	}
}

func TestSpecEndpoints(t *testing.T) {
	h := newHandler(t)
	y := get(h, "GET", "/docs/openapi.yaml")
	if y.Code != 200 || !bytes.Equal(y.Body.Bytes(), Spec) || y.Header().Get("Content-Type") != yamlType {
		t.Fatalf("yaml: %d %q", y.Code, y.Header().Get("Content-Type"))
	}
	j := get(h, "GET", "/docs/openapi.json")
	if j.Code != 200 || j.Header().Get("Content-Type") != jsonType {
		t.Fatalf("json: %d", j.Code)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct{ Title, Version string }
		Paths   map[string]map[string]any `json:"paths"`
		Comp    struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(j.Body.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if doc.OpenAPI != "3.0.3" || doc.Info.Title != "StreamAfrica API" || len(doc.Paths) != 24 || doc.Comp.Schemas["TitleDetail"] == nil {
		t.Fatalf("converted spec looks wrong: openapi=%q title=%q paths=%d", doc.OpenAPI, doc.Info.Title, len(doc.Paths))
	}
	ops := 0
	for _, item := range doc.Paths {
		for key := range item {
			if key != "parameters" { // path-level parameters are not operations
				ops++
			}
		}
	}
	if ops != 27 {
		t.Fatalf("operations = %d, want 27", ops)
	}
	// response codes must survive as strings ("200"), which Swagger UI requires
	var raw map[string]any
	_ = json.Unmarshal(j.Body.Bytes(), &raw)
	responses := raw["paths"].(map[string]any)["/v1/titles"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
	if responses["200"] == nil || responses["304"] == nil {
		t.Fatalf("response codes lost in conversion: %v", responses)
	}
}

func TestStaticAssetsAndCompression(t *testing.T) {
	h := newHandler(t)
	for _, c := range []struct{ path, ctype, sha string }{
		{"/docs/swagger-ui-bundle.js", jsType, bundleSHA},
		{"/docs/swagger-ui.css", cssType, cssSHA},
	} {
		gz := get(h, "GET", c.path, "Accept-Encoding", "gzip, deflate, br")
		if gz.Code != 200 || gz.Header().Get("Content-Encoding") != "gzip" || gz.Header().Get("Content-Type") != c.ctype ||
			!strings.Contains(gz.Header().Get("Vary"), "Accept-Encoding") || gz.Header().Get("Cache-Control") != cacheAssets {
			t.Fatalf("%s gzip: %d %v", c.path, gz.Code, gz.Header())
		}
		if sum(gunzip(t, gz.Body.Bytes())) != c.sha {
			t.Errorf("%s: gzip body does not decompress to the upstream file", c.path)
		}
		if gz.Body.Len() > 500_000 {
			t.Errorf("%s: %d bytes compressed is too heavy for mobile data", c.path, gz.Body.Len())
		}
		for _, hdr := range []string{"", "identity", "gzip;q=0", "br"} {
			plain := get(h, "GET", c.path, "Accept-Encoding", hdr)
			if plain.Header().Get("Content-Encoding") != "" || sum(plain.Body.Bytes()) != c.sha {
				t.Errorf("%s with Accept-Encoding %q must be served uncompressed", c.path, hdr)
			}
		}
	}
	// small files are compressed too; init.js is the script the page runs
	rec := get(h, "GET", "/docs/init.js", "Accept-Encoding", "gzip")
	if rec.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(string(gunzip(t, rec.Body.Bytes())), "openapi.json") {
		t.Fatalf("init.js: %v", rec.Header())
	}
	lic := get(h, "GET", "/docs/licenses.txt")
	if lic.Code != 200 || !strings.Contains(lic.Body.String(), "Apache License") || !strings.Contains(lic.Body.String(), "SmartBear") {
		t.Fatalf("the Swagger UI license text must ship with the binary: %d", lic.Code)
	}
}

func TestConditionalRequests(t *testing.T) {
	h := newHandler(t)
	for _, path := range []string{"/docs/openapi.json", "/docs/swagger-ui-bundle.js", "/docs/"} {
		for _, enc := range []string{"", "gzip"} {
			first := get(h, "GET", path, "Accept-Encoding", enc)
			etag := first.Header().Get("ETag")
			if etag == "" {
				t.Fatalf("%s: no ETag", path)
			}
			again := get(h, "GET", path, "Accept-Encoding", enc, "If-None-Match", etag)
			if again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("ETag") != etag {
				t.Errorf("%s (%q): %d body=%d", path, enc, again.Code, again.Body.Len())
			}
		}
	}
	// a client must not be told its gzip copy is current when it asks for the plain one
	plain := get(h, "GET", "/docs/swagger-ui.css").Header().Get("ETag")
	zipped := get(h, "GET", "/docs/swagger-ui.css", "Accept-Encoding", "gzip").Header().Get("ETag")
	if plain == zipped {
		t.Error("different encodings must have different ETags")
	}
}

func TestMethodsPathsAndRedirect(t *testing.T) {
	h := newHandler(t)
	if rec := get(h, "POST", "/docs/"); rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" || !strings.Contains(rec.Body.String(), "method_not_allowed") {
		t.Errorf("POST: %d %v", rec.Code, rec.Header())
	}
	for _, p := range []string{"/docs/nope", "/docs/assets/index.html", "/docs/../etc/passwd", "/docs/openapi.yaml/", "/docs/swagger-ui.css.gz", "/docs/.env", "/docs//index.html"} {
		rec := get(h, "GET", p)
		if rec.Code != 404 && rec.Code != 301 && rec.Code != 307 && rec.Code != 308 {
			t.Errorf("%s: %d (only the documented files may be reachable)", p, rec.Code)
		}
		if rec.Code == 404 && !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
			t.Errorf("%s: 404 must use the standard error format: %s", p, rec.Body.String())
		}
	}
	if rec := get(h, "GET", "/docs"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "docs/" {
		t.Errorf("/docs redirect: %d %q (a relative Location keeps the /api prefix behind nginx)", rec.Code, rec.Header().Get("Location"))
	}
	head := get(h, "HEAD", "/docs/openapi.json")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD: %d len=%d", head.Code, head.Body.Len())
	}
}

func TestRoutesAreNotPartOfTheDocumentedAPI(t *testing.T) {
	h, _ := New()
	var recorded []string
	m := recordingMux{http.NewServeMux(), &recorded}
	h.Mount(m)
	if len(recorded) != 0 {
		t.Fatalf("docs routes must not be registered as API routes: %v", recorded)
	}
}

type recordingMux struct {
	*http.ServeMux
	recorded *[]string
}

func (m recordingMux) Record(path string, _ []string) { *m.recorded = append(*m.recorded, path) }

var _ httpx.Mux = recordingMux{}

func TestEmbeddedSpecMatchesDocsFolder(t *testing.T) {
	path := os.Getenv("OPENAPI_SPEC")
	if path == "" {
		_, file, _, _ := runtime.Caller(0)
		path = filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "openapi.yaml")
	}
	docs, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("docs/openapi.yaml not available here (%v)", err)
	}
	if !bytes.Equal(docs, Spec) {
		t.Fatal("docs/openapi.yaml and api/internal/apidocs/openapi.yaml differ. Edit docs/openapi.yaml, then run: make docs-sync")
	}
}
