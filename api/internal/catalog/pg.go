package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/api/internal/auth"
)

// PGStore is the PostgreSQL implementation of Store. All SQL is parameterized; only fixed SQL
// fragments are concatenated, never user input.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps a pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const summaryCols = `t.id::text, t.slug, t.type::text, t.name, t.release_year, t.maturity_rating,
	t.runtime_minutes, t.original_language, t.poster_url, t.status::text, t.published_at,
	COALESCE((SELECT array_agg(g.slug ORDER BY g.slug)
	            FROM title_genres tg JOIN genres g ON g.id = tg.genre_id
	           WHERE tg.title_id = t.id), '{}'::text[])`

func scanSummary(row pgx.Row, extra ...any) (TitleSummary, error) {
	var s TitleSummary
	dest := []any{&s.ID, &s.Slug, &s.Type, &s.Name, &s.ReleaseYear, &s.MaturityRating, &s.RuntimeMinutes,
		&s.OriginalLanguage, &s.PosterURL, &s.Status, &s.PublishedAt, &s.Genres}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return TitleSummary{}, err
	}
	if s.Genres == nil {
		s.Genres = []string{}
	}
	return s, nil
}

func summaries(rows pgx.Rows) ([]TitleSummary, error) {
	defer rows.Close()
	out := []TitleSummary{}
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// visibility returns the SQL predicate for the titles the viewer may see:
// viewers and anonymous visitors see published titles; creators also see their own titles in
// any state; admins see everything. arg binds a parameter and returns its placeholder.
func visibility(v Viewer, alias string, arg func(any) string) string {
	switch {
	case v.Authenticated() && v.Role == auth.RoleAdmin:
		return "true"
	case v.Authenticated() && v.Role == auth.RoleCreator:
		return "(" + alias + ".status = 'published' OR " + alias + ".creator_id = " + arg(v.UserID) + "::uuid)"
	default:
		return alias + ".status = 'published'"
	}
}

type argList struct{ args []any }

func (a *argList) add(v any) string {
	a.args = append(a.args, v)
	return "$" + strconv.Itoa(len(a.args))
}

// ListTitles returns up to p.Limit+1 rows so the caller can tell whether another page exists.
func (s *PGStore) ListTitles(ctx context.Context, v Viewer, p ListParams) ([]listRow, error) {
	var a argList
	var b strings.Builder
	b.WriteString("SELECT " + summaryCols)
	if p.Q != "" {
		b.WriteString(", ts_rank(t.search_vector, q.query)")
	} else {
		b.WriteString(", 0::real")
	}
	b.WriteString(" FROM titles t")
	if p.Q != "" {
		b.WriteString(", websearch_to_tsquery('simple', app_unaccent(" + a.add(p.Q) + ")) AS q(query)")
	}
	b.WriteString(" WHERE " + visibility(v, "t", a.add))
	if p.Q != "" {
		b.WriteString(" AND t.search_vector @@ q.query")
	}
	if p.Type != "" {
		b.WriteString(" AND t.type = " + a.add(p.Type) + "::title_type")
	}
	if p.Language != "" {
		b.WriteString(" AND t.original_language = " + a.add(p.Language))
	}
	if p.Genre != "" {
		b.WriteString(" AND EXISTS (SELECT 1 FROM title_genres tg2 JOIN genres g2 ON g2.id = tg2.genre_id" +
			" WHERE tg2.title_id = t.id AND g2.slug = " + a.add(p.Genre) + ")")
	}

	if c := p.After; c != nil {
		switch p.Sort {
		case SortNewest:
			// order: published_at DESC NULLS FIRST, id ASC
			id := a.add(c.ID) + "::uuid"
			if c.TS == nil {
				b.WriteString(" AND ((t.published_at IS NULL AND t.id > " + id + ") OR t.published_at IS NOT NULL)")
			} else {
				ts := a.add(*c.TS) + "::timestamptz"
				b.WriteString(" AND (t.published_at < " + ts + " OR (t.published_at = " + ts + " AND t.id > " + id + "))")
			}
		case SortName:
			b.WriteString(" AND (t.name, t.id) > (" + a.add(c.Name) + ", " + a.add(c.ID) + "::uuid)")
		case SortRelevance:
			r := a.add(c.Rank) + "::real"
			id := a.add(c.ID) + "::uuid"
			b.WriteString(" AND (ts_rank(t.search_vector, q.query) < " + r +
				" OR (ts_rank(t.search_vector, q.query) = " + r + " AND t.id > " + id + "))")
		}
	}

	switch p.Sort {
	case SortName:
		b.WriteString(" ORDER BY t.name ASC, t.id ASC")
	case SortRelevance:
		b.WriteString(" ORDER BY ts_rank(t.search_vector, q.query) DESC, t.id ASC")
	default:
		b.WriteString(" ORDER BY t.published_at DESC NULLS FIRST, t.id ASC")
	}
	b.WriteString(" LIMIT " + a.add(p.Limit+1))

	rows, err := s.pool.Query(ctx, b.String(), a.args...)
	if err != nil {
		return nil, fmt.Errorf("list titles: %w", err)
	}
	defer rows.Close()
	var out []listRow
	for rows.Next() {
		var r listRow
		sum, err := scanSummary(rows, &r.rank)
		if err != nil {
			return nil, fmt.Errorf("scan title: %w", err)
		}
		r.TitleSummary = sum
		out = append(out, r)
	}
	return out, rows.Err()
}

// TitleBySlug loads the full title page, or ErrNotFound when it does not exist or the viewer may
// not see it (the two are deliberately indistinguishable).
func (s *PGStore) TitleBySlug(ctx context.Context, v Viewer, slug string) (TitleDetail, error) {
	var a argList
	slugArg := a.add(slug)
	vis := visibility(v, "t", a.add)
	var d TitleDetail
	err := s.pool.QueryRow(ctx,
		`SELECT t.id::text, t.slug, t.type::text, t.name, t.release_year, t.maturity_rating, t.runtime_minutes,
		        t.original_language, t.poster_url, t.status::text, t.published_at, t.synopsis, t.backdrop_url
		   FROM titles t WHERE t.slug = `+slugArg+` AND `+vis, a.args...).
		Scan(&d.ID, &d.Slug, &d.Type, &d.Name, &d.ReleaseYear, &d.MaturityRating, &d.RuntimeMinutes,
			&d.OriginalLanguage, &d.PosterURL, &d.Status, &d.PublishedAt, &d.Synopsis, &d.BackdropURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return TitleDetail{}, ErrNotFound
	}
	if err != nil {
		return TitleDetail{}, fmt.Errorf("load title: %w", err)
	}
	d.Genres, d.Directors, d.Writers, d.Cast = []Genre{}, []Credit{}, []Credit{}, []Credit{}
	d.Subtitles, d.Seasons = []Subtitle{}, []Season{}

	if err := s.loadGenres(ctx, &d); err != nil {
		return TitleDetail{}, err
	}
	if err := s.loadCredits(ctx, &d); err != nil {
		return TitleDetail{}, err
	}
	subs, playable, err := s.loadMedia(ctx, d.ID)
	if err != nil {
		return TitleDetail{}, err
	}
	if d.Type == "movie" {
		d.Playable = playable.title
		d.Subtitles = subs.title
	} else if err := s.loadSeasons(ctx, &d, subs, playable); err != nil {
		return TitleDetail{}, err
	}
	if v.Authenticated() {
		st, err := s.viewerState(ctx, v.UserID, d.ID)
		if err != nil {
			return TitleDetail{}, err
		}
		d.Viewer = &st
	}
	return d, nil
}

func (s *PGStore) loadGenres(ctx context.Context, d *TitleDetail) error {
	rows, err := s.pool.Query(ctx,
		`SELECT g.slug, g.name FROM title_genres tg JOIN genres g ON g.id = tg.genre_id
		  WHERE tg.title_id = $1::uuid ORDER BY g.name`, d.ID)
	if err != nil {
		return fmt.Errorf("load genres: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g Genre
		if err := rows.Scan(&g.Slug, &g.Name); err != nil {
			return err
		}
		d.Genres = append(d.Genres, g)
	}
	return rows.Err()
}

func (s *PGStore) loadCredits(ctx context.Context, d *TitleDetail) error {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id::text, p.name, p.photo_url, tp.role::text, tp.character_name, tp.billing_order
		   FROM title_people tp JOIN people p ON p.id = tp.person_id
		  WHERE tp.title_id = $1::uuid ORDER BY tp.billing_order, p.name`, d.ID)
	if err != nil {
		return fmt.Errorf("load credits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c Credit
		var role string
		if err := rows.Scan(&c.PersonID, &c.Name, &c.PhotoURL, &role, &c.Character, &c.Order); err != nil {
			return err
		}
		switch role {
		case "director":
			d.Directors = append(d.Directors, c)
		case "writer":
			d.Writers = append(d.Writers, c)
		default:
			d.Cast = append(d.Cast, c)
		}
	}
	return rows.Err()
}

type mediaSubs struct {
	title   []Subtitle
	episode map[string][]Subtitle
}

type mediaPlayable struct {
	title   bool
	episode map[string]bool
}

func (s *PGStore) loadMedia(ctx context.Context, titleID string) (mediaSubs, mediaPlayable, error) {
	subs := mediaSubs{title: []Subtitle{}, episode: map[string][]Subtitle{}}
	play := mediaPlayable{episode: map[string]bool{}}
	rows, err := s.pool.Query(ctx,
		`SELECT a.title_id::text, a.episode_id::text, a.status::text, s.language, s.label, s.vtt_url
		   FROM media_assets a LEFT JOIN subtitles s ON s.asset_id = a.id
		  WHERE a.title_id = $1::uuid OR a.episode_id IN (SELECT id FROM episodes WHERE title_id = $1::uuid)
		  ORDER BY s.language`, titleID)
	if err != nil {
		return subs, play, fmt.Errorf("load media: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tid, eid, lang, label, url *string
		var status string
		if err := rows.Scan(&tid, &eid, &status, &lang, &label, &url); err != nil {
			return subs, play, err
		}
		ready := status == "ready"
		if tid != nil {
			play.title = play.title || ready
		} else if eid != nil {
			play.episode[*eid] = play.episode[*eid] || ready
		}
		if lang == nil || label == nil || url == nil {
			continue
		}
		sub := Subtitle{Language: *lang, Label: *label, URL: *url}
		if tid != nil {
			subs.title = append(subs.title, sub)
		} else if eid != nil {
			subs.episode[*eid] = append(subs.episode[*eid], sub)
		}
	}
	return subs, play, rows.Err()
}

func (s *PGStore) loadSeasons(ctx context.Context, d *TitleDetail, subs mediaSubs, play mediaPlayable) error {
	rows, err := s.pool.Query(ctx,
		`SELECT se.number, se.name, se.synopsis, e.id::text, e.number, e.name, e.synopsis, e.runtime_minutes, e.air_date::text
		   FROM seasons se LEFT JOIN episodes e ON e.season_id = se.id
		  WHERE se.title_id = $1::uuid ORDER BY se.number, e.number`, d.ID)
	if err != nil {
		return fmt.Errorf("load seasons: %w", err)
	}
	defer rows.Close()
	index := map[int]int{}
	for rows.Next() {
		var sNum int
		var sName, sSyn *string
		var eID, eName, eSyn, eAir *string
		var eNum, eRun *int
		if err := rows.Scan(&sNum, &sName, &sSyn, &eID, &eNum, &eName, &eSyn, &eRun, &eAir); err != nil {
			return err
		}
		i, ok := index[sNum]
		if !ok {
			d.Seasons = append(d.Seasons, Season{Number: sNum, Name: sName, Synopsis: sSyn, Episodes: []Episode{}})
			i = len(d.Seasons) - 1
			index[sNum] = i
		}
		if eID == nil {
			continue
		}
		ep := Episode{ID: *eID, Number: *eNum, Name: *eName, Synopsis: eSyn, RuntimeMinutes: eRun, AirDate: eAir,
			Playable: play.episode[*eID], Subtitles: subs.episode[*eID]}
		if ep.Subtitles == nil {
			ep.Subtitles = []Subtitle{}
		}
		d.Seasons[i].Episodes = append(d.Seasons[i].Episodes, ep)
	}
	return rows.Err()
}

func (s *PGStore) viewerState(ctx context.Context, userID, titleID string) (ViewerState, error) {
	st := ViewerState{Progress: []ProgressRef{}}
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM watchlist WHERE user_id = $1::uuid AND title_id = $2::uuid)`,
		userID, titleID).Scan(&st.InWatchlist); err != nil {
		return st, fmt.Errorf("load watchlist state: %w", err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT episode_id::text, position_seconds, duration_seconds, completed, updated_at
		   FROM watch_progress WHERE user_id = $1::uuid AND title_id = $2::uuid ORDER BY updated_at DESC`,
		userID, titleID)
	if err != nil {
		return st, fmt.Errorf("load progress: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p ProgressRef
		if err := rows.Scan(&p.EpisodeID, &p.PositionSeconds, &p.DurationSeconds, &p.Completed, &p.UpdatedAt); err != nil {
			return st, err
		}
		st.Progress = append(st.Progress, p)
	}
	return st, rows.Err()
}

// TitleType returns the type of a title the viewer may see.
func (s *PGStore) TitleType(ctx context.Context, v Viewer, titleID string) (string, error) {
	var a argList
	idArg := a.add(titleID)
	vis := visibility(v, "t", a.add)
	var typ string
	err := s.pool.QueryRow(ctx,
		`SELECT t.type::text FROM titles t WHERE t.id = `+idArg+`::uuid AND `+vis, a.args...).Scan(&typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return typ, err
}

// EpisodeBelongsTo reports whether the episode is part of the title.
func (s *PGStore) EpisodeBelongsTo(ctx context.Context, episodeID, titleID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM episodes WHERE id = $1::uuid AND title_id = $2::uuid)`,
		episodeID, titleID).Scan(&ok)
	return ok, err
}

// Genres lists every genre with its number of published titles.
func (s *PGStore) Genres(ctx context.Context) ([]GenreCount, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT g.slug, g.name, count(t.id)::int
		   FROM genres g
		   LEFT JOIN title_genres tg ON tg.genre_id = g.id
		   LEFT JOIN titles t ON t.id = tg.title_id AND t.status = 'published'
		  GROUP BY g.id ORDER BY g.name`)
	if err != nil {
		return nil, fmt.Errorf("genres: %w", err)
	}
	defer rows.Close()
	out := []GenreCount{}
	for rows.Next() {
		var g GenreCount
		if err := rows.Scan(&g.Slug, &g.Name, &g.TitleCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// HomePublic loads the rows that are the same for every visitor.
func (s *PGStore) HomePublic(ctx context.Context, since time.Time, perRow, languageRows int) (homePublic, error) {
	var h homePublic

	// Trending: distinct viewers with progress activity since `since`, newest as the tie-break and
	// as the fallback when little has been watched yet.
	rows, err := s.pool.Query(ctx,
		`SELECT `+summaryCols+`
		   FROM titles t
		   LEFT JOIN (SELECT title_id, count(DISTINCT user_id) AS viewers
		                FROM watch_progress WHERE updated_at > $1 GROUP BY title_id) v ON v.title_id = t.id
		  WHERE t.status = 'published'
		  ORDER BY COALESCE(v.viewers, 0) DESC, t.published_at DESC, t.id ASC
		  LIMIT $2`, since, perRow)
	if err != nil {
		return h, fmt.Errorf("trending: %w", err)
	}
	if h.Trending, err = summaries(rows); err != nil {
		return h, err
	}

	rows, err = s.pool.Query(ctx,
		`SELECT `+summaryCols+` FROM titles t WHERE t.status = 'published'
		  ORDER BY t.published_at DESC, t.id ASC LIMIT $1`, perRow)
	if err != nil {
		return h, fmt.Errorf("new releases: %w", err)
	}
	if h.New, err = summaries(rows); err != nil {
		return h, err
	}

	langs, err := s.pool.Query(ctx,
		`SELECT original_language FROM titles
		  WHERE status = 'published' AND original_language IS NOT NULL
		  GROUP BY original_language ORDER BY count(*) DESC, original_language ASC LIMIT $1`, languageRows)
	if err != nil {
		return h, fmt.Errorf("languages: %w", err)
	}
	var codes []string
	for langs.Next() {
		var c string
		if err := langs.Scan(&c); err != nil {
			langs.Close()
			return h, err
		}
		codes = append(codes, c)
	}
	langs.Close()
	if err := langs.Err(); err != nil {
		return h, err
	}
	for _, code := range codes {
		rows, err := s.pool.Query(ctx,
			`SELECT `+summaryCols+` FROM titles t
			  WHERE t.status = 'published' AND t.original_language = $1
			  ORDER BY t.published_at DESC, t.id ASC LIMIT $2`, code, perRow)
		if err != nil {
			return h, fmt.Errorf("language row: %w", err)
		}
		items, err := summaries(rows)
		if err != nil {
			return h, err
		}
		h.Languages = append(h.Languages, langRow{Code: code, Items: items})
	}
	return h, nil
}

// Watchlist lists the viewer's saved titles that they may still see, newest first.
func (s *PGStore) Watchlist(ctx context.Context, v Viewer, after *cursor, limit int) ([]watchRow, error) {
	var a argList
	user := a.add(v.UserID) + "::uuid"
	var b strings.Builder
	b.WriteString("SELECT " + summaryCols + ", w.added_at FROM watchlist w JOIN titles t ON t.id = w.title_id" +
		" WHERE w.user_id = " + user + " AND " + visibility(v, "t", a.add))
	if after != nil {
		ts := a.add(*after.TS) + "::timestamptz"
		id := a.add(after.ID) + "::uuid"
		b.WriteString(" AND (w.added_at < " + ts + " OR (w.added_at = " + ts + " AND t.id > " + id + "))")
	}
	b.WriteString(" ORDER BY w.added_at DESC, t.id ASC LIMIT " + a.add(limit+1))

	rows, err := s.pool.Query(ctx, b.String(), a.args...)
	if err != nil {
		return nil, fmt.Errorf("watchlist: %w", err)
	}
	defer rows.Close()
	var out []watchRow
	for rows.Next() {
		var w watchRow
		sum, err := scanSummary(rows, &w.AddedAt)
		if err != nil {
			return nil, err
		}
		w.TitleSummary = sum
		out = append(out, w)
	}
	return out, rows.Err()
}

// WatchlistAdd adds a title; adding an existing one is a no-op that reports added=false.
func (s *PGStore) WatchlistAdd(ctx context.Context, userID, titleID string) (time.Time, bool, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO watchlist (user_id, title_id)
		 SELECT $1::uuid, $2::uuid WHERE (SELECT count(*) FROM watchlist WHERE user_id = $1::uuid) < $3
		 ON CONFLICT DO NOTHING RETURNING added_at`, userID, titleID, MaxWatchlist).Scan(&at)
	if err == nil {
		return at, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, fmt.Errorf("watchlist add: %w", err)
	}
	// Nothing inserted: it was already there, or the list is full.
	err = s.pool.QueryRow(ctx,
		`SELECT added_at FROM watchlist WHERE user_id = $1::uuid AND title_id = $2::uuid`, userID, titleID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, ErrWatchlistFull
	}
	return at, false, err
}

// WatchlistRemove deletes an entry. Removing a missing entry is not an error.
func (s *PGStore) WatchlistRemove(ctx context.Context, userID, titleID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM watchlist WHERE user_id = $1::uuid AND title_id = $2::uuid`, userID, titleID)
	return err
}

const progressReturning = ` RETURNING position_seconds, duration_seconds, completed, updated_at`

const progressUpdate = ` DO UPDATE SET position_seconds = EXCLUDED.position_seconds,
	duration_seconds = EXCLUDED.duration_seconds, completed = EXCLUDED.completed, updated_at = now()
	WHERE (watch_progress.position_seconds, watch_progress.duration_seconds, watch_progress.completed)
	      IS DISTINCT FROM (EXCLUDED.position_seconds, EXCLUDED.duration_seconds, EXCLUDED.completed)`

// UpsertProgress stores a position. Sending the same values again changes nothing (not even
// updated_at), so retries and duplicate beacons are harmless.
func (s *PGStore) UpsertProgress(ctx context.Context, in ProgressInput) (Progress, error) {
	out := Progress{TitleID: in.TitleID, EpisodeID: in.EpisodeID}
	var err error
	if in.EpisodeID == nil {
		err = s.pool.QueryRow(ctx,
			`INSERT INTO watch_progress (user_id, title_id, episode_id, position_seconds, duration_seconds, completed)
			 VALUES ($1::uuid, $2::uuid, NULL, $3, $4, $5)
			 ON CONFLICT (user_id, title_id) WHERE episode_id IS NULL`+progressUpdate+progressReturning,
			in.UserID, in.TitleID, in.PositionSeconds, in.DurationSeconds, in.Completed).
			Scan(&out.PositionSeconds, &out.DurationSeconds, &out.Completed, &out.UpdatedAt)
	} else {
		err = s.pool.QueryRow(ctx,
			`INSERT INTO watch_progress (user_id, title_id, episode_id, position_seconds, duration_seconds, completed)
			 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6)
			 ON CONFLICT (user_id, episode_id) WHERE episode_id IS NOT NULL`+progressUpdate+progressReturning,
			in.UserID, in.TitleID, *in.EpisodeID, in.PositionSeconds, in.DurationSeconds, in.Completed).
			Scan(&out.PositionSeconds, &out.DurationSeconds, &out.Completed, &out.UpdatedAt)
	}
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return out, nil
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return Progress{}, ErrInvalidProgress // episode does not belong to the title
	case !errors.Is(err, pgx.ErrNoRows):
		return Progress{}, fmt.Errorf("upsert progress: %w", err)
	}
	// ON CONFLICT ... WHERE matched nothing: identical values were already stored. Return them.
	q := `SELECT position_seconds, duration_seconds, completed, updated_at FROM watch_progress
	       WHERE user_id = $1::uuid AND `
	if in.EpisodeID == nil {
		err = s.pool.QueryRow(ctx, q+`title_id = $2::uuid AND episode_id IS NULL`, in.UserID, in.TitleID).
			Scan(&out.PositionSeconds, &out.DurationSeconds, &out.Completed, &out.UpdatedAt)
	} else {
		err = s.pool.QueryRow(ctx, q+`episode_id = $2::uuid`, in.UserID, *in.EpisodeID).
			Scan(&out.PositionSeconds, &out.DurationSeconds, &out.Completed, &out.UpdatedAt)
	}
	return out, err
}

// ContinueWatching lists unfinished titles, most recently watched first. A series appears once,
// at the episode the viewer touched last.
func (s *PGStore) ContinueWatching(ctx context.Context, v Viewer, limit int) ([]ContinueItem, error) {
	var a argList
	user := a.add(v.UserID) + "::uuid"
	vis := visibility(v, "t", a.add)
	lim := a.add(limit)
	rows, err := s.pool.Query(ctx,
		`SELECT `+summaryCols+`, x.position_seconds, x.duration_seconds, x.updated_at,
		        x.episode_id::text, se.number, e.number, e.name
		   FROM (SELECT DISTINCT ON (p.title_id) p.title_id, p.episode_id, p.position_seconds, p.duration_seconds, p.updated_at
		           FROM watch_progress p
		          WHERE p.user_id = `+user+` AND NOT p.completed
		          ORDER BY p.title_id, p.updated_at DESC) x
		   JOIN titles t ON t.id = x.title_id
		   LEFT JOIN episodes e ON e.id = x.episode_id
		   LEFT JOIN seasons se ON se.id = e.season_id
		  WHERE `+vis+`
		  ORDER BY x.updated_at DESC, t.id ASC
		  LIMIT `+lim, a.args...)
	if err != nil {
		return nil, fmt.Errorf("continue watching: %w", err)
	}
	defer rows.Close()
	out := []ContinueItem{}
	for rows.Next() {
		var it ContinueItem
		var epID, epName *string
		var seasonNum, epNum *int
		sum, err := scanSummary(rows, &it.PositionSeconds, &it.DurationSeconds, &it.UpdatedAt, &epID, &seasonNum, &epNum, &epName)
		if err != nil {
			return nil, err
		}
		it.Title = sum
		if epID != nil && seasonNum != nil && epNum != nil && epName != nil {
			it.Episode = &EpisodeRef{ID: *epID, SeasonNumber: *seasonNum, Number: *epNum, Name: *epName}
		}
		it.ProgressPercent = it.PositionSeconds * 100 / it.DurationSeconds
		out = append(out, it)
	}
	return out, rows.Err()
}
