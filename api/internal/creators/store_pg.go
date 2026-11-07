package creators

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the PostgreSQL Store. All SQL is parameterized.
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore wraps a pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const uploadCols = `id::text, creator_id::text, title_id::text, filename, content_type, container, size_bytes,
	part_size_bytes, part_count, bucket, object_key, COALESCE(s3_upload_id, ''), status::text,
	COALESCE(reject_code, ''), COALESCE(reject_reason, ''), expires_at, completed_at, created_at, updated_at`

func scanUpload(row pgx.Row) (Upload, error) {
	var u Upload
	var status string
	err := row.Scan(&u.ID, &u.CreatorID, &u.TitleID, &u.Filename, &u.ContentType, &u.Container, &u.SizeBytes,
		&u.PartSizeBytes, &u.PartCount, &u.Bucket, &u.ObjectKey, &u.S3UploadID, &status,
		&u.RejectCode, &u.RejectReason, &u.ExpiresAt, &u.CompletedAt, &u.CreatedAt, &u.UpdatedAt)
	u.Status = Status(status)
	return u, err
}

const jobCols = `id::text, upload_id::text, state::text, attempts, COALESCE(stage, ''), percent, COALESCE(last_error, '')`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var pct *int16
	err := row.Scan(&j.ID, &j.UploadID, &j.State, &j.Attempts, &j.Stage, &pct, &j.Error)
	if pct != nil {
		p := int(*pct)
		j.Percent = &p
	}
	return j, err
}

// Title implements Store.
func (s *PGStore) Title(ctx context.Context, id string) (TitleInfo, error) {
	if !uuidRE.MatchString(id) {
		return TitleInfo{}, ErrTitleNotFound
	}
	var t TitleInfo
	err := s.pool.QueryRow(ctx, `
		SELECT t.id::text, t.creator_id::text, t.type::text,
		       EXISTS (SELECT 1 FROM media_assets m WHERE m.title_id = t.id AND m.status = 'ready')
		  FROM titles t WHERE t.id = $1::uuid`, id).Scan(&t.ID, &t.CreatorID, &t.Type, &t.HasAsset)
	if errors.Is(err, pgx.ErrNoRows) {
		return TitleInfo{}, ErrTitleNotFound
	}
	return t, err
}

// CountActive implements Store.
func (s *PGStore) CountActive(ctx context.Context, creatorID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM uploads
		 WHERE creator_id = $1::uuid AND status IN ('initiated', 'uploaded', 'processing')`, creatorID).Scan(&n)
	return n, err
}

// BytesSince implements Store.
func (s *PGStore) BytesSince(ctx context.Context, creatorID string, since time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(size_bytes), 0)::bigint FROM uploads
		 WHERE creator_id = $1::uuid AND created_at > $2 AND status NOT IN ('aborted', 'expired')`,
		creatorID, since).Scan(&n)
	return n, err
}

// Create implements Store.
func (s *PGStore) Create(ctx context.Context, u Upload) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO uploads (id, creator_id, title_id, filename, content_type, container, size_bytes,
		                     part_size_bytes, part_count, bucket, object_key, s3_upload_id, status,
		                     expires_at, created_at, updated_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'initiated', $13, $14, $14)`,
		u.ID, u.CreatorID, u.TitleID, u.Filename, u.ContentType, u.Container, u.SizeBytes,
		u.PartSizeBytes, u.PartCount, u.Bucket, u.ObjectKey, u.S3UploadID, u.ExpiresAt, u.CreatedAt)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "uploads_one_active_per_title" {
		return ErrTitleBusy
	}
	return err
}

// Get implements Store.
func (s *PGStore) Get(ctx context.Context, id string) (Upload, error) {
	if !uuidRE.MatchString(id) {
		return Upload{}, ErrNotFound
	}
	u, err := scanUpload(s.pool.QueryRow(ctx, `SELECT `+uploadCols+` FROM uploads WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	return u, err
}

// MarkUploaded implements Store.
func (s *PGStore) MarkUploaded(ctx context.Context, id string, policy Policy, now time.Time) (Job, error) {
	raw, err := json.Marshal(policy)
	if err != nil {
		return Job{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE uploads SET status = 'uploaded', s3_upload_id = NULL, completed_at = $2, updated_at = $2
		 WHERE id = $1::uuid AND status = 'initiated'`, id, now)
	if err != nil {
		return Job{}, err
	}
	if tag.RowsAffected() == 0 {
		return Job{}, ErrConflict
	}
	job, err := scanJob(tx.QueryRow(ctx, `
		INSERT INTO transcode_jobs (upload_id, policy, created_at, updated_at, run_after)
		VALUES ($1::uuid, $2::jsonb, $3, $3, $3) RETURNING `+jobCols, id, raw, now))
	if err != nil {
		return Job{}, err
	}
	return job, tx.Commit(ctx)
}

// JobFor implements Store.
func (s *PGStore) JobFor(ctx context.Context, uploadID string) (Job, error) {
	if !uuidRE.MatchString(uploadID) {
		return Job{}, ErrNotFound
	}
	j, err := scanJob(s.pool.QueryRow(ctx, `
		SELECT `+jobCols+` FROM transcode_jobs WHERE upload_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, uploadID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// Reject implements Store.
func (s *PGStore) Reject(ctx context.Context, id, code, reason string, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = 'rejected', reject_code = $2, reject_reason = $3, s3_upload_id = NULL, updated_at = $4
		 WHERE id = $1::uuid AND status IN ('initiated', 'uploaded')`, id, code, reason, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// Close implements Store.
func (s *PGStore) Close(ctx context.Context, id string, to Status, now time.Time) error {
	if to != StatusAborted && to != StatusExpired {
		return errors.New("creators: Close only moves to aborted or expired")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE uploads SET status = $2::upload_status, s3_upload_id = NULL, updated_at = $3
		 WHERE id = $1::uuid AND status = 'initiated'`, id, string(to), now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// Expired implements Store.
func (s *PGStore) Expired(ctx context.Context, now time.Time, limit int) ([]Upload, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+uploadCols+` FROM uploads
		 WHERE status = 'initiated' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// WithLock implements Store using a session-level advisory lock, so only one API replica
// runs the sweep at a time and a crashed holder releases the lock when its connection drops.
func (s *PGStore) WithLock(ctx context.Context, key int64, fn func(context.Context) error) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil || !got {
		return false, err
	}
	defer func() {
		// Use a fresh context: ctx may already be cancelled, and the lock must still be released.
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key)
	}()
	return true, fn(ctx)
}

// Requeue implements Store.
func (s *PGStore) Requeue(ctx context.Context, id string, now time.Time) (Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE uploads SET status = 'uploaded', reject_code = NULL, reject_reason = NULL, updated_at = $2
		WHERE id = $1::uuid AND status = 'rejected' AND reject_code = 'processing_failed'`, id, now)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "uploads_one_active_per_title" {
		return Job{}, ErrTitleBusy
	}
	if err != nil {
		return Job{}, err
	}
	if tag.RowsAffected() == 0 {
		return Job{}, ErrConflict
	}
	job, err := scanJob(tx.QueryRow(ctx, `UPDATE transcode_jobs SET state = 'queued', attempts = 0, run_after = $2, finished_at = NULL,
		last_error = NULL, stage = NULL, percent = NULL, locked_by = NULL, locked_until = NULL, updated_at = $2
		WHERE id = (SELECT id FROM transcode_jobs WHERE upload_id = $1::uuid AND state = 'failed' ORDER BY created_at DESC LIMIT 1)
		RETURNING `+jobCols, id, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrConflict
	}
	if err != nil {
		return Job{}, err
	}
	return job, tx.Commit(ctx)
}

// Asset implements Store.
func (s *PGStore) Asset(ctx context.Context, assetID string) (AssetInfo, error) {
	if !uuidRE.MatchString(assetID) {
		return AssetInfo{}, ErrNotFound
	}
	var a AssetInfo
	err := s.pool.QueryRow(ctx, `
		SELECT a.id::text, t.id::text, t.creator_id::text, a.status::text, t.status::text
		  FROM media_assets a
		  LEFT JOIN episodes e ON e.id = a.episode_id
		  JOIN titles t ON t.id = COALESCE(a.title_id, e.title_id)
		 WHERE a.id = $1::uuid`, assetID).Scan(&a.ID, &a.TitleID, &a.CreatorID, &a.Status, &a.TitleStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetInfo{}, ErrNotFound
	}
	return a, err
}

// ReadyUploadIDs implements Store.
func (s *PGStore) ReadyUploadIDs(ctx context.Context, titleID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM uploads WHERE title_id = $1::uuid AND status = 'ready' ORDER BY created_at`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UnpublishTitle implements Store.
func (s *PGStore) UnpublishTitle(ctx context.Context, titleID string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE titles SET status = 'unpublished', updated_at = $2 WHERE id = $1::uuid AND status = 'published'`, titleID, now)
	return err
}

// DeleteAsset implements Store.
func (s *PGStore) DeleteAsset(ctx context.Context, assetID string, uploadIDs []string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM media_assets WHERE id = $1::uuid`, assetID); err != nil {
		return err
	}
	if len(uploadIDs) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE uploads SET status = 'deleted', updated_at = $2 WHERE id = ANY($1::uuid[]) AND status = 'ready'`, uploadIDs, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
