package playback

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// Errors returned by the service.
var (
	ErrNotFound    = errors.New("playback: not found")
	ErrNotEntitled = errors.New("playback: not entitled")
)

// Title is the part of a title the access decision needs.
type Title struct {
	ID        string
	Status    string // draft, processing, published, unpublished
	CreatorID string
}

// Entitlements decides whether a viewer may watch a title. Phase 4 supplies the real rules
// (subscriptions, rentals, free windows); until then PublishedOnly lets everyone watch published titles.
type Entitlements interface {
	CanWatch(ctx context.Context, userID string, title Title) (bool, error)
}

// PublishedOnly allows every signed-in viewer to watch published titles.
type PublishedOnly struct{}

// CanWatch implements Entitlements.
func (PublishedOnly) CanWatch(_ context.Context, _ string, t Title) (bool, error) {
	return t.Status == "published", nil
}

// Viewer is who is asking.
type Viewer struct {
	UserID string
	Admin  bool
}

// Asset is a ready media asset with what is needed to play it.
type Asset struct {
	ID              string
	Title           Title
	DurationSeconds int
	Subtitles       []SubtitleTrack
}

// SubtitleTrack is one subtitle file of an asset (a path relative to the asset prefix).
type SubtitleTrack struct {
	Language string
	Label    string
	File     string
}

// Store finds assets.
type Store interface {
	// Ready returns the ready asset whose title or episode id is playableID, or ErrNotFound.
	Ready(ctx context.Context, playableID string) (Asset, error)
}

// Subtitle is a subtitle track in a playback response.
type Subtitle struct {
	Language string `json:"language"`
	Label    string `json:"label"`
	URL      string `json:"url"`
}

// Playback is the answer to "how do I play this".
type Playback struct {
	PlayableID      string
	AssetID         string
	ManifestURL     string
	ExpiresAt       time.Time
	DurationSeconds int
	Subtitles       []Subtitle
	PosterURL       string
	ThumbnailsURL   string
}

// Service authorizes playback and signs URLs.
type Service struct {
	store      Store
	ent        Entitlements
	signer     Signer
	ttl        time.Duration
	publicBase string
	now        func() time.Time
	log        *slog.Logger
}

// NewService builds a Service. publicBase is where the public bucket is reachable (posters, thumbnails).
func NewService(store Store, ent Entitlements, signer Signer, ttl time.Duration, publicBase string, log *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, ent: ent, signer: signer, ttl: ttl, publicBase: strings.TrimRight(publicBase, "/"), now: now, log: log}
}

// Playback returns signed URLs for the viewer. Owners and admins may preview their own unpublished
// videos; everyone else needs a published title and the Entitlements hook's approval. A title the
// viewer may not even know exists is reported as not found, not as forbidden.
func (s *Service) Playback(ctx context.Context, v Viewer, playableID string) (Playback, error) {
	a, err := s.store.Ready(ctx, playableID)
	if err != nil {
		return Playback{}, err
	}
	privileged := v.Admin || (v.UserID != "" && v.UserID == a.Title.CreatorID)
	if !privileged {
		if a.Title.Status != "published" {
			return Playback{}, ErrNotFound
		}
		ok, err := s.ent.CanWatch(ctx, v.UserID, a.Title)
		if err != nil {
			return Playback{}, err
		}
		if !ok {
			return Playback{}, ErrNotEntitled
		}
	}
	exp := s.now().Add(s.ttl).UTC().Truncate(time.Second)
	out := Playback{
		PlayableID: playableID, AssetID: a.ID, ManifestURL: s.signer.URL(a.ID, exp, "master.m3u8"), ExpiresAt: exp,
		DurationSeconds: a.DurationSeconds,
		PosterURL:       s.publicBase + "/" + a.Title.ID + "/poster.jpg",
		ThumbnailsURL:   s.publicBase + "/" + a.Title.ID + "/thumbs.vtt",
	}
	for _, t := range a.Subtitles {
		out.Subtitles = append(out.Subtitles, Subtitle{Language: t.Language, Label: t.Label, URL: s.signer.URL(a.ID, exp, t.File)})
	}
	if out.Subtitles == nil {
		out.Subtitles = []Subtitle{}
	}
	s.log.InfoContext(ctx, "playback authorized", "user_id", v.UserID, "playable_id", playableID, "asset_id", a.ID, "preview", privileged && a.Title.Status != "published")
	return out, nil
}
