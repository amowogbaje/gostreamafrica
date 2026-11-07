package creators

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	retryRule    = ratelimit.Rule{Name: "uploads-retry-user", Limit: 10, Window: time.Hour}
	eventsRule   = ratelimit.Rule{Name: "uploads-events-user", Limit: 60, Window: time.Hour}
	assetRule    = ratelimit.Rule{Name: "assets-user", Limit: 30, Window: time.Hour}
)

// Middleware wraps a handler.
type Middleware = func(http.Handler) http.Handler

// Handler exposes the creator upload endpoints.
type Handler struct {
	svc       *Service
	required  Middleware // 401 without a valid access token
	limiter   ratelimit.Store
	logger    *slog.Logger
	pollEvery time.Duration // how often a live-progress stream looks for changes
}

// NewHandler builds the handler. required is the authentication middleware.
func NewHandler(svc *Service, required Middleware, limiter ratelimit.Store, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, required: required, limiter: limiter, logger: logger, pollEvery: time.Second}
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
	httpx.Route(mux, "/v1/creator/uploads/{id}/retry", creator(limit(retryRule, h.retry)), http.MethodPost)
	httpx.Route(mux, "/v1/creator/uploads/{id}/events", creator(limit(eventsRule, h.events)), http.MethodGet)
	httpx.Route(mux, "/v1/creator/assets/{id}", creator(limit(assetRule, h.deleteAsset)), http.MethodDelete)
	httpx.Route(mux, "/v1/creator/assets/{id}/unpublish", creator(limit(assetRule, h.unpublish)), http.MethodPost)
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

// retryingNote is all a creator is told about a failed attempt. The real error (FFmpeg output, hostnames)
// stays in the logs and the database.
const retryingNote = "Processing hit a temporary problem and will be retried automatically."

func jobView(j *Job) *jobJSON {
	if j == nil {
		return nil
	}
	v := &jobJSON{ID: j.ID, State: j.State, Stage: j.Stage, Percent: j.Percent}
	if j.State == "queued" && j.Attempts > 0 && j.Error != "" {
		v.Error = retryingNote
	}
	return v
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

func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	d, err := h.svc.Retry(r.Context(), h.principal(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	reply(w, http.StatusAccepted, detailsView(d))
}

type assetJSON struct {
	AssetID     string `json:"asset_id"`
	TitleID     string `json:"title_id"`
	TitleStatus string `json:"title_status"`
}

func (h *Handler) unpublish(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.UnpublishAsset(r.Context(), h.principal(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	reply(w, http.StatusOK, assetJSON{AssetID: a.ID, TitleID: a.TitleID, TitleStatus: a.TitleStatus})
}

func (h *Handler) deleteAsset(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteAsset(r.Context(), h.principal(r), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// maxStream bounds one live-progress connection; the client simply reconnects.
const maxStream = 30 * time.Minute

func terminal(s Status) bool {
	switch s {
	case StatusReady, StatusRejected, StatusAborted, StatusExpired, StatusDeleted:
		return true
	}
	return false
}

// events streams an upload's progress as Server-Sent Events: one `status` event now, another whenever
// something changes, and `end` when the upload reaches a final state. A comment line every 15 seconds
// keeps proxies from closing an idle connection. Clients that cannot use EventSource can poll GET instead.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	id, p := r.PathValue("id"), h.principal(r)
	d, err := h.svc.Snapshot(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // the server's write timeout must not cut a long stream
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache, no-store")
	hd.Set("X-Accel-Buffering", "no") // tell nginx not to hold the stream back
	w.WriteHeader(http.StatusOK)

	send := func(event string, payload []byte) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	var last []byte
	emit := func(d Details) (done, ok bool) {
		raw, err := json.Marshal(detailsView(d))
		if err != nil {
			return true, false
		}
		if string(raw) != string(last) {
			last = raw
			if !send("status", raw) {
				return true, false
			}
		}
		if terminal(d.Upload.Status) {
			return true, send("end", []byte(`{"reason":"`+string(d.Upload.Status)+`"}`))
		}
		return false, true
	}
	if done, _ := emit(d); done {
		return
	}
	poll := time.NewTicker(h.pollEvery)
	ping := time.NewTicker(15 * time.Second)
	limit := time.NewTimer(maxStream)
	defer poll.Stop()
	defer ping.Stop()
	defer limit.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-limit.C:
			send("end", []byte(`{"reason":"timeout"}`))
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-poll.C:
			d, err := h.svc.Snapshot(r.Context(), p, id)
			if err != nil {
				if r.Context().Err() == nil {
					h.logger.ErrorContext(r.Context(), "progress stream failed", "error", err.Error(), "upload_id", id)
					send("end", []byte(`{"reason":"error"}`))
				}
				return
			}
			if done, _ := emit(d); done {
				return
			}
		}
	}
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
