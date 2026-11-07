package creators

import (
	"errors"
	"time"
)

// Status is where an upload is in its life.
type Status string

const (
	StatusInitiated  Status = "initiated"  // the creator is sending parts
	StatusUploaded   Status = "uploaded"   // complete, checked, a transcode job is queued
	StatusProcessing Status = "processing" // the worker has the job
	StatusReady      Status = "ready"      // an HLS asset exists
	StatusRejected   Status = "rejected"   // the file is not acceptable
	StatusAborted    Status = "aborted"    // the creator cancelled
	StatusExpired    Status = "expired"    // never finished in time
	StatusDeleted    Status = "deleted"    // the creator deleted the finished video
)

// Rejection codes. Clients may branch on these; the wording of the reason may change.
const (
	RejectSizeMismatch         = "size_mismatch"
	RejectUnsupportedContainer = "unsupported_container"
	RejectContainerMismatch    = "container_mismatch"
	RejectMalware              = "malware_detected"
)

// Errors returned by a Store.
var (
	ErrNotFound      = errors.New("not found")
	ErrTitleNotFound = errors.New("title not found")
	ErrTitleBusy     = errors.New("title already has an unfinished upload")
	// ErrConflict means the upload was not in the state the change needed (someone else moved it first).
	ErrConflict = errors.New("upload is not in the expected state")
)

// Upload is the database record of one upload attempt.
type Upload struct {
	ID            string
	CreatorID     string
	TitleID       string
	Filename      string
	ContentType   string
	Container     string
	SizeBytes     int64
	PartSizeBytes int64
	PartCount     int
	Bucket        string
	ObjectKey     string
	S3UploadID    string // empty once the multipart upload is completed or aborted
	Status        Status
	RejectCode    string
	RejectReason  string
	ExpiresAt     time.Time
	CompletedAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PartSize is the exact number of bytes part n (1-based) must have: every part is PartSizeBytes
// except the last, which holds the remainder. Out-of-range numbers return 0.
func (u Upload) PartSize(n int) int64 {
	switch {
	case n < 1 || n > u.PartCount:
		return 0
	case n < u.PartCount:
		return u.PartSizeBytes
	}
	return u.SizeBytes - int64(u.PartCount-1)*u.PartSizeBytes
}

// AssetInfo is a media asset with the title it belongs to.
type AssetInfo struct {
	ID          string
	TitleID     string
	CreatorID   string
	Status      string // processing, ready, failed
	TitleStatus string
}

// Job is the transcode job queued for an upload.
type Job struct {
	ID       string
	UploadID string
	State    string
	Attempts int
	Stage    string
	Percent  *int
	Error    string
}

// TitleInfo is what the upload flow needs to know about a title.
type TitleInfo struct {
	ID        string
	CreatorID string
	Type      string
	HasAsset  bool
}
