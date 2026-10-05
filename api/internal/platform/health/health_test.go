package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"streamafrica/api/internal/platform/health"
)

func logger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func ok(context.Context) error   { return nil }
func fail(context.Context) error { return errors.New("secret internal detail") }

func dep(name string, check func(context.Context) error) health.Dependency {
	return health.Dependency{Name: name, Check: check}
}

func TestReady_AllOK(t *testing.T) {
	h := health.Ready(logger(), time.Second, []health.Dependency{dep("postgres", ok), dep("redis", ok)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var rep health.Report
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if rep.Status != "ok" || rep.Checks["postgres"] != "ok" {
		t.Fatalf("bad report: %+v", rep)
	}
}

func TestReady_Degraded(t *testing.T) {
	h := health.Ready(logger(), time.Second, []health.Dependency{dep("postgres", ok), dep("redis", fail)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "unreachable") || strings.Contains(body, "secret") {
		t.Fatalf("body=%s", body)
	}
}

func TestLive(t *testing.T) {
	rec := httptest.NewRecorder()
	health.Live().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := health.TCP(addr)(context.Background()); err != nil {
		t.Fatalf("expected reachable: %v", err)
	}
	_ = ln.Close()
	if err := health.TCP(addr)(context.Background()); err == nil {
		t.Fatal("expected unreachable after close")
	}
}
