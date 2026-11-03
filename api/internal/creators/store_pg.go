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
