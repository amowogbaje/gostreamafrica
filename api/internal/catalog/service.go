package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"
)

// Config holds the catalog's tunables.
type Config struct {
	HomeTTL        time.Duration
	GenresTTL      time.Duration
	TrendingWindow time.Duration // how far back "recent views" count
	RowSize        int           // titles per home row
	LanguageRows   int           // how many per-language rows
}

func (c *Config) defaults() {
	if c.HomeTTL <= 0 {
		c.HomeTTL = 5 * time.Minute
	}
	if c.GenresTTL <= 0 {
		c.GenresTTL = 10 * time.Minute
	}
	if c.TrendingWindow <= 0 {
		c.TrendingWindow = 7 * 24 * time.Hour
	}
	if c.RowSize <= 0 {
		c.RowSize = 12
	}
	if c.LanguageRows <= 0 {
		c.LanguageRows = 5
	}
}

// Service holds the catalog rules.
type Service struct {
	store  Store
	cache  Cache
	logger *slog.Logger
	now    func() time.Time
	cfg    Config
	group  singleflight.Group
}

// NewService builds the service.
func NewService(store Store, cache Cache, logger *slog.Logger, now func() time.Time, cfg Config) *Service {
	cfg.defaults()
	return &Service{store: store, cache: cache, logger: logger, now: now, cfg: cfg}
}

// TitlesChanged must be called after anything that changes what viewers see (a title created,
// edited, published or unpublished, its genres, its artwork). It invalidates the cached home page
// and genre list. Write paths in the creators and admin modules call it.
func (s *Service) TitlesChanged(ctx context.Context) error { return s.cache.Invalidate(ctx) }

// List returns one page of titles the viewer may see.
func (s *Service) List(ctx context.Context, v Viewer, p ListParams) (Page, error) {
	rows, err := s.store.ListTitles(ctx, v, p)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: make([]TitleSummary, 0, min(len(rows), p.Limit))}
	more := len(rows) > p.Limit
	if more {
		rows = rows[:p.Limit]
	}
	for _, r := range rows {
		page.Items = append(page.Items, r.TitleSummary)
	}
	if more {
		last := rows[len(rows)-1]
		c := cursor{Kind: "titles", Sort: p.Sort, ID: last.ID}
		switch p.Sort {
		case SortName:
			c.Name = last.Name
		case SortRelevance:
			c.Rank = last.rank
		default:
			c.TS = last.PublishedAt
		}
		enc := c.encode()
		page.NextCursor = &enc
	}
	return page, nil
}

// Detail returns the title page, or ErrNotFound.
func (s *Service) Detail(ctx context.Context, v Viewer, slug string) (TitleDetail, error) {
	return s.store.TitleBySlug(ctx, v, slug)
}

// cached returns the rendered bytes for name, building and storing them on a miss. A cache outage
// never fails a request: it logs and serves straight from the database.
func (s *Service) cached(ctx context.Context, name string, ttl time.Duration, build func(context.Context) ([]byte, error)) ([]byte, error) {
	val, epoch, hit, err := s.cache.Load(ctx, name)
	if err != nil {
		s.logger.WarnContext(ctx, "catalog cache unavailable, serving from database", "name", name, "error", err.Error())
		return build(ctx)
	}
	if hit {
		return val, nil
	}
	res, err, _ := s.group.Do(name+":"+strconv.FormatInt(epoch, 10), func() (any, error) {
		body, err := build(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		if err := s.cache.Store(context.WithoutCancel(ctx), epoch, name, body, ttl); err != nil {
			s.logger.WarnContext(ctx, "catalog cache store failed", "name", name, "error", err.Error())
		}
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	return res.([]byte), nil
}

// GenresJSON returns the rendered {"items":[...]} body of GET /v1/genres.
func (s *Service) GenresJSON(ctx context.Context) ([]byte, error) {
	return s.cached(ctx, "genres", s.cfg.GenresTTL, func(ctx context.Context) ([]byte, error) {
		gs, err := s.store.Genres(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Items []GenreCount `json:"items"`
		}{gs})
	})
}

var languageNames = map[string]string{
	"en": "English", "yo": "Yoruba", "ig": "Igbo", "ha": "Hausa", "pcm": "Nigerian Pidgin",
	"tw": "Twi", "kri": "Krio", "fr": "French", "sw": "Swahili", "am": "Amharic", "zu": "Zulu",
}

func languageName(code string) string {
	if n, ok := languageNames[code]; ok {
		return n
	}
	return code
}

func (s *Service) publicHome(ctx context.Context) ([]byte, error) {
	return s.cached(ctx, "home", s.cfg.HomeTTL, func(ctx context.Context) ([]byte, error) {
		h, err := s.store.HomePublic(ctx, s.now().Add(-s.cfg.TrendingWindow), s.cfg.RowSize, s.cfg.LanguageRows)
		if err != nil {
			return nil, err
		}
		home := Home{Rows: []HomeRow{}}
		add := func(row HomeRow) {
			if len(row.Items) > 0 {
				home.Rows = append(home.Rows, row)
			}
		}
		add(HomeRow{Key: "trending", Kind: "titles", Title: "Trending Now", Items: h.Trending})
		add(HomeRow{Key: "new-releases", Kind: "titles", Title: "New Releases", Items: h.New})
		for _, l := range h.Languages {
			code := l.Code
			add(HomeRow{Key: "language-" + code, Kind: "titles", Title: languageName(code), Language: &code, Items: l.Items})
		}
		return json.Marshal(home)
	})
}

// HomeJSON returns the rendered body of GET /v1/home. The public rows come from the cache; for a
// signed-in viewer a live "Continue Watching" row is placed first. shared reports whether the
// body is identical for every visitor (and so may be cached by shared caches).
func (s *Service) HomeJSON(ctx context.Context, v Viewer) (body []byte, shared bool, err error) {
	pub, err := s.publicHome(ctx)
	if err != nil {
		return nil, false, err
	}
	if !v.Authenticated() {
		return pub, true, nil
	}
	items, err := s.store.ContinueWatching(ctx, v, s.cfg.RowSize)
	if err != nil {
		return nil, false, err
	}
	if len(items) == 0 {
		return pub, false, nil
	}
	var home Home
	if err := json.Unmarshal(pub, &home); err != nil {
		return nil, false, fmt.Errorf("decode cached home: %w", err)
	}
	home.Rows = append([]HomeRow{{Key: "continue-watching", Kind: "continue", Title: "Continue Watching", Continue: items}}, home.Rows...)
	body, err = json.Marshal(home)
	return body, false, err
}

// Watchlist returns one page of the viewer's watchlist.
func (s *Service) Watchlist(ctx context.Context, v Viewer, after *cursor, limit int) (WatchlistPage, error) {
	rows, err := s.store.Watchlist(ctx, v, after, limit)
	if err != nil {
		return WatchlistPage{}, err
	}
	page := WatchlistPage{Items: make([]WatchlistItem, 0, min(len(rows), limit))}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	for _, r := range rows {
		page.Items = append(page.Items, r.WatchlistItem)
	}
	if more {
		last := rows[len(rows)-1]
		at := last.AddedAt
		enc := cursor{Kind: "watchlist", TS: &at, ID: last.ID}.encode()
		page.NextCursor = &enc
	}
	return page, nil
}

// WatchlistAdd saves a title the viewer may see. created is false when it was already saved.
func (s *Service) WatchlistAdd(ctx context.Context, v Viewer, titleID string) (addedAt time.Time, created bool, err error) {
	if _, err := s.store.TitleType(ctx, v, titleID); err != nil {
		return time.Time{}, false, err
	}
	return s.store.WatchlistAdd(ctx, v.UserID, titleID)
}

// WatchlistRemove removes a title from the viewer's watchlist (idempotent).
func (s *Service) WatchlistRemove(ctx context.Context, v Viewer, titleID string) error {
	return s.store.WatchlistRemove(ctx, v.UserID, titleID)
}

// ProgressUpdate is a client's report of where playback is.
type ProgressUpdate struct {
	TitleID         string
	EpisodeID       *string
	PositionSeconds int
	DurationSeconds int
}

// completedPercent is the share of the runtime after which a playable counts as watched.
const completedPercent = 95

// SaveProgress validates the target, clamps the position to the duration, marks the item
// completed at 95 percent and upserts it. Repeating the same update changes nothing.
func (s *Service) SaveProgress(ctx context.Context, v Viewer, u ProgressUpdate) (Progress, error) {
	typ, err := s.store.TitleType(ctx, v, u.TitleID)
	if err != nil {
		return Progress{}, err
	}
	switch {
	case typ == "movie" && u.EpisodeID != nil, typ == "series" && u.EpisodeID == nil:
		return Progress{}, ErrInvalidProgress
	case u.EpisodeID != nil:
		ok, err := s.store.EpisodeBelongsTo(ctx, *u.EpisodeID, u.TitleID)
		if err != nil {
			return Progress{}, err
		}
		if !ok {
			return Progress{}, ErrInvalidProgress
		}
	}
	pos := min(max(u.PositionSeconds, 0), u.DurationSeconds)
	return s.store.UpsertProgress(ctx, ProgressInput{
		UserID: v.UserID, TitleID: u.TitleID, EpisodeID: u.EpisodeID,
		PositionSeconds: pos, DurationSeconds: u.DurationSeconds,
		Completed: pos*100 >= u.DurationSeconds*completedPercent,
	})
}

// ContinueWatching lists the viewer's unfinished titles.
func (s *Service) ContinueWatching(ctx context.Context, v Viewer, limit int) (ContinueList, error) {
	items, err := s.store.ContinueWatching(ctx, v, limit)
	return ContinueList{Items: items}, err
}
