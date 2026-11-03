package creators

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func newSweeper(e *env) *Sweeper {
	return NewSweeper(e.store, e.objs, quietLogger(), func() time.Time { return e.clock },
		SweeperConfig{Bucket: "media-raw", AbandonAfter: 24 * time.Hour, Every: time.Hour, Batch: 100})
}

// startOn starts an upload for a fresh title of the given creator.
func (e *env) startOn(n int, creator auth2) Upload {
	e.t.Helper()
	id := uuidN(1000 + n)
	e.addTitle(id, creator.UserID, "movie", false)
	in := e.req(10 * mib)
	in.TitleID = id
	res, err := e.svc.Create(context.Background(), creator.principal(), in)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Upload
}

func TestSweeper_ExpiresAbandonedUploads(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(c *Config) { c.MaxActive = 50 })
	sw := newSweeper(e)
	old1, old2 := e.startOn(1, e.asCreator()), e.startOn(2, e.asCreator())
	e.clock = e.clock.Add(23 * time.Hour)
	fresh := e.startOn(3, e.asCreator()) // started 1 hour before the others expire
	done := e.startOn(4, e.asCreator())
	e.objs.putAllParts(done)
	if _, err := e.svc.Complete(ctx, e.creator, done.ID); err != nil {
		t.Fatal(err)
	}

	res, err := sw.RunOnce(ctx)
	if err != nil || !res.Ran || res.Expired != 0 {
		t.Fatalf("nothing is due yet: %+v, %v", res, err)
	}

	e.clock = e.clock.Add(90 * time.Minute) // old1 and old2 are now 24.5 h old; fresh is 1.5 h old
	res, err = sw.RunOnce(ctx)
	if err != nil || res.Expired != 2 || res.Failures != 0 {
		t.Fatalf("%+v, %v", res, err)
	}
	for _, id := range []string{old1.ID, old2.ID} {
		if got, _ := e.store.Get(ctx, id); got.Status != StatusExpired || got.S3UploadID != "" {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	if got, _ := e.store.Get(ctx, fresh.ID); got.Status != StatusInitiated {
		t.Fatalf("a recent upload must be left alone: %s", got.Status)
	}
	if got, _ := e.store.Get(ctx, done.ID); got.Status != StatusUploaded {
		t.Fatalf("a completed upload must be left alone: %s", got.Status)
	}
	if e.objs.openUploads() != 1 { // only fresh is still open in storage
		t.Fatalf("%d storage uploads still open", e.objs.openUploads())
	}
	if res, _ = sw.RunOnce(ctx); res.Expired != 0 || res.OrphansAborted != 0 {
		t.Fatalf("a second sweep has nothing to do: %+v", res)
	}
	// the title is free again, so the creator can start over
	if _, err := e.svc.Create(ctx, e.creator, CreateInput{TitleID: old1.TitleID, Filename: "again.mp4", SizeBytes: 10 * mib}); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestSweeper_HandlesMoreThanOneBatch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(c *Config) { c.MaxActive = 1000 })
	sw := newSweeper(e)
	sw.cfg.Batch = 100
	const n = 250
	for i := 0; i < n; i++ {
		e.startOn(i, e.asCreator())
	}
	e.clock = e.clock.Add(25 * time.Hour)
	res, err := sw.RunOnce(ctx)
	if err != nil || res.Expired != n || e.objs.openUploads() != 0 {
		t.Fatalf("expired %d of %d (%d open), %v", res.Expired, n, e.objs.openUploads(), err)
	}
}

func TestSweeper_AFailedAbortIsRetriedNextTime(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sw := newSweeper(e)
	u := e.startOn(1, e.asCreator())
	e.clock = e.clock.Add(25 * time.Hour)
	e.objs.failNext["AbortMultipartUpload"] = errors.New("storage unreachable")

	res, err := sw.RunOnce(ctx)
	if err != nil || res.Expired != 0 || res.Failures < 1 {
		t.Fatalf("%+v, %v", res, err)
	}
	if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusInitiated || got.S3UploadID == "" {
		t.Fatalf("the row must stay so that the storage upload is not forgotten: %+v", got)
	}
	res, err = sw.RunOnce(ctx)
	if err != nil || res.Expired != 1 {
		t.Fatalf("retry: %+v, %v", res, err)
	}
	if e.objs.openUploads() != 0 {
		t.Fatal("storage upload left behind")
	}
}

func TestSweeper_AbortsOrphanedStorageUploads(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sw := newSweeper(e)

	// storage uploads with no database row (the API crashed between the two writes)
	mk := func(key string, age time.Duration) string {
		id, _ := e.objs.CreateMultipartUpload(ctx, "media-raw", key, "video/mp4")
		e.objs.uploads[id].started = e.clock.Add(-age)
		return id
	}
	ancient := mk("ghost-1/source.mp4", 30*time.Hour)
	justPast := mk("ghost-2/source.mp4", 24*time.Hour+30*time.Minute) // inside the one-hour grace: kept
	recent := mk("ghost-3/source.mp4", 2*time.Hour)
	elsewhere, _ := e.objs.CreateMultipartUpload(ctx, "media-hls", "x", "")
	e.objs.uploads[elsewhere].started = e.clock.Add(-100 * time.Hour)

	res, err := sw.RunOnce(ctx)
	if err != nil || res.OrphansAborted != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
	for id, wantOpen := range map[string]bool{ancient: false, justPast: true, recent: true, elsewhere: true} {
		if _, open := e.objs.uploads[id]; open != wantOpen {
			t.Errorf("upload %s open=%v, want %v", id, open, wantOpen)
		}
	}
	e.clock = e.clock.Add(2 * time.Hour) // now justPast is outside the grace period too
	if res, _ = sw.RunOnce(ctx); res.OrphansAborted != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestSweeper_OnlyOneReplicaRuns(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sw := newSweeper(e)
	e.store.lockHeld = true // another API replica is sweeping
	res, err := sw.RunOnce(ctx)
	if err != nil || res.Ran {
		t.Fatalf("%+v, %v", res, err)
	}
	if e.objs.called("ListMultipartUploads") != 0 {
		t.Fatal("a replica that does not hold the lock must not touch storage")
	}
}

func TestSweeper_StorageListingFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sw := newSweeper(e)
	u := e.startOn(1, e.asCreator())
	e.clock = e.clock.Add(25 * time.Hour)
	e.objs.failNext["ListMultipartUploads"] = errors.New("boom")
	res, err := sw.RunOnce(ctx)
	if err == nil || res.Failures < 1 {
		t.Fatalf("a failing storage listing must be reported: %+v, %v", res, err)
	}
	if got, _ := e.store.Get(ctx, u.ID); got.Status != StatusExpired {
		t.Fatalf("the database pass still ran: %s", got.Status)
	}
}

func TestSweeper_RunStopsWhenCancelled(t *testing.T) {
	e := newEnv(t)
	sw := newSweeper(e)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sw.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}

// ---- the sweeper against the real database

func TestSweeper_WithPostgres(t *testing.T) {
	f := newPG(t)
	ctx := context.Background()
	clock := f.now
	objs := newFakeObjects(func() time.Time { return clock })
	title := f.title("movie", f.creator)
	uid, _ := objs.CreateMultipartUpload(ctx, "media-raw", "k/source.mp4", "video/mp4")
	f.upload(title, f.creator, func(u *Upload) { u.S3UploadID = uid; u.ObjectKey = "k/source.mp4" })
	fresh := f.title("movie", f.creator)
	freshUp := f.upload(fresh, f.creator, func(u *Upload) { u.ExpiresAt = f.now.Add(48 * time.Hour) })

	sw := NewSweeper(f.store, objs, quietLogger(), func() time.Time { return clock },
		SweeperConfig{Bucket: "media-raw", AbandonAfter: 24 * time.Hour, Every: time.Hour})
	clock = f.now.Add(25 * time.Hour)
	res, err := sw.RunOnce(ctx)
	if err != nil || !res.Ran || res.Expired != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
	var status string
	_ = f.pool.QueryRow(ctx, `SELECT status::text FROM uploads WHERE title_id = $1::uuid`, title).Scan(&status)
	if status != "expired" || len(objs.uploads) != 0 {
		t.Fatalf("status=%s open=%d", status, len(objs.uploads))
	}
	if got, _ := f.store.Get(ctx, freshUp.ID); got.Status != StatusInitiated {
		t.Fatalf("an upload with a later deadline was swept: %s", got.Status)
	}
}

var _ = fmt.Sprint
