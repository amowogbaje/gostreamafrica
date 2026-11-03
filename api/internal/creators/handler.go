package creators

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/httpx"
	"streamafrica/api/internal/platform/ratelimit"
)

const maxBodyBytes = 4 << 10

// Per-creator limits on top of the quotas in the service. Creating an upload is the expensive call
// (it opens a storage upload), so it is the tightest.
var (
	createRule   = ratelimit.Rule{Name: "uploads-create-user", Limit: 10, Window: time.Hour}
	partsRule    = ratelimit.Rule{Name: "uploads-parts-user", Limit: 120, Window: time.Minute}
	completeRule = ratelimit.Rule{Name: "uploads-complete-user", Limit: 30, Window: time.Hour}
)

// Middleware wraps a handler.
type Middleware = func(http.Handler) http.Handler

// Handler exposes the creator upload endpoints.
type Handler struct {
	svc      *Service
	required Middleware // 401 without a valid access token
	limiter  ratelimit.Store
	logger   *slog.Logger
}

// NewHandler builds the handler. required is the authentication middleware.
func NewHandler(svc *Service, required Middleware, limiter ratelimit.Store, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, required: required, limiter: limiter, logger: logger}
}

// Mount registers the routes. Every route requires a creator or an admin.
func (h *Handler) Mount(mux httpx.Mux) {
	creator := func(next http.Handler) http.Handler {
		return h.required(auth.RequireRole(auth.RoleCreator, auth.RoleAdmin)(next))
	}
	userKey := func(r *http.Request) string {
		p, _ := auth.PrincipalFrom(r.Context())
		return p.UserID
	}
	limit := func(rule ratelimit.Rule, f http.HandlerFunc) http.Handler {
		return ratelimit.Middleware(h.limiter, h.logger, rule, userKey)(f)
	}

	httpx.Route(mux, "/v1/creator/uploads", creator(limit(createRule, h.create)), http.MethodPost)
	httpx.Route(mux, "/v1/creator/uploads/{id}", creator(http.HandlerFunc(h.item)), http.MethodGet, http.MethodDelete)
	httpx.Route(mux, "/v1/creator/uploads/{id}/parts", creator(limit(partsRule, h.parts)), http.MethodPost)
	httpx.Route(mux, "/v1/creator/uploads/{id}/complete", creator(limit(completeRule, h.complete)), http.MethodPost)
}

// ---- JSON shapes

type jobJSON struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Stage   string `json:"stage,omitempty"`
	Percent *int   `json:"percent,omitempty"`
	Error   string `json:"error,omitempty"`
}

type storedPartJSON struct {
	PartNumber int   `json:"part_number"`
	SizeBytes  int64 `json:"size_bytes"`
}

type uploadJSON struct {
	ID            string            `json:"id"`
	TitleID       string            `json:"title_id"`
	Filename      string            `json:"filename"`
	ContentType   string            `json:"content_type"`
	SizeBytes     int64             `json:"size_bytes"`
	Status        string            `json:"status"`
	PartSizeBytes int64             `json:"part_size_bytes"`
	PartCount     int               `json:"part_count"`
	RejectCode    string            `json:"reject_code,omitempty"`
	RejectReason  string            `json:"reject_reason,omitempty"`
	ExpiresAt     time.Time         `json:"expires_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	Job           *jobJSON          `json:"job,omitempty"`
	StoredParts   *[]storedPartJSON `json:"stored_parts,omitempty"`
}

type partURLJSON struct {
	PartNumber int    `json:"part_number"`
	URL        string `json:"url"`
	SizeBytes  int64  `json:"size_bytes"`
}

type partsJSON struct {
	Parts        []partURLJSON `json:"parts"`
	URLsExpireAt time.Time     `json:"urls_expire_at"`
}

type createdJSON struct {
	Upload       uploadJSON    `json:"upload"`
	Parts        []partURLJSON `json:"parts"`
	URLsExpireAt time.Time     `json:"urls_expire_at"`
}

func jobView(j *Job) *jobJSON {
	if j == nil {
		return nil
	}
	return &jobJSON{ID: j.ID, State: j.State, Stage: j.Stage, Percent: j.Percent, Error: j.Error}
}

func uploadView(u Upload, job *Job) uploadJSON {
	return uploadJSON{
		ID: u.ID, TitleID: u.TitleID, Filename: u.Filename, ContentType: u.ContentType, SizeBytes: u.SizeBytes,
		Status: string(u.Status), PartSizeBytes: u.PartSizeBytes, PartCount: u.PartCount,
		RejectCode: u.RejectCode, RejectReason: u.RejectReason,
		ExpiresAt: u.ExpiresAt, CompletedAt: u.CompletedAt, CreatedAt: u.CreatedAt, Job: jobView(job),
	}
}

func partViews(in []PartURL) []partURLJSON {
	out := make([]partURLJSON, 0, len(in))
	for _, p := range in {
		out = append(out, partURLJSON{PartNumber: p.Number, URL: p.URL, SizeBytes: p.SizeBytes})
	}
	return out
}

// ---- handlers

// reply sends JSON that must never be cached: it contains presigned URLs and upload state.
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, status, v)
}

func (h *Handler) principal(r *http.Request) auth.Principal {
	p, _ := auth.PrincipalFrom(r.Context())
	return p
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TitleID     string `json:"title_id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		SizeBytes   int64  `json:"size_bytes"`
	}
	if !httpx.DecodeJSON(w, r, &body, maxBodyBytes) {
		return
	}
	res, err := h.svc.Create(r.Context(), h.principal(r), CreateInput{
		TitleID: body.TitleID, Filename: body.Filename, ContentType: body.ContentType, SizeBytes: body.SizeBytes})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	reply(w, http.StatusCreated, createdJSON{
		Upload: uploadView(res.Upload, nil), Parts: partViews(res.Parts), URLsExpireAt: res.URLsExpireAt})
}

// item serves GET (state, and stored parts while unfinished) and DELETE (cancel).
func (h *Handler) item(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.Method == http.MethodDelete {
		if err := h.svc.Abort(r.Context(), h.principal(r), id); err != nil {
			h.fail(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	d, err := h.svc.Get(r.Context(), h.principal(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	reply(w, http.StatusOK, detailsView(d))
}

func detailsView(d Details) uploadJSON {
	v := uploadView(d.Upload, d.Job)
	if d.Upload.Status == StatusInitiated {
		stored := make([]storedPartJSON, 0, len(d.StoredParts))
		for _, p := range d.StoredParts {
			stored = append(stored, storedPartJSON{PartNumber: p.Number, SizeBytes: p.Size})
		}
		v.StoredParts = &stored
	}
	return v
}

func (h *Handler) parts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PartNumbers []int `json:"part_numbers"`
	}
	if !httpx.DecodeJSON(w, r, &body, maxBodyBytes) {
		return
	}
	res, err := h.svc.PartURLs(r.Context(), h.principal(r), r.PathValue("id"), body.PartNumbers)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	reply(w, http.StatusOK, partsJSON{Parts: partViews(res.Parts), URLsExpireAt: res.URLsExpireAt})
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	d, err := h.svc.Complete(r.Context(), h.principal(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	reply(w, http.StatusAccepted, detailsView(d))
}

// fail answers an error from the service: expected problems as documented, storage trouble as 502,
// everything else as an opaque 500 (details go to the log only).
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var p *Problem
	var se *storageError
	switch {
	case errors.As(err, &p):
		if len(p.Fields) > 0 {
			httpx.WriteValidationError(w, r, p.Fields)
			return
		}
		httpx.WriteError(w, r, p.Status, p.Code, p.Message)
	case errors.As(err, &se):
		h.logger.ErrorContext(r.Context(), "object storage failed", "error", se.Error(), "path", r.URL.Path)
		httpx.WriteError(w, r, http.StatusBadGateway, "storage_unavailable", "file storage is unavailable right now; try again shortly")
	default:
		h.logger.ErrorContext(r.Context(), "upload request failed", "error", err.Error(), "path", r.URL.Path)
		httpx.WriteError(w, r, http.StatusInternalServerError, "internal_error", "something went wrong")
	}
}
