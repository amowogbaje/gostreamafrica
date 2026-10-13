package catalog

import (
	"context"
	"time"
)

// ListParams are the validated parameters of a title list query.
type ListParams struct {
	Type     string // "movie", "series" or ""
	Genre    string // genre slug or ""
	Language string // language code or ""
	Q        string // search text or ""
	Sort     Sort
	After    *cursor
	Limit    int
}

// listRow is a summary plus the sort keys needed to build the next cursor.
type listRow struct {
	TitleSummary
	rank float32
}

// watchRow is a watchlist entry plus its sort key.
type watchRow struct {
	WatchlistItem
}

// langRow is the newest titles of one language.
type langRow struct {
	Code  string
	Items []TitleSummary
}

// homePublic is the part of the home page that is the same for everyone (and cacheable).
type homePublic struct {
	Trending  []TitleSummary
	New       []TitleSummary
	Languages []langRow
}

// ProgressInput is a validated progress update.
type ProgressInput struct {
	UserID          string
	TitleID         string
	EpisodeID       *string
	PositionSeconds int
	DurationSeconds int
	Completed       bool
}

// Store is the persistence the catalog needs. PGStore is the PostgreSQL implementation.
type Store interface {
	ListTitles(ctx context.Context, v Viewer, p ListParams) ([]listRow, error)
	TitleBySlug(ctx context.Context, v Viewer, slug string) (TitleDetail, error)
	// TitleType returns "movie" or "series" for a title the viewer may see, else ErrNotFound.
	TitleType(ctx context.Context, v Viewer, titleID string) (string, error)
	EpisodeBelongsTo(ctx context.Context, episodeID, titleID string) (bool, error)
	Genres(ctx context.Context) ([]GenreCount, error)
	HomePublic(ctx context.Context, since time.Time, perRow, languageRows int) (homePublic, error)

	Watchlist(ctx context.Context, v Viewer, after *cursor, limit int) ([]watchRow, error)
	// WatchlistAdd returns added=false when the title was already on the list.
	WatchlistAdd(ctx context.Context, userID, titleID string) (addedAt time.Time, added bool, err error)
	WatchlistRemove(ctx context.Context, userID, titleID string) error

	UpsertProgress(ctx context.Context, in ProgressInput) (Progress, error)
	ContinueWatching(ctx context.Context, v Viewer, limit int) ([]ContinueItem, error)
}
