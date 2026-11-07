package creators

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/s3"
)

// ObjectStore is the part of the S3 client the service uses (satisfied by *s3.Client).
type ObjectStore interface {
	CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (string, error)
	PresignUploadPart(bucket, key, uploadID string, partNumber int, contentLength int64, ttl time.Duration) string
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]s3.Part, error)
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []s3.Part) error
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	ListMultipartUploads(ctx context.Context, bucket, prefix string) ([]s3.MultipartUpload, error)
	Head(ctx context.Context, bucket, key string) (s3.ObjectInfo, error)
	GetRange(ctx context.Context, bucket, key string, offset, length int64) ([]byte, error)
	Delete(ctx context.Context, bucket, key string) error
	DeletePrefix(ctx context.Context, bucket, prefix string) (int, error)
}

// Config holds the upload limits and storage names.
type Config struct {
	RawBucket    string
	HLSBucket    string        // published video (private)
	PublicBucket string        // posters and thumbnails
	MaxBytes     int64         // largest accepted file
	PartBytes    int64         // part size handed to clients
	URLTTL       time.Duration // lifetime of one presigned part URL
	AbandonAfter time.Duration // an unfinished upload is aborted after this long
	MaxActive    int           // unfinished uploads per creator
	DailyBytes   int64         // bytes a creator may start uploading per rolling 24 h
	InitialParts int           // part URLs returned when an upload starts (default 4)
	Policy       Policy        // snapshot copied into every transcode job
}

// MaxPartsPerRequest caps how many part URLs one call may ask for.
const MaxPartsPerRequest = 20

// Problem is an expected failure with the HTTP answer to give. Anything else is an internal error.
type Problem struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]string
}

func (p *Problem) Error() string { return p.Code + ": " + p.Message }

func problem(status int, code, msg string) *Problem {
	return &Problem{Status: status, Code: code, Message: msg}
}

var (
	errUploadNotFound = problem(http.StatusNotFound, "upload_not_found", "upload not found")
	errTitleNotFound  = problem(http.StatusNotFound, "title_not_found", "title not found")
)

// Service implements the upload flow.
type Service struct {
	store   Store
	objects ObjectStore
	scanner Scanner
	logger  *slog.Logger
	now     func() time.Time
	cfg     Config
}

// NewService builds the service. scanner may be nil (no scanning).
func NewService(store Store, objects ObjectStore, scanner Scanner, logger *slog.Logger, now func() time.Time, cfg Config) *Service {
	if scanner == nil {
		scanner = NoopScanner{}
	}
	if now == nil {
		now = time.Now
	}
	if cfg.InitialParts <= 0 {
		cfg.InitialParts = 4
	}
	return &Service{store: store, objects: objects, scanner: scanner, logger: logger, now: now, cfg: cfg}
}

// ---- requests and results

// CreateInput is the body of POST /v1/creator/uploads.
type CreateInput struct {
	TitleID     string
	Filename    string
	ContentType string
	SizeBytes   int64
}

// PartURL is a presigned URL for one part. The client must PUT exactly SizeBytes bytes.
type PartURL struct {
	Number    int
	URL       string
	SizeBytes int64
}

// CreateResult is returned when an upload starts.
type CreateResult struct {
	Upload        Upload
	Parts         []PartURL
	URLsExpireAt  time.Time
	MaxPartsAsked int
}

// PartsResult is returned when more part URLs are requested.
type PartsResult struct {
	Parts        []PartURL
	URLsExpireAt time.Time
}

// Details is an upload with its job and, while unfinished, the parts already stored.
type Details struct {
	Upload      Upload
	Job         *Job
	StoredParts []s3.Part // only while the upload is initiated
}

var (
	uuidRE        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	octetStream   = "application/octet-stream"
	allowedTypeOf = map[string]string{} // content type -> container extension
)

func init() {
	for ext, ct := range containerTypes {
		allowedTypeOf[ct] = ext
	}
	allowedTypeOf["video/x-m4v"] = "mp4"
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ---- validation

// validateCreate checks the request body and returns the container extension.
func (s *Service) validateCreate(in CreateInput) (container string, fields map[string]string) {
	fields = map[string]string{}
	if !uuidRE.MatchString(in.TitleID) {
		fields["title_id"] = "must be a title id (UUID)"
	}
	name := strings.TrimSpace(in.Filename)
	switch {
	case name == "":
		fields["filename"] = "is required"
	case len(name) > 255:
		fields["filename"] = "must be at most 255 characters"
	case strings.ContainsAny(name, `/\`) || strings.IndexFunc(name, unicode.IsControl) >= 0:
		fields["filename"] = "must be a plain file name"
	default:
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
		if _, ok := containerTypes[ext]; !ok {
			fields["filename"] = "unsupported file type: use mp4, mov, mkv or webm"
		} else {
			container = ext
		}
	}
	if in.SizeBytes <= 0 {
		fields["size_bytes"] = "must be greater than 0"
	} else if in.SizeBytes > s.cfg.MaxBytes {
		fields["size_bytes"] = fmt.Sprintf("must be at most %d bytes (%s)", s.cfg.MaxBytes, humanBytes(s.cfg.MaxBytes))
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(in.ContentType, ";", 2)[0]))
	if ct != "" && ct != octetStream {
		ext, ok := allowedTypeOf[ct]
		switch {
		case !ok:
			fields["content_type"] = "must be a video/mp4, video/quicktime, video/x-matroska or video/webm file"
		case container != "" && containerFamily(ext) != containerFamily(container):
			fields["content_type"] = "does not match the file extension"
		}
	}
	return container, fields
}

func humanBytes(n int64) string {
	const gib = 1 << 30
	if n >= gib && n%gib == 0 {
		return fmt.Sprintf("%d GiB", n/gib)
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/gib)
}

func canAccess(p auth.Principal, ownerID string) bool {
	return p.Role == auth.RoleAdmin || (ownerID != "" && p.UserID == ownerID)
}

// getOwned loads an upload the caller may use. Someone else's upload looks exactly like a missing one.
func (s *Service) getOwned(ctx context.Context, p auth.Principal, id string) (Upload, error) {
	if !uuidRE.MatchString(id) {
		return Upload{}, errUploadNotFound
	}
	u, err := s.store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) || (err == nil && !canAccess(p, u.CreatorID)) {
		return Upload{}, errUploadNotFound
	}
	return u, err
}

func (s *Service) partURLs(u Upload, numbers []int, now time.Time) []PartURL {
	out := make([]PartURL, 0, len(numbers))
	for _, n := range numbers {
		size := u.PartSize(n)
		out = append(out, PartURL{Number: n, SizeBytes: size,
			URL: s.objects.PresignUploadPart(u.Bucket, u.ObjectKey, u.S3UploadID, n, size, s.cfg.URLTTL)})
	}
	return out
}

// ---- operations

// Create starts a resumable multipart upload for a title the caller owns.
func (s *Service) Create(ctx context.Context, p auth.Principal, in CreateInput) (CreateResult, error) {
	container, fields := s.validateCreate(in)
	if len(fields) > 0 {
		return CreateResult{}, &Problem{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
			Message: "one or more fields are invalid", Fields: fields}
	}
	now := s.now()

	title, err := s.store.Title(ctx, in.TitleID)
	switch {
	case errors.Is(err, ErrTitleNotFound):
		return CreateResult{}, errTitleNotFound
	case err != nil:
		return CreateResult{}, err
	case !canAccess(p, title.CreatorID):
		return CreateResult{}, errTitleNotFound // do not reveal that the title exists
	case title.Type != "movie":
		return CreateResult{}, &Problem{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
			Message: "one or more fields are invalid",
			Fields:  map[string]string{"title_id": "uploads are only supported for movies; series episodes come later"}}
	case title.HasAsset:
		return CreateResult{}, problem(http.StatusConflict, "video_exists", "this title already has a video; remove it before uploading a new one")
	}

	active, err := s.store.CountActive(ctx, p.UserID)
	if err != nil {
		return CreateResult{}, err
	}
	if active >= s.cfg.MaxActive {
		return CreateResult{}, problem(http.StatusTooManyRequests, "too_many_uploads",
			fmt.Sprintf("you already have %d unfinished uploads; finish or cancel one first", active))
	}
	used, err := s.store.BytesSince(ctx, p.UserID, now.Add(-24*time.Hour))
	if err != nil {
		return CreateResult{}, err
	}
	if used+in.SizeBytes > s.cfg.DailyBytes {
		return CreateResult{}, problem(http.StatusTooManyRequests, "daily_quota_exceeded",
			fmt.Sprintf("this would exceed your daily upload limit of %s; try again later", humanBytes(s.cfg.DailyBytes)))
	}

	id, err := newUUID()
	if err != nil {
		return CreateResult{}, err
	}
	u := Upload{
		ID: id, CreatorID: p.UserID, TitleID: in.TitleID, Filename: strings.TrimSpace(in.Filename),
		ContentType: containerTypes[container], Container: container, SizeBytes: in.SizeBytes,
		Bucket: s.cfg.RawBucket, ObjectKey: id + "/source." + container,
		Status: StatusInitiated, ExpiresAt: now.Add(s.cfg.AbandonAfter), CreatedAt: now, UpdatedAt: now,
	}
	u.PartSizeBytes, u.PartCount = s.cfg.PartBytes, int((in.SizeBytes+s.cfg.PartBytes-1)/s.cfg.PartBytes)
	if u.PartCount == 1 {
		u.PartSizeBytes = in.SizeBytes
	}

	u.S3UploadID, err = s.objects.CreateMultipartUpload(ctx, u.Bucket, u.ObjectKey, u.ContentType)
	if err != nil {
		return CreateResult{}, storageUnavailable(fmt.Errorf("start multipart upload: %w", err))
	}
	if err := s.store.Create(ctx, u); err != nil {
		s.abortQuietly(u)
		switch {
		case errors.Is(err, ErrTitleBusy):
			return CreateResult{}, problem(http.StatusConflict, "upload_in_progress", "this title already has an unfinished upload; finish or cancel it first")
		case errors.Is(err, ErrTitleNotFound):
			return CreateResult{}, errTitleNotFound
		}
		return CreateResult{}, err
	}

	first := s.cfg.InitialParts
	if first > u.PartCount {
		first = u.PartCount
	}
	numbers := make([]int, first)
	for i := range numbers {
		numbers[i] = i + 1
	}
	s.logger.InfoContext(ctx, "upload started", "upload_id", u.ID, "creator_id", u.CreatorID,
		"title_id", u.TitleID, "size_bytes", u.SizeBytes, "parts", u.PartCount)
	return CreateResult{Upload: u, Parts: s.partURLs(u, numbers, now), URLsExpireAt: now.Add(s.cfg.URLTTL)}, nil
}

// PartURLs returns fresh presigned URLs for the requested part numbers.
func (s *Service) PartURLs(ctx context.Context, p auth.Principal, id string, numbers []int) (PartsResult, error) {
	u, err := s.getOwned(ctx, p, id)
	if err != nil {
		return PartsResult{}, err
	}
	if len(numbers) == 0 || len(numbers) > MaxPartsPerRequest {
		return PartsResult{}, &Problem{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
			Message: "one or more fields are invalid", Fields: map[string]string{
				"part_numbers": fmt.Sprintf("must list 1 to %d part numbers", MaxPartsPerRequest)}}
	}
	seen := map[int]bool{}
	var uniq []int
	for _, n := range numbers {
		if n < 1 || n > u.PartCount {
			return PartsResult{}, &Problem{Status: http.StatusUnprocessableEntity, Code: "validation_failed",
				Message: "one or more fields are invalid", Fields: map[string]string{
					"part_numbers": fmt.Sprintf("each part number must be from 1 to %d", u.PartCount)}}
		}
		if !seen[n] {
			seen[n] = true
			uniq = append(uniq, n)
		}
	}
	sort.Ints(uniq)
	now := s.now()
	if u.Status != StatusInitiated {
		return PartsResult{}, problem(http.StatusConflict, "upload_not_open", "this upload is no longer accepting parts")
	}
	if !now.Before(u.ExpiresAt) {
		return PartsResult{}, problem(http.StatusConflict, "upload_expired", "this upload expired; start a new one")
	}
	return PartsResult{Parts: s.partURLs(u, uniq, now), URLsExpireAt: now.Add(s.cfg.URLTTL)}, nil
}

// Get returns an upload's state. While it is unfinished it also lists the parts already stored,
// which is what a client needs to resume after a lost connection.
func (s *Service) Get(ctx context.Context, p auth.Principal, id string) (Details, error) {
	u, err := s.getOwned(ctx, p, id)
	if err != nil {
		return Details{}, err
	}
	d := Details{Upload: u}
	if j, err := s.store.JobFor(ctx, u.ID); err == nil {
		d.Job = &j
	} else if !errors.Is(err, ErrNotFound) {
		return Details{}, err
	}
	if u.Status == StatusInitiated && u.S3UploadID != "" {
		parts, err := s.objects.ListParts(ctx, u.Bucket, u.ObjectKey, u.S3UploadID)
		if err != nil && !s3.IsNotFound(err) {
			return Details{}, fmt.Errorf("list parts: %w", err)
		}
		d.StoredParts = parts
	}
	return d, nil
}

// Complete assembles the uploaded parts, checks the file, and queues the transcode job. It is
// safe to call again: a finished upload just returns its job.
func (s *Service) Complete(ctx context.Context, p auth.Principal, id string) (Details, error) {
	u, err := s.getOwned(ctx, p, id)
	if err != nil {
		return Details{}, err
	}
	switch u.Status {
	case StatusUploaded, StatusProcessing, StatusReady:
		return s.details(ctx, u)
	case StatusRejected:
		return Details{}, problem(http.StatusConflict, "upload_rejected", "this upload was rejected: "+u.RejectReason)
	case StatusAborted, StatusExpired:
		return Details{}, problem(http.StatusConflict, "upload_closed", "this upload was cancelled or expired; start a new one")
	}
	now := s.now()
	if !now.Before(u.ExpiresAt) {
		return Details{}, problem(http.StatusConflict, "upload_expired", "this upload expired; start a new one")
	}

	// 1. Every part must be stored, with exactly the size it was given.
	parts, err := s.objects.ListParts(ctx, u.Bucket, u.ObjectKey, u.S3UploadID)
	assembled := false
	switch {
	case s3.IsNotFound(err):
		// An earlier call may have assembled the object and then failed before saving: check for it.
		info, herr := s.objects.Head(ctx, u.Bucket, u.ObjectKey)
		if herr != nil || info.Size != u.SizeBytes {
			return Details{}, problem(http.StatusConflict, "upload_incomplete", "the stored upload was not found; start a new one")
		}
		assembled = true
	case err != nil:
		return Details{}, storageUnavailable(err)
	default:
		if msg := s.checkParts(u, parts); msg != "" {
			return Details{}, problem(http.StatusUnprocessableEntity, "upload_incomplete", msg)
		}
	}

	// 2. Assemble.
	if !assembled {
		if err := s.objects.CompleteMultipartUpload(ctx, u.Bucket, u.ObjectKey, u.S3UploadID, parts); err != nil {
			return Details{}, storageUnavailable(err)
		}
	}

	// 3. Size, then the file signature.
	info, err := s.objects.Head(ctx, u.Bucket, u.ObjectKey)
	if err != nil {
		return Details{}, storageUnavailable(err)
	}
	if info.Size != u.SizeBytes {
		return Details{}, s.reject(ctx, u, RejectSizeMismatch,
			fmt.Sprintf("the file is %d bytes but %d bytes were declared", info.Size, u.SizeBytes))
	}
	headLen := int64(4096)
	if u.SizeBytes < headLen {
		headLen = u.SizeBytes
	}
	head, err := s.objects.GetRange(ctx, u.Bucket, u.ObjectKey, 0, headLen)
	if err != nil {
		return Details{}, storageUnavailable(err)
	}
	family := sniffFamily(head)
	switch {
	case family == "":
		return Details{}, s.reject(ctx, u, RejectUnsupportedContainer,
			"this file is not a recognised video container (MP4, MOV, MKV or WebM)")
	case family != containerFamily(u.Container):
		return Details{}, s.reject(ctx, u, RejectContainerMismatch,
			fmt.Sprintf("the file extension .%s does not match the file contents", u.Container))
	}

	// 4. Malware scan. A scanner that fails leaves the upload retryable.
	threat, err := s.scanner.Scan(ctx, ScanTarget{Bucket: u.Bucket, Key: u.ObjectKey, Size: u.SizeBytes})
	if err != nil {
		s.logger.ErrorContext(ctx, "upload scan failed", "upload_id", u.ID, "error", err.Error())
		return Details{}, problem(http.StatusServiceUnavailable, "scan_unavailable", "the file could not be checked right now; try again shortly")
	}
	if threat != "" {
		return Details{}, s.reject(ctx, u, RejectMalware, "the file did not pass the security check")
	}

	// 5. Queue the job in the same transaction that marks the upload uploaded.
	job, err := s.store.MarkUploaded(ctx, u.ID, s.cfg.Policy, now)
	if errors.Is(err, ErrConflict) { // another call won the race
		cur, gerr := s.store.Get(ctx, u.ID)
		if gerr != nil {
			return Details{}, gerr
		}
		return s.details(ctx, cur)
	}
	if err != nil {
		return Details{}, err
	}
	u.Status, u.S3UploadID, u.CompletedAt = StatusUploaded, "", &now
	s.logger.InfoContext(ctx, "upload completed", "upload_id", u.ID, "job_id", job.ID, "size_bytes", u.SizeBytes)
	return Details{Upload: u, Job: &job}, nil
}

func (s *Service) details(ctx context.Context, u Upload) (Details, error) {
	d := Details{Upload: u}
	j, err := s.store.JobFor(ctx, u.ID)
	switch {
	case err == nil:
		d.Job = &j
	case !errors.Is(err, ErrNotFound):
		return Details{}, err
	}
	return d, nil
}

// checkParts returns "" when parts is exactly the expected set, otherwise a message for the client.
func (s *Service) checkParts(u Upload, parts []s3.Part) string {
	have := make(map[int]s3.Part, len(parts))
	for _, p := range parts {
		have[p.Number] = p
	}
	var missing, wrong []string
	for n := 1; n <= u.PartCount; n++ {
		p, ok := have[n]
		switch {
		case !ok:
			if len(missing) < 20 {
				missing = append(missing, fmt.Sprint(n))
			}
		case p.Size != u.PartSize(n):
			if len(wrong) < 20 {
				wrong = append(wrong, fmt.Sprint(n))
			}
		}
	}
	switch {
	case len(missing) > 0:
		return fmt.Sprintf("parts still missing: %s (of %d). Upload them, then call complete again", strings.Join(missing, ", "), u.PartCount)
	case len(wrong) > 0:
		return fmt.Sprintf("parts with the wrong size: %s. Upload them again with exactly the size you were given", strings.Join(wrong, ", "))
	case len(parts) != u.PartCount:
		return "the upload has unexpected extra parts; start a new upload"
	}
	return ""
}

// reject records the refusal, deletes the object, and returns the problem to show the creator.
func (s *Service) reject(ctx context.Context, u Upload, code, reason string) error {
	if err := s.store.Reject(ctx, u.ID, code, reason, s.now()); err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	if err := s.objects.Delete(ctx, u.Bucket, u.ObjectKey); err != nil {
		s.logger.ErrorContext(ctx, "could not delete a rejected upload's object", "upload_id", u.ID, "error", err.Error())
	}
	s.logger.WarnContext(ctx, "upload rejected", "upload_id", u.ID, "reason_code", code)
	return problem(http.StatusUnprocessableEntity, code, reason)
}

// Abort cancels an unfinished upload and frees its storage. Cancelling twice is fine.
func (s *Service) Abort(ctx context.Context, p auth.Principal, id string) error {
	u, err := s.getOwned(ctx, p, id)
	if err != nil {
		return err
	}
	switch u.Status {
	case StatusAborted, StatusExpired:
		return nil
	case StatusInitiated:
	default:
		return problem(http.StatusConflict, "upload_not_cancellable", "this upload can no longer be cancelled")
	}
	if u.S3UploadID != "" {
		if err := s.objects.AbortMultipartUpload(ctx, u.Bucket, u.ObjectKey, u.S3UploadID); err != nil {
			return storageUnavailable(err)
		}
	}
	if err := s.store.Close(ctx, u.ID, StatusAborted, s.now()); err != nil {
		if errors.Is(err, ErrConflict) {
			return problem(http.StatusConflict, "upload_not_cancellable", "this upload can no longer be cancelled")
		}
		return err
	}
	s.logger.InfoContext(ctx, "upload cancelled", "upload_id", u.ID)
	return nil
}

func (s *Service) abortQuietly(u Upload) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.objects.AbortMultipartUpload(ctx, u.Bucket, u.ObjectKey, u.S3UploadID); err != nil {
		s.logger.Error("could not abort a multipart upload after a failed insert", "upload_id", u.ID, "error", err.Error())
	}
}

func storageUnavailable(err error) error {
	return &storageError{err: err}
}

// storageError is an object-store failure: answered with 502 and logged, never shown in detail.
type storageError struct{ err error }

func (e *storageError) Error() string { return "object storage: " + e.err.Error() }
func (e *storageError) Unwrap() error { return e.err }
