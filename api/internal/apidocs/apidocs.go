// Package apidocs serves the API documentation from the API itself: an interactive Swagger UI at
// /docs/ and the OpenAPI spec at /docs/openapi.yaml and /docs/openapi.json. Everything is embedded
// in the binary, so it works offline and needs no extra container. Through nginx the same paths
// are /api/docs/...
//
// The spec embedded here is a copy of docs/openapi.yaml. A test fails when the two differ; run
// `make docs-sync` to refresh the copy after editing docs/openapi.yaml.
package apidocs

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"streamafrica/api/internal/platform/httpx"
)

// Spec is the OpenAPI document served at /docs/openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte

//go:embed assets
var assetFS embed.FS

// securityHeaders lock the documentation page down: scripts only from this origin (the page has
// no inline script), calls only to this origin, no framing.
var securityHeaders = map[string]string{
	"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; " +
		"base-uri 'none'; form-action 'none'",
	"X-Content-Type-Options": "nosniff",
	"Referrer-Policy":        "no-referrer",
	"X-Frame-Options":        "DENY",
}

const (
	cacheShort  = "no-cache"              // revalidate with the ETag
	cacheAssets = "public, max-age=86400" // pinned third-party files change only with a release
	htmlType    = "text/html; charset=utf-8"
	jsType      = "text/javascript; charset=utf-8"
	cssType     = "text/css; charset=utf-8"
	yamlType    = "application/yaml; charset=utf-8"
	jsonType    = "application/json; charset=utf-8"
	textType    = "text/plain; charset=utf-8"
)

type file struct {
	raw, gz      []byte // gz may be nil
	contentType  string
	cacheControl string
	etag         string
}

// Handler serves the documentation files.
type Handler struct {
	files map[string]file
}

// New loads the embedded files and converts the spec to JSON once.
func New() (*Handler, error) {
	h := &Handler{files: map[string]file{}}

	add := func(name string, raw []byte, ctype, cache string, gz []byte) {
		sum := sha256.Sum256(raw)
		if gz == nil && len(raw) > 512 {
			gz = compress(raw)
		}
		h.files[name] = file{raw: raw, gz: gz, contentType: ctype, cacheControl: cache,
			etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
	}
	read := func(name string) ([]byte, error) { return assetFS.ReadFile("assets/" + name) }
	pregzipped := func(name string) (raw, gz []byte, err error) {
		if gz, err = read(name + ".gz"); err != nil {
			return nil, nil, err
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			return nil, nil, err
		}
		raw, err = io.ReadAll(zr)
		return raw, gz, err
	}

	for name, ctype := range map[string]string{"swagger-ui-bundle.js": jsType, "swagger-ui.css": cssType} {
		raw, gz, err := pregzipped(name)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", name, err)
		}
		add(name, raw, ctype, cacheAssets, gz)
	}
	for name, ctype := range map[string]string{"index.html": htmlType, "init.js": jsType} {
		raw, err := read(name)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", name, err)
		}
		add(name, raw, ctype, cacheShort, nil)
	}
	var licenses bytes.Buffer
	licenses.WriteString("The API documentation page embeds Swagger UI (swagger-ui-dist), licensed under Apache-2.0.\n\n")
	for _, name := range []string{"LICENSE-swagger-ui.txt", "NOTICE-swagger-ui.txt"} {
		b, err := read(name)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", name, err)
		}
		licenses.Write(b)
		licenses.WriteString("\n\n")
	}
	add("licenses.txt", licenses.Bytes(), textType, cacheAssets, nil)

	asJSON, err := specJSON(Spec)
	if err != nil {
		return nil, fmt.Errorf("convert spec to JSON: %w", err)
	}
	add("openapi.yaml", Spec, yamlType, cacheShort, nil)
	add("openapi.json", asJSON, jsonType, cacheShort, nil)
	return h, nil
}

// Mount registers /docs and /docs/ . They are deliberately NOT part of the documented API, so they
// use Handle rather than httpx.Route and are not compared with the spec.
func (h *Handler) Mount(mux httpx.Mux) {
	mux.Handle("/docs/", h)
	mux.Handle("/docs", http.HandlerFunc(redirectToSlash))
}

// redirectToSlash sends /docs to docs/ . The relative Location keeps any proxy prefix: a browser at
// /api/docs lands on /api/docs/ .
func redirectToSlash(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", "docs/")
	w.WriteHeader(http.StatusFound)
}

// ServeHTTP serves one documentation file.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for k, v := range securityHeaders {
		w.Header().Set(k, v)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/docs/")
	if name == "" {
		name = "index.html"
	}
	f, ok := h.files[name]
	if !ok {
		httpx.WriteError(w, r, http.StatusNotFound, "not_found", "documentation file not found")
		return
	}

	body, etag := f.raw, f.etag
	hd := w.Header()
	hd.Add("Vary", "Accept-Encoding")
	if f.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body = f.gz
		etag = strings.TrimSuffix(f.etag, `"`) + `-gzip"` // a different representation needs a different ETag
		hd.Set("Content-Encoding", "gzip")
	}
	hd.Set("ETag", etag)
	hd.Set("Cache-Control", f.cacheControl)
	if matches(r.Header.Get("If-None-Match"), etag) {
		hd.Del("Content-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hd.Set("Content-Type", f.contentType)
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func matches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		if p := strings.TrimSpace(part); p == "*" || strings.TrimPrefix(p, "W/") == etag {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether the client accepts gzip (and did not turn it off with q=0).
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			for _, p := range fields[1:] {
				if q := strings.TrimSpace(p); strings.HasPrefix(q, "q=") {
					v, err := strconv.ParseFloat(strings.TrimPrefix(q, "q="), 64)
					return err == nil && v > 0
				}
			}
			return true
		}
	}
	return false
}

func compress(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// specJSON converts the YAML spec to JSON for Swagger UI.
func specJSON(spec []byte) ([]byte, error) {
	var doc any
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, err
	}
	return json.Marshal(normalize(doc))
}

// normalize turns the maps YAML can produce (with non-string keys) into maps JSON can encode.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = normalize(val)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	default:
		return v
	}
}
