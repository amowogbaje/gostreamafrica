package catalog

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/ratelimit"
)

const maxBodyBytes = 4 << 10

// progressRule throttles progress beacons per user: players report every 10 to 15 seconds, so 30 a
// minute leaves room for several devices while stopping a runaway client.
var progressRule = ratelimit.Rule{Name: "progress-user", Limit: 30, Window: time.Minute}

// Middleware wraps a handler.
type Middleware = func(http.Handler) http.Handler

// AuthMiddleware are the authentication middlewares the catalog routes need.
type AuthMiddleware struct {
	Required Middleware // 401 without a valid access token
	Optional Middleware // anonymous allowed; a present token must be valid
}

// Handler exposes the catalog HTTP endpoints.
type Handler struct {
	svc     *Service
	auth    AuthMiddleware
	limiter ratelimit.Store
	logger  *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, a AuthMiddleware, limiter ratelimit.Store, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, auth: a, limiter: limiter, logger: logger}
}

// Mount registers the catalog routes.
func (h *Handler) Mount(mux httpx.Mux) {
	fn := func(f http.HandlerFunc) http.Handler { return f }
	userKey := func(r *http.Request) string {
		p, _ := auth.PrincipalFrom(r.Context())
		return p.UserID
	}
	progressLimit := ratelimit.Middleware(h.limiter, h.logger, progressRule, userKey)

	httpx.Route(mux, "/v1/titles", h.auth.Optional(fn(h.listTitles)), http.MethodGet)
	httpx.Route(mux, "/v1/titles/{slug}", h.auth.Optional(fn(h.getTitle)), http.MethodGet)
	httpx.Route(mux, "/v1/genres", fn(h.genres), http.MethodGet)
	httpx.Route(mux, "/v1/home", h.auth.Optional(fn(h.home)), http.MethodGet)
	httpx.Route(mux, "/v1/me/watchlist", h.auth.Required(fn(h.watchlist)), http.MethodGet, http.MethodPost)
	httpx.Route(mux, "/v1/me/watchlist/{title_id}", h.auth.Required(fn(h.removeFromWatchlist)), http.MethodDelete)
	httpx.Route(mux, "/v1/me/progress", h.auth.Required(progressLimit(fn(h.putProgress))), http.MethodPut)
	httpx.Route(mux, "/v1/me/continue-watching", h.auth.Required(fn(h.continueWatching)), http.MethodGet)
}

func viewerOf(r *http.Request) Viewer {
	if p, ok := auth.PrincipalFrom(r.Context()); ok {
		return Viewer{UserID: p.UserID, Role: p.Role}
	}
	return Viewer{}
}

// shareable returns the public Cache-Control for anonymous requests and a private one for signed-in
// viewers, whose view of the catalog can differ (creators see their own drafts).
func shareable(v Viewer, public string) string {
	if v.Authenticated() {
		return ccPrivate
	}
	return public
}

func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.logger.ErrorContext(r.Context(), "catalog operation failed",
		"op", op, "request_id", httpx.RequestIDFrom(r.Context()), "error", err.Error())
	httpx.WriteError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
}

func notFound(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusNotFound, "title_not_found", "title not found")
}

func (h *Handler) listTitles(w http.ResponseWriter, r *http.Request) {
	p, fields, err := parseListQuery(r.URL.Query())
	switch {
	case len(fields) > 0:
		httpx.WriteValidationError(w, r, fields)
		return
	case errors.Is(err, ErrInvalidCursor):
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_cursor", "the cursor is not valid for this request")
		return
	}
	v := viewerOf(r)
	page, err := h.svc.List(r.Context(), v, p)
	if err != nil {
		h.internalError(w, r, "list-titles", err)
		return
	}
	body, err := marshal(page)
	if err != nil {
		h.internalError(w, r, "list-titles", err)
		return
	}
	writeBody(w, r, body, shareable(v, ccList))
}

func (h *Handler) getTitle(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if len(slug) > 120 || !slugRE.MatchString(slug) {
		notFound(w, r)
		return
	}
	v := viewerOf(r)
	d, err := h.svc.Detail(r.Context(), v, slug)
	switch {
	case errors.Is(err, ErrNotFound):
		notFound(w, r)
		return
	case err != nil:
		h.internalError(w, r, "get-title", err)
		return
	}
	body, err := marshal(d)
	if err != nil {
		h.internalError(w, r, "get-title", err)
		return
	}
	writeBody(w, r, body, shareable(v, ccDetail))
}

func (h *Handler) genres(w http.ResponseWriter, r *http.Request) {
	body, err := h.svc.GenresJSON(r.Context())
	if err != nil {
		h.internalError(w, r, "genres", err)
		return
	}
	writeBody(w, r, body, ccGenres)
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	v := viewerOf(r)
	body, _, err := h.svc.HomeJSON(r.Context(), v)
	if err != nil {
		h.internalError(w, r, "home", err)
		return
	}
	writeBody(w, r, body, shareable(v, ccHome))
}

type watchlistAddRequest struct {
	TitleID string `json:"title_id"`
}

type watchlistAddResponse struct {
	TitleID string    `json:"title_id"`
	AddedAt time.Time `json:"added_at"`
}

func (h *Handler) watchlist(w http.ResponseWriter, r *http.Request) {
	v := viewerOf(r)
	if r.Method == http.MethodPost {
		var req watchlistAddRequest
		if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
			return
		}
		if !uuidRE.MatchString(req.TitleID) {
			httpx.WriteValidationError(w, r, map[string]string{"title_id": "must be a title id"})
			return
		}
		at, created, err := h.svc.WatchlistAdd(r.Context(), v, req.TitleID)
		switch {
		case errors.Is(err, ErrNotFound):
			notFound(w, r)
		case errors.Is(err, ErrWatchlistFull):
			httpx.WriteError(w, r, http.StatusConflict, "watchlist_full", "your watchlist is full")
		case err != nil:
			h.internalError(w, r, "watchlist-add", err)
		default:
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			w.Header().Set("Cache-Control", ccNoStore)
			httpx.WriteJSON(w, status, watchlistAddResponse{TitleID: req.TitleID, AddedAt: at})
		}
		return
	}

	fields := map[string]string{}
	limit := parseLimit(r.URL.Query(), fields)
	if len(fields) > 0 {
		httpx.WriteValidationError(w, r, fields)
		return
	}
	after, err := parseCursor(r.URL.Query().Get("cursor"), "watchlist", "")
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, "invalid_cursor", "the cursor is not valid for this request")
		return
	}
	page, err := h.svc.Watchlist(r.Context(), v, after, limit)
	if err != nil {
		h.internalError(w, r, "watchlist", err)
		return
	}
	w.Header().Set("Cache-Control", ccNoStore)
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (h *Handler) removeFromWatchlist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("title_id")
	if !uuidRE.MatchString(id) {
		httpx.WriteValidationError(w, r, map[string]string{"title_id": "must be a title id"})
		return
	}
	if err := h.svc.WatchlistRemove(r.Context(), viewerOf(r), id); err != nil {
		h.internalError(w, r, "watchlist-remove", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type progressRequest struct {
	TitleID         string   `json:"title_id"`
	EpisodeID       *string  `json:"episode_id"`
	PositionSeconds *float64 `json:"position_seconds"`
	DurationSeconds *float64 `json:"duration_seconds"`
}

const maxDurationSeconds = 48 * 3600

func (h *Handler) putProgress(w http.ResponseWriter, r *http.Request) {
	var req progressRequest
	if !httpx.DecodeJSON(w, r, &req, maxBodyBytes) {
		return
	}
	fields := map[string]string{}
	if !uuidRE.MatchString(req.TitleID) {
		fields["title_id"] = "must be a title id"
	}
	if req.EpisodeID != nil && !uuidRE.MatchString(*req.EpisodeID) {
		fields["episode_id"] = "must be an episode id"
	}
	finite := func(f *float64) bool { return f != nil && !math.IsNaN(*f) && !math.IsInf(*f, 0) }
	if !finite(req.PositionSeconds) || *req.PositionSeconds < 0 {
		fields["position_seconds"] = "must be a number of seconds, 0 or more"
	}
	if !finite(req.DurationSeconds) || *req.DurationSeconds < 1 || *req.DurationSeconds > maxDurationSeconds {
		fields["duration_seconds"] = "must be a number of seconds from 1 to 172800"
	}
	if len(fields) > 0 {
		httpx.WriteValidationError(w, r, fields)
		return
	}
	prog, err := h.svc.SaveProgress(r.Context(), viewerOf(r), ProgressUpdate{
		TitleID:         req.TitleID,
		EpisodeID:       req.EpisodeID,
		PositionSeconds: int(*req.PositionSeconds),
		DurationSeconds: int(*req.DurationSeconds),
	})
	switch {
	case errors.Is(err, ErrNotFound):
		notFound(w, r)
	case errors.Is(err, ErrInvalidProgress):
		httpx.WriteValidationError(w, r, map[string]string{
			"episode_id": "required for a series, not allowed for a movie, and must belong to the title"})
	case err != nil:
		h.internalError(w, r, "put-progress", err)
	default:
		w.Header().Set("Cache-Control", ccNoStore)
		httpx.WriteJSON(w, http.StatusOK, prog)
	}
}

func (h *Handler) continueWatching(w http.ResponseWriter, r *http.Request) {
	fields := map[string]string{}
	limit := parseLimit(r.URL.Query(), fields)
	if len(fields) > 0 {
		httpx.WriteValidationError(w, r, fields)
		return
	}
	list, err := h.svc.ContinueWatching(r.Context(), viewerOf(r), limit)
	if err != nil {
		h.internalError(w, r, "continue-watching", err)
		return
	}
	w.Header().Set("Cache-Control", ccNoStore)
	httpx.WriteJSON(w, http.StatusOK, list)
}
