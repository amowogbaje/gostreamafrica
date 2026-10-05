package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"streamafrica/api/internal/platform/health"
	"streamafrica/api/internal/platform/server"
)

func newHandler(deps ...health.Dependency) http.Handler {
	return server.NewHandler(server.Deps{
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Dependencies: deps,
	})
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	rec := do(newHandler(), http.MethodGet, "/healthz")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

func TestReadyz_Degraded(t *testing.T) {
	down := health.Dependency{Name: "redis", Check: func(context.Context) error { return errors.New("x") }}
	if rec := do(newHandler(down), http.MethodGet, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestMethodNotAllowed_And_NotFound_UseErrorFormat(t *testing.T) {
	h := newHandler()
	rec := do(h, http.MethodPost, "/healthz")
	if rec.Code != 405 || !strings.Contains(rec.Body.String(), `"code":"method_not_allowed"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/nope")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
