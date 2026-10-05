// Package health provides liveness and readiness handlers.
package health

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"streamafrica/api/internal/platform/httpx"
)

// Dependency is one thing readiness depends on.
type Dependency struct {
	Name  string
	Check func(ctx context.Context) error
}

// Report is the readiness response body.
type Report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// TCP returns a check that succeeds when addr accepts a TCP connection.
func TCP(addr string) func(context.Context) error {
	return func(ctx context.Context) error {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
}

// Live reports that the process is up. It checks nothing external.
func Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, Report{Status: "ok"})
	})
}

// Ready runs every dependency check concurrently under one timeout.
// Failure details go to the log, never to the client.
func Ready(logger *slog.Logger, timeout time.Duration, deps []Dependency) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		errs := make([]error, len(deps))
		var wg sync.WaitGroup
		for i, d := range deps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = d.Check(ctx)
			}()
		}
		wg.Wait()

		report := Report{Status: "ok", Checks: make(map[string]string, len(deps))}
		code := http.StatusOK
		for i, d := range deps {
			if errs[i] != nil {
				logger.WarnContext(ctx, "dependency check failed", "dependency", d.Name, "error", errs[i].Error())
				report.Checks[d.Name] = "unreachable"
				report.Status = "degraded"
				code = http.StatusServiceUnavailable
				continue
			}
			report.Checks[d.Name] = "ok"
		}
		httpx.WriteJSON(w, code, report)
	})
}
