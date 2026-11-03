// Package jobs is the transcode queue. It lives in PostgreSQL (the transcode_jobs table the API
// writes, see docs/adr/0016): a job is claimed with FOR UPDATE SKIP LOCKED, holds a lease that its
// worker keeps extending, and goes back to the queue when the lease runs out. That gives
// at-least-once delivery, a visibility timeout, retries with backoff and a dead-letter state
// (state 'failed') without another moving part, and the job and upload states change in the same
// transaction.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/worker/internal/transcode"
)

// ErrLeaseLost means another worker owns the job now (ours expired); the caller must stop and write nothing.
var ErrLeaseLost = errors.New("jobs: lease lost")

// Reason shown to the creator when a job runs out of attempts.
const (
	ProcessingFailedCode   = "processing_failed"
	processingFailedReason = "We could not process this video after several attempts. Please upload it again, or contact support if it keeps failing."
)

// Job is a claimed job.
type Job struct {
	ID          string
	UploadID    string
	Attempts    int // including this attempt
	MaxAttempts int
	Policy      transcode.Policy
}

// Upload is the source file of a job.
type Upload struct {
	ID        string
	TitleID   string
	Bucket    string
	Key       string
	SizeBytes int64
	Container string
}

// Store is the PostgreSQL queue.
type Store struct {
	pool     *pgxpool.Pool
	workerID string
	lease    time.Duration
}

// NewStore builds a Store. lease is how long a claim lasts without a heartbeat.
func NewStore(pool *pgxpool.Pool, workerID string, lease time.Duration) *Store {
	return &Store{pool: pool, workerID: workerID, lease: lease}
}

func secs(d time.Duration) float64 { return d.Seconds() }

// Claim takes the next runnable job, or returns nil when there is none.
func (s *Store) Claim(ctx context.Context) (*Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var j Job
	var policy []byte
	err = tx.QueryRow(ctx, `
		UPDATE transcode_jobs SET state = 'running', locked_by = $1, locked_until = now() + make_interval(secs => $2),
		       attempts = attempts + 1, started_at = COALESCE(started_at, now()), updated_at = now(),
		       stage = 'starting', percent = 0
		 WHERE id = (SELECT id FROM transcode_jobs WHERE state = 'queued' AND run_after <= now()
		              ORDER BY run_after, created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id::text, upload_id::text, attempts, max_attempts, policy`, s.workerID, secs(s.lease)).
		Scan(&j.ID, &j.UploadID, &j.Attempts, &j.MaxAttempts, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(policy, &j.Policy); err != nil {
		return nil, fmt.Errorf("job %s has an unreadable policy: %w", j.ID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'processing', updated_at = now()
		WHERE id = $1::uuid AND status IN ('uploaded', 'processing')`, j.UploadID); err != nil {
		return nil, err
	}
	return &j, tx.Commit(ctx)
}

// Heartbeat extends the lease. ErrLeaseLost when the job is no longer ours.
func (s *Store) Heartbeat(ctx context.Context, jobID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE transcode_jobs SET locked_until = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = $1::uuid AND locked_by = $2 AND state = 'running'`, jobID, s.workerID, secs(s.lease))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Progress records the stage and percentage shown to the creator. Failures are not fatal to the job.
func (s *Store) Progress(ctx context.Context, jobID, stage string, percent int) error {
	_, err := s.pool.Exec(ctx, `UPDATE transcode_jobs SET stage = $3, percent = $4, updated_at = now()
		WHERE id = $1::uuid AND locked_by = $2 AND state = 'running'`, jobID, s.workerID, stage, max(0, min(100, percent)))
	return err
}

// LoadUpload returns the source file of an upload.
func (s *Store) LoadUpload(ctx context.Context, uploadID string) (Upload, error) {
	var u Upload
	err := s.pool.QueryRow(ctx, `SELECT id::text, title_id::text, bucket, object_key, size_bytes, container FROM uploads WHERE id = $1::uuid`, uploadID).
		Scan(&u.ID, &u.TitleID, &u.Bucket, &u.Key, &u.SizeBytes, &u.Container)
	return u, err
}

// UpsertAsset returns the media asset of a title, creating it or putting it back to 'processing'.
// Reprocessing therefore reuses the same asset id and the same storage prefix.
func (s *Store) UpsertAsset(ctx context.Context, titleID, sourceKey string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO media_assets (title_id, status, source_key) VALUES ($1::uuid, 'processing', $2)
		ON CONFLICT (title_id) WHERE title_id IS NOT NULL
		DO UPDATE SET status = 'processing', source_key = EXCLUDED.source_key, hls_manifest_key = NULL,
		              duration_seconds = NULL, updated_at = now()
		RETURNING id::text`, titleID, sourceKey).Scan(&id)
	return id, err
}

// UpsertSubtitle records a converted subtitle track.
func (s *Store) UpsertSubtitle(ctx context.Context, assetID, language, label, vttKey string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO subtitles (asset_id, language, label, vtt_url) VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (asset_id, language) DO UPDATE SET label = EXCLUDED.label, vtt_url = EXCLUDED.vtt_url`,
		assetID, language, label, vttKey)
	return err
}

// Finish marks the job, the upload and the asset as done, atomically. ErrLeaseLost if the job
// was taken over, in which case nothing is changed.
func (s *Store) Finish(ctx context.Context, job Job, assetID, manifestKey string, durationSecs int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE transcode_jobs SET state = 'succeeded', finished_at = now(), stage = 'done', percent = 100,
		locked_by = NULL, locked_until = NULL, last_error = NULL, updated_at = now()
		WHERE id = $1::uuid AND locked_by = $2 AND state = 'running'`, job.ID, s.workerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE media_assets SET status = 'ready', hls_manifest_key = $2, duration_seconds = $3, updated_at = now()
		WHERE id = $1::uuid`, assetID, manifestKey, max(1, durationSecs)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'ready', updated_at = now() WHERE id = $1::uuid`, job.UploadID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Reject ends the job for good because the file is unacceptable: no retry. The creator sees code
// and reason on the upload.
func (s *Store) Reject(ctx context.Context, job Job, assetID string, rj *transcode.Rejection) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE transcode_jobs SET state = 'failed', finished_at = now(), last_error = $3,
		locked_by = NULL, locked_until = NULL, updated_at = now()
		WHERE id = $1::uuid AND locked_by = $2 AND state = 'running'`, job.ID, s.workerID, rj.Code+": "+rj.Reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'rejected', reject_code = $2, reject_reason = $3, updated_at = now()
		WHERE id = $1::uuid`, job.UploadID, rj.Code, rj.Reason); err != nil {
		return err
	}
	if assetID != "" {
		if _, err := tx.Exec(ctx, `UPDATE media_assets SET status = 'failed', updated_at = now() WHERE id = $1::uuid AND status = 'processing'`, assetID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Retry puts a failed attempt back in the queue after delay, or, when attempts are used up,
// moves it to the dead-letter state ('failed') and tells the creator. It reports whether the job is dead.
func (s *Store) Retry(ctx context.Context, job Job, cause error, delay time.Duration, assetID string) (dead bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state string
	err = tx.QueryRow(ctx, `UPDATE transcode_jobs SET
		state = CASE WHEN attempts >= max_attempts THEN 'failed'::job_state ELSE 'queued'::job_state END,
		run_after = now() + make_interval(secs => $3), last_error = $4, locked_by = NULL, locked_until = NULL, percent = NULL,
		finished_at = CASE WHEN attempts >= max_attempts THEN now() END, updated_at = now()
		WHERE id = $1::uuid AND locked_by = $2 AND state = 'running' RETURNING state::text`,
		job.ID, s.workerID, secs(delay), truncate(cause.Error(), 2000)).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, err
	}
	if state == "failed" {
		if err := killUpload(ctx, tx, job.UploadID, assetID); err != nil {
			return false, err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'uploaded', updated_at = now() WHERE id = $1::uuid AND status = 'processing'`, job.UploadID); err != nil {
		return false, err
	}
	return state == "failed", tx.Commit(ctx)
}

func killUpload(ctx context.Context, tx pgx.Tx, uploadID, assetID string) error {
	if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'rejected', reject_code = $2, reject_reason = $3, updated_at = now()
		WHERE id = $1::uuid AND status IN ('uploaded', 'processing')`, uploadID, ProcessingFailedCode, processingFailedReason); err != nil {
		return err
	}
	if assetID == "" {
		_, err := tx.Exec(ctx, `UPDATE media_assets SET status = 'failed', updated_at = now()
			WHERE status = 'processing' AND title_id = (SELECT title_id FROM uploads WHERE id = $1::uuid)`, uploadID)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE media_assets SET status = 'failed', updated_at = now() WHERE id = $1::uuid AND status = 'processing'`, assetID)
	return err
}

// Release hands a job back untouched because the worker is shutting down: the attempt does not count.
func (s *Store) Release(ctx context.Context, job Job) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE transcode_jobs SET state = 'queued', attempts = GREATEST(attempts - 1, 0), run_after = now(),
		locked_by = NULL, locked_until = NULL, percent = NULL, stage = NULL, updated_at = now()
		WHERE id = $1::uuid AND locked_by = $2 AND state = 'running'`, job.ID, s.workerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'uploaded', updated_at = now() WHERE id = $1::uuid AND status = 'processing'`, job.UploadID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Reap finds running jobs whose lease expired (their worker died or hung) and re-queues them, or
// dead-letters those out of attempts. It returns how many it touched.
func (s *Store) Reap(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `UPDATE transcode_jobs SET
		state = CASE WHEN attempts >= max_attempts THEN 'failed'::job_state ELSE 'queued'::job_state END,
		run_after = now(), last_error = 'the worker stopped responding (lease expired)', locked_by = NULL, locked_until = NULL, percent = NULL,
		finished_at = CASE WHEN attempts >= max_attempts THEN now() END, updated_at = now()
		WHERE state = 'running' AND locked_until < now() RETURNING upload_id::text, state::text`)
	if err != nil {
		return 0, err
	}
	type res struct{ upload, state string }
	var all []res
	for rows.Next() {
		var r res
		if err := rows.Scan(&r.upload, &r.state); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range all {
		if r.state == "failed" {
			if err := killUpload(ctx, tx, r.upload, ""); err != nil {
				return 0, err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'uploaded', updated_at = now() WHERE id = $1::uuid AND status = 'processing'`, r.upload); err != nil {
			return 0, err
		}
	}
	return len(all), tx.Commit(ctx)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// Backoff is the delay before attempt number `attempt` (1 = the first retry): base, 2x, 4x...
// capped at max.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}
