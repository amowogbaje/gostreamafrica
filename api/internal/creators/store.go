package creators

import (
	"context"
	"time"
)

// Store is the persistence the upload flow needs. The Postgres implementation is store_pg.go;
// tests use it too (against a real database) so SQL is exercised, not mocked.
type Store interface {
	// Title returns ErrTitleNotFound for an unknown id.
	Title(ctx context.Context, id string) (TitleInfo, error)
	// CountActive counts the creator's unfinished uploads (initiated, uploaded, processing).
	CountActive(ctx context.Context, creatorID string) (int, error)
	// BytesSince sums the declared size of the creator's uploads started after since,
	// ignoring those that were aborted or expired.
	BytesSince(ctx context.Context, creatorID string, since time.Time) (int64, error)
	// Create inserts an upload. ErrTitleBusy when the title already has an unfinished one.
	Create(ctx context.Context, u Upload) error
	// Get returns ErrNotFound for an unknown id.
	Get(ctx context.Context, id string) (Upload, error)
	// MarkUploaded moves initiated -> uploaded and queues the job in one transaction.
	// ErrConflict when the upload is no longer 'initiated'.
	MarkUploaded(ctx context.Context, id string, policy Policy, now time.Time) (Job, error)
	// JobFor returns the newest job of an upload, or ErrNotFound when none was queued.
	JobFor(ctx context.Context, uploadID string) (Job, error)
	// Reject records that the file is unacceptable. ErrConflict unless initiated or uploaded.
	Reject(ctx context.Context, id, code, reason string, now time.Time) error
	// Close moves an 'initiated' upload to aborted or expired and clears its storage upload id.
	// ErrConflict when the upload is no longer 'initiated'.
	Close(ctx context.Context, id string, to Status, now time.Time) error
	// Expired lists 'initiated' uploads whose deadline has passed, oldest first.
	Expired(ctx context.Context, now time.Time, limit int) ([]Upload, error)
	// WithLock runs fn only if no other process holds the named lock; ran reports whether it did.
	WithLock(ctx context.Context, key int64, fn func(context.Context) error) (ran bool, err error)
}
