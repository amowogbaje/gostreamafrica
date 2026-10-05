// Package server assembles the HTTP handler: routes plus middleware.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"streamafrica/api/internal/platform/health"
	"streamafrica/api/internal/platform/httpx"
)

// Deps are the collaborators the handler needs. Modules will register routes here later.
type Deps struct {
	Logger       *slog.Logger
	Dependencies []health.Dependency
}

// NewHandler builds the root handler.
func NewHandler(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", httpx.AllowMethods(health.Live(), http.MethodGet, http.MethodHead))
	mux.Handle("/readyz", httpx.AllowMethods(
		health.Ready(d.Logger, 2*time.Second, d.Dependencies), http.MethodGet, http.MethodHead))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, http.StatusNotFound, "not_found", "route not found")
	})

	return httpx.Chain(mux, httpx.RequestID, httpx.AccessLog(d.Logger), httpx.Recover(d.Logger))
}
