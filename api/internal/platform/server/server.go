// Package server assembles the HTTP handler: routes plus middleware.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"streamafrica/api/internal/platform/health"
	"streamafrica/api/internal/platform/httpx"
)

// Deps are the collaborators the handler needs.
type Deps struct {
	Logger       *slog.Logger
	Dependencies []health.Dependency
	// Mount lets each module register its own routes.
	Mount []func(httpx.Mux)
	// OnRoute, when set, is told about every route registered with httpx.Route.
	OnRoute func(path string, methods []string)
}

type mux struct {
	*http.ServeMux
	onRoute func(path string, methods []string)
}

func (m mux) Record(path string, methods []string) {
	if m.onRoute != nil {
		m.onRoute(path, methods)
	}
}

// NewHandler builds the root handler.
func NewHandler(d Deps) http.Handler {
	m := mux{ServeMux: http.NewServeMux(), onRoute: d.OnRoute}
	httpx.Route(m, "/healthz", health.Live(), http.MethodGet, http.MethodHead)
	httpx.Route(m, "/readyz", health.Ready(d.Logger, 2*time.Second, d.Dependencies), http.MethodGet, http.MethodHead)
	for _, mount := range d.Mount {
		mount(m)
	}
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, http.StatusNotFound, "not_found", "route not found")
	})
	return httpx.Chain(m, httpx.RequestID, httpx.AccessLog(d.Logger), httpx.Recover(d.Logger))
}
