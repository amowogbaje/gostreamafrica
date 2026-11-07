package playback

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// PGStore is the PostgreSQL Store.
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore wraps a pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Ready implements Store. The id may be a movie's title id or an episode id.
func (s *PGStore) Ready(ctx context.Context, playableID string) (Asset, error) {
	if !uuidRE.MatchString(playableID) {
		return Asset{}, ErrNotFound
	}
	var a Asset
	err := s.pool.QueryRow(ctx, `
		SELECT a.id::text, COALESCE(a.duration_seconds, 0), t.id::text, t.status::text, t.creator_id::text
		  FROM media_assets a
		  LEFT JOIN episodes e ON e.id = a.episode_id
		  JOIN titles t ON t.id = COALESCE(a.title_id, e.title_id)
		 WHERE a.status = 'ready' AND (a.title_id = $1::uuid OR a.episode_id = $1::uuid)`, playableID).
		Scan(&a.ID, &a.DurationSeconds, &a.Title.ID, &a.Title.Status, &a.Title.CreatorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT language, label, vtt_url FROM subtitles WHERE asset_id = $1::uuid ORDER BY language`, a.ID)
	if err != nil {
		return Asset{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var t SubtitleTrack
		var key string
		if err := rows.Scan(&t.Language, &t.Label, &key); err != nil {
			return Asset{}, err
		}
		t.File = strings.TrimPrefix(key, a.ID+"/") // stored as "<assetId>/subtitles/en.vtt"
		a.Subtitles = append(a.Subtitles, t)
	}
	return a, rows.Err()
}
