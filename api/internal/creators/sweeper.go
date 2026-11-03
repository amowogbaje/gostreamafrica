package creators

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// sweepLockKey is the advisory-lock id that makes one API replica at a time run the sweep.
const sweepLockKey int64 = 0x53545245414d // "STREAM"

// orphanGrace keeps the storage-side sweep clear of uploads that are still within their deadline:
// the storage upload is created a moment before its database row.
const orphanGrace = time.Hour

// SweeperConfig configures the abandoned-upload cleanup.
type SweeperConfig struct {
	Bucket       string        // the raw-upload bucket
	AbandonAfter time.Duration // matches Config.AbandonAfter
	Every        time.Duration // how often Run sweeps
	Batch        int           // expired rows handled per round (default 100)
}

// Sweeper cleans up uploads that were started and never finished. Two passes:
//  1. database: uploads still 'initiated' past their deadline are aborted in storage and marked expired;
//  2. storage: any unfinished multipart upload older than the deadline (plus a grace period) is
//     aborted, which also catches uploads whose database row never got written.
//
// Object stores do not reliably apply lifecycle rules to multipart uploads (ADR-0014), so this
// job is the cleanup.
type Sweeper struct {
	store   Store
	objects ObjectStore
	logger  *slog.Logger
	now     func() time.Time
	cfg     SweeperConfig
}

// NewSweeper builds a Sweeper.
func NewSweeper(store Store, objects ObjectStore, logger *slog.Logger, now func() time.Time, cfg SweeperConfig) *Sweeper {
	if now == nil {
		now = time.Now
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	return &Sweeper{store: store, objects: objects, logger: logger, now: now, cfg: cfg}
}

// SweepResult says what one sweep did.
type SweepResult struct {
	Ran            bool // false when another replica held the lock
	Expired        int  // database uploads marked expired
	OrphansAborted int  // storage uploads aborted by age
	Failures       int  // storage calls that failed (retried on the next sweep)
}

// RunOnce performs one sweep.
func (s *Sweeper) RunOnce(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	ran, err := s.store.WithLock(ctx, sweepLockKey, func(ctx context.Context) error {
		s.expireRows(ctx, &res)
		return s.abortOrphans(ctx, &res)
	})
	res.Ran = ran
	return res, err
}

func (s *Sweeper) expireRows(ctx context.Context, res *SweepResult) {
	for round := 0; round < 50 && ctx.Err() == nil; round++ {
		rows, err := s.store.Expired(ctx, s.now(), s.cfg.Batch)
		if err != nil {
			s.logger.ErrorContext(ctx, "upload sweep: listing expired uploads failed", "error", err.Error())
			res.Failures++
			return
		}
		progress := 0
		for _, u := range rows {
			if u.S3UploadID != "" {
				if err := s.objects.AbortMultipartUpload(ctx, u.Bucket, u.ObjectKey, u.S3UploadID); err != nil {
					s.logger.ErrorContext(ctx, "upload sweep: abort failed", "upload_id", u.ID, "error", err.Error())
					res.Failures++
					continue // leave the row: the next sweep tries again
				}
			}
			if err := s.store.Close(ctx, u.ID, StatusExpired, s.now()); err != nil && !errors.Is(err, ErrConflict) {
				s.logger.ErrorContext(ctx, "upload sweep: marking expired failed", "upload_id", u.ID, "error", err.Error())
				res.Failures++
				continue
			}
			progress++
			res.Expired++
			s.logger.InfoContext(ctx, "upload expired", "upload_id", u.ID, "creator_id", u.CreatorID)
		}
		if len(rows) < s.cfg.Batch || progress == 0 {
			return
		}
	}
}

func (s *Sweeper) abortOrphans(ctx context.Context, res *SweepResult) error {
	cutoff := s.now().Add(-(s.cfg.AbandonAfter + orphanGrace))
	ups, err := s.objects.ListMultipartUploads(ctx, s.cfg.Bucket, "")
	if err != nil {
		res.Failures++
		return err
	}
	for _, u := range ups {
		if !u.Initiated.Before(cutoff) {
			continue
		}
		if err := s.objects.AbortMultipartUpload(ctx, s.cfg.Bucket, u.Key, u.UploadID); err != nil {
			s.logger.ErrorContext(ctx, "upload sweep: orphan abort failed", "key", u.Key, "error", err.Error())
			res.Failures++
			continue
		}
		res.OrphansAborted++
		s.logger.InfoContext(ctx, "abandoned storage upload aborted", "key", u.Key, "started", u.Initiated)
	}
	return nil
}

// Run sweeps shortly after start and then every cfg.Every until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		res, err := s.RunOnce(ctx)
		switch {
		case err != nil:
			s.logger.ErrorContext(ctx, "upload sweep failed", "error", err.Error())
		case res.Ran && (res.Expired > 0 || res.OrphansAborted > 0 || res.Failures > 0):
			s.logger.InfoContext(ctx, "upload sweep done", "expired", res.Expired,
				"orphans_aborted", res.OrphansAborted, "failures", res.Failures)
		}
		timer.Reset(s.cfg.Every)
	}
}
