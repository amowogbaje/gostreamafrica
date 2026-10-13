package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// etagFor returns a strong ETag for a response body.
func etagFor(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches implements If-None-Match for GET (weak comparison, "*" and lists allowed).
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

// writeBody sends a 200 JSON body with an ETag and Cache-Control, or a 304 when the client
// already has this exact body. Authorization is listed in Vary because the response differs for
// signed-in viewers (creators see their own drafts).
func writeBody(w http.ResponseWriter, r *http.Request, body []byte, cacheControl string) {
	etag := etagFor(body)
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", cacheControl)
	h.Add("Vary", "Authorization")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// body may be shared with other in-flight requests (singleflight), so it must never be
	// modified or appended to: write the trailing newline separately.
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

func marshal(v any) ([]byte, error) { return json.Marshal(v) }

// Cache-Control values. Anonymous responses may be shared by browsers and the CDN; anything
// that depends on who is asking must stay private.
const (
	ccGenres  = "public, max-age=300, stale-while-revalidate=600"
	ccHome    = "public, max-age=60, stale-while-revalidate=120"
	ccList    = "public, max-age=30, stale-while-revalidate=60"
	ccDetail  = "public, max-age=60, stale-while-revalidate=120"
	ccPrivate = "private, no-cache"
	ccNoStore = "private, no-store"
)
