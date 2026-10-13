// Package catalog owns what viewers browse: titles, genres, the home page, the watchlist and
// watch progress. Watchlist and progress live here (not in a separate module) because every
// read joins them to titles and applies the same visibility rules. They move to the playback
// module when it gains signed URLs.
package catalog

import (
	"errors"
	"time"

	"streamafrica/api/internal/auth"
)

// Viewer is who is asking. The zero value is an anonymous visitor.
type Viewer struct {
	UserID string
	Role   auth.Role
}

// Authenticated reports whether the request carried a valid access token.
func (v Viewer) Authenticated() bool { return v.UserID != "" }

// TitleCore holds the fields shared by list and detail responses.
type TitleCore struct {
	ID               string     `json:"id"`
	Slug             string     `json:"slug"`
	Type             string     `json:"type"`
	Name             string     `json:"name"`
	ReleaseYear      *int       `json:"release_year"`
	MaturityRating   *string    `json:"maturity_rating"`
	RuntimeMinutes   *int       `json:"runtime_minutes"`
	OriginalLanguage *string    `json:"original_language"`
	PosterURL        *string    `json:"poster_url"`
	Status           string     `json:"status"`
	PublishedAt      *time.Time `json:"published_at"`
}

// TitleSummary is the lean card used in lists, rows and the watchlist.
type TitleSummary struct {
	TitleCore
	Genres []string `json:"genres"` // genre slugs
}

// Genre is a genre as shown on a detail page.
type Genre struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// GenreCount is a genre with the number of published titles in it.
type GenreCount struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	TitleCount int    `json:"title_count"`
}

// Credit is one cast or crew entry.
type Credit struct {
	PersonID  string  `json:"person_id"`
	Name      string  `json:"name"`
	PhotoURL  *string `json:"photo_url"`
	Character *string `json:"character"` // cast only
	Order     int     `json:"order"`
}

// Subtitle is one subtitle track of a playable.
type Subtitle struct {
	Language string `json:"language"`
	Label    string `json:"label"`
	URL      string `json:"url"`
}

// Episode is one episode of a season.
type Episode struct {
	ID             string     `json:"id"`
	Number         int        `json:"number"`
	Name           string     `json:"name"`
	Synopsis       *string    `json:"synopsis"`
	RuntimeMinutes *int       `json:"runtime_minutes"`
	AirDate        *string    `json:"air_date"` // YYYY-MM-DD
	Playable       bool       `json:"playable"`
	Subtitles      []Subtitle `json:"subtitles"`
}

// Season groups episodes.
type Season struct {
	Number   int       `json:"number"`
	Name     *string   `json:"name"`
	Synopsis *string   `json:"synopsis"`
	Episodes []Episode `json:"episodes"`
}

// ProgressRef is the viewer's saved position in one playable of a title.
type ProgressRef struct {
	EpisodeID       *string   `json:"episode_id"`
	PositionSeconds int       `json:"position_seconds"`
	DurationSeconds int       `json:"duration_seconds"`
	Completed       bool      `json:"completed"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ViewerState is included on the detail of a title for signed-in viewers.
type ViewerState struct {
	InWatchlist bool          `json:"in_watchlist"`
	Progress    []ProgressRef `json:"progress"`
}

// TitleDetail is the full title page.
type TitleDetail struct {
	TitleCore
	Synopsis    *string      `json:"synopsis"`
	BackdropURL *string      `json:"backdrop_url"`
	Genres      []Genre      `json:"genres"`
	Directors   []Credit     `json:"directors"`
	Writers     []Credit     `json:"writers"`
	Cast        []Credit     `json:"cast"`
	Playable    bool         `json:"playable"`  // movies: an HLS asset is ready
	Subtitles   []Subtitle   `json:"subtitles"` // movies
	Seasons     []Season     `json:"seasons"`   // series
	Viewer      *ViewerState `json:"viewer,omitempty"`
}

// Page is a page of title summaries with the cursor for the next page (null on the last page).
type Page struct {
	Items      []TitleSummary `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

// WatchlistItem is a watchlist entry.
type WatchlistItem struct {
	TitleSummary
	AddedAt time.Time `json:"added_at"`
}

// WatchlistPage is a page of the watchlist.
type WatchlistPage struct {
	Items      []WatchlistItem `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

// EpisodeRef identifies the episode a continue-watching item resumes.
type EpisodeRef struct {
	ID           string `json:"id"`
	SeasonNumber int    `json:"season_number"`
	Number       int    `json:"number"`
	Name         string `json:"name"`
}

// ContinueItem is one "continue watching" entry.
type ContinueItem struct {
	Title           TitleSummary `json:"title"`
	Episode         *EpisodeRef  `json:"episode"`
	PositionSeconds int          `json:"position_seconds"`
	DurationSeconds int          `json:"duration_seconds"`
	ProgressPercent int          `json:"progress_percent"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

// ContinueList is the response of GET /v1/me/continue-watching.
type ContinueList struct {
	Items []ContinueItem `json:"items"`
}

// Progress is the stored result of a progress update.
type Progress struct {
	TitleID         string    `json:"title_id"`
	EpisodeID       *string   `json:"episode_id"`
	PositionSeconds int       `json:"position_seconds"`
	DurationSeconds int       `json:"duration_seconds"`
	Completed       bool      `json:"completed"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// HomeRow is one horizontal row on the home page. Kind says which list is filled: "titles" rows
// use Items, the "continue" row uses Continue.
type HomeRow struct {
	Key      string         `json:"key"`
	Kind     string         `json:"kind"`
	Title    string         `json:"title"`
	Language *string        `json:"language,omitempty"`
	Items    []TitleSummary `json:"items,omitempty"`
	Continue []ContinueItem `json:"continue,omitempty"`
}

// Home is the response of GET /v1/home.
type Home struct {
	Rows []HomeRow `json:"rows"`
}

// Errors.
var (
	// ErrNotFound means the title (or episode) does not exist or is not visible to the viewer.
	ErrNotFound = errors.New("not found")
	// ErrWatchlistFull means the watchlist reached its size limit.
	ErrWatchlistFull = errors.New("watchlist is full")
	// ErrInvalidProgress means the episode does not fit the title (missing for a series,
	// present for a movie, or from another title).
	ErrInvalidProgress = errors.New("invalid progress target")
)

// MaxWatchlist is the largest watchlist a user may keep.
const MaxWatchlist = 500
