package creators

import (
	"context"
	"errors"
	"net/http"

	"streamafrica/api/internal/auth"
	"streamafrica/api/internal/platform/s3"
)

var errAssetNotFound = problem(http.StatusNotFound, "asset_not_found", "video not found")

// Snapshot is Get without the storage call: the state and job of an upload. It is cheap enough to poll.
func (s *Service) Snapshot(ctx context.Context, p auth.Principal, id string) (Details, error) {
	u, err := s.getOwned(ctx, p, id)
	if err != nil {
		return Details{}, err
	}
	d := Details{Upload: u}
	if j, err := s.store.JobFor(ctx, u.ID); err == nil {
		d.Job = &j
	} else if !errors.Is(err, ErrNotFound) {
		return Details{}, err
	}
	return d, nil
}

// Retry gives an upload that failed for good (the worker ran out of attempts) one more go, without
// uploading again. A file that was refused as unacceptable cannot be retried: the creator must send a
// different file.
func (s *Service) Retry(ctx context.Context, p auth.Principal, id string) (Details, error) {
	u, err := s.getOwned(ctx, p, id)
	if err != nil {
		return Details{}, err
	}
	if u.Status == StatusUploaded || u.Status == StatusProcessing {
		return s.Snapshot(ctx, p, id) // already queued or running: nothing to do, and calling twice is harmless
	}
	if u.Status != StatusRejected || u.RejectCode != "processing_failed" {
		return Details{}, problem(http.StatusConflict, "not_retryable",
			"only an upload that failed during processing can be retried; upload the video again instead")
	}
	if _, err := s.objects.Head(ctx, u.Bucket, u.ObjectKey); err != nil {
		if s3.IsNotFound(err) {
			return Details{}, problem(http.StatusConflict, "source_missing", "the original file is no longer stored; upload the video again")
		}
		return Details{}, storageUnavailable(err)
	}
	if _, err := s.store.Requeue(ctx, id, s.now()); err != nil {
		switch {
		case errors.Is(err, ErrTitleBusy):
			return Details{}, problem(http.StatusConflict, "upload_in_progress", "this title already has another unfinished upload")
		case errors.Is(err, ErrConflict):
			return s.Snapshot(ctx, p, id) // someone else just retried it
		}
		return Details{}, err
	}
	s.logger.InfoContext(ctx, "upload retried", "upload_id", id, "user_id", p.UserID)
	return s.Snapshot(ctx, p, id)
}

func (s *Service) getAsset(ctx context.Context, p auth.Principal, id string) (AssetInfo, error) {
	a, err := s.store.Asset(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return AssetInfo{}, errAssetNotFound
	}
	if err != nil {
		return AssetInfo{}, err
	}
	if p.Role != auth.RoleAdmin && a.CreatorID != p.UserID {
		return AssetInfo{}, errAssetNotFound // never reveal that someone else's video exists
	}
	return a, nil
}

// UnpublishAsset hides the video's title from viewers. The files stay, so it can be published again.
// It is safe to repeat.
func (s *Service) UnpublishAsset(ctx context.Context, p auth.Principal, assetID string) (AssetInfo, error) {
	a, err := s.getAsset(ctx, p, assetID)
	if err != nil {
		return AssetInfo{}, err
	}
	if a.TitleStatus == "published" {
		if err := s.store.UnpublishTitle(ctx, a.TitleID, s.now()); err != nil {
			return AssetInfo{}, err
		}
		a.TitleStatus = "unpublished"
		s.logger.InfoContext(ctx, "title unpublished", "asset_id", a.ID, "title_id", a.TitleID, "user_id", p.UserID)
	}
	return a, nil
}

// DeleteAsset removes a video for good: the title is unpublished first, then the HLS output, the poster
// and thumbnails and the original upload are deleted from storage, and finally the database records.
// The order makes a failure at any step safe to retry: storage deletion is idempotent, and the database
// rows go last, so a half-deleted video is never left looking playable.
func (s *Service) DeleteAsset(ctx context.Context, p auth.Principal, assetID string) error {
	a, err := s.getAsset(ctx, p, assetID)
	if err != nil {
		return err
	}
	if a.Status == "processing" {
		return problem(http.StatusConflict, "asset_busy", "this video is still being processed; wait for it to finish, then delete it")
	}
	if err := s.store.UnpublishTitle(ctx, a.TitleID, s.now()); err != nil {
		return err
	}
	uploads, err := s.store.ReadyUploadIDs(ctx, a.TitleID)
	if err != nil {
		return err
	}
	prefixes := []struct{ bucket, prefix string }{{s.cfg.HLSBucket, a.ID + "/"}, {s.cfg.PublicBucket, a.TitleID + "/"}}
	for _, id := range uploads {
		prefixes = append(prefixes, struct{ bucket, prefix string }{s.cfg.RawBucket, id + "/"})
	}
	removed := 0
	for _, pf := range prefixes {
		n, err := s.objects.DeletePrefix(ctx, pf.bucket, pf.prefix)
		if err != nil {
			return storageUnavailable(err)
		}
		removed += n
	}
	if err := s.store.DeleteAsset(ctx, a.ID, uploads, s.now()); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "video deleted", "asset_id", a.ID, "title_id", a.TitleID, "user_id", p.UserID, "objects_removed", removed)
	return nil
}
