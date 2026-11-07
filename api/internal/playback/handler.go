package playback

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/ratelimit"
)

var playbackRule = ratelimit.Rule{Name: "playback-user", Limit: 120, Window: time.Minute}

// Middleware wraps a handler.
type Middleware = func(http.Handler) http.Handler

// Handler exposes GET /v1/playback/{playableId}.
type Handler struct {
	svc      *Service
	required Middleware
	limiter  ratelimit.Store
	logger   *slog.Logger
}

// NewHandler builds the handler. required is the authentication middleware.
func NewHandler(svc *Service, required Middleware, limiter ratelimit.Store, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, required: required, limiter: limiter, logger: logger}
}

// Mount registers the routes.
func (h *Handler) Mount(mux httpx.Mux) {
	userKey := func(r *http.Request) string {
		p, _ := auth.PrincipalFrom(r.Context())
		return p.UserID
	}
	limited := ratelimit.Middleware(h.limiter, h.logger, playbackRule, userKey)(http.HandlerFunc(h.get))
	httpx.Route(mux, "/v1/playback/{playableId}", h.required(limited), http.MethodGet)
}

type subtitleJSON struct {
	Language string `json:"language"`
	Label    string `json:"label"`
	URL      string `json:"url"`
}

type playbackJSON struct {
	PlayableID      string         `json:"playable_id"`
	AssetID         string         `json:"asset_id"`
	ManifestURL     string         `json:"manifest_url"`
	ExpiresAt       time.Time      `json:"expires_at"`
	DurationSeconds int            `json:"duration_seconds"`
	Subtitles       []subtitleJSON `json:"subtitles"`
	PosterURL       string         `json:"poster_url"`
	ThumbnailsURL   string         `json:"thumbnails_url"`
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	pb, err := h.svc.Playback(r.Context(), Viewer{UserID: p.UserID, Admin: p.Role == auth.RoleAdmin}, r.PathValue("playableId"))
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, "not_playable", "this video is not available")
		return
	case errors.Is(err, ErrNotEntitled):
		httpx.WriteError(w, r, http.StatusForbidden, "not_entitled", "your account cannot watch this video")
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "playback failed", "error", err.Error())
		httpx.WriteError(w, r, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	out := playbackJSON{PlayableID: pb.PlayableID, AssetID: pb.AssetID, ManifestURL: pb.ManifestURL, ExpiresAt: pb.ExpiresAt,
		DurationSeconds: pb.DurationSeconds, PosterURL: pb.PosterURL, ThumbnailsURL: pb.ThumbnailsURL, Subtitles: make([]subtitleJSON, 0, len(pb.Subtitles))}
	for _, s := range pb.Subtitles {
		out.Subtitles = append(out.Subtitles, subtitleJSON(s))
	}
	w.Header().Set("Cache-Control", "no-store") // signed URLs must never be cached or shared
	httpx.WriteJSON(w, http.StatusOK, out)
}
