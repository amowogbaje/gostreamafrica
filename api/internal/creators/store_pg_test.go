package creators

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/api/internal/testdb"
)

// pgFx is a real database with every migration applied (skipped without TEST_DATABASE_URL).
type pgFx struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	store   *PGStore
	creator string
	other   string
	now     time.Time
}

func newPG(t *testing.T) *pgFx {
	t.Helper()
	pool := testdb.Pool(t)
	f := &pgFx{t: t, ctx: context.Background(), pool: pool, store: NewPGStore(pool), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	f.creator, f.other = f.user("creator"), f.user("creator")
	return f
}

func (f *pgFx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%v\n%s", err, sql)
	}
}

func hexN(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *pgFx) user(role string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO users (email, password_hash, role) VALUES ($1::citext, '!', $2::user_role) RETURNING id::text`,
		role+"-"+hexN(5)+"@example.com", role).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO profiles (user_id, display_name) VALUES ($1::uuid, $2)`, id, role)
	return id
}

func (f *pgFx) title(typ, creator string) string {
	f.t.Helper()
	var id string
	slug := "t-" + hexN(4)
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO titles (type, slug, name, original_language, status, creator_id)
		VALUES ($1::title_type, $2, $2, 'en', 'draft', $3::uuid) RETURNING id::text`, typ, slug, creator).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *pgFx) upload(title, creator string, mod func(*Upload)) Upload {
	f.t.Helper()
	id, _ := newUUID()
	u := Upload{ID: id, CreatorID: creator, TitleID: title, Filename: "film.mp4", ContentType: "video/mp4", Container: "mp4",
		SizeBytes: 200 << 20, PartSizeBytes: 64 << 20, PartCount: 4, Bucket: "media-raw", ObjectKey: id + "/source.mp4",
		S3UploadID: "UP-" + hexN(3), Status: StatusInitiated, ExpiresAt: f.now.Add(24 * time.Hour), CreatedAt: f.now, UpdatedAt: f.now}
	if mod != nil {
		mod(&u)
	}
	if err := f.store.Create(f.ctx, u); err != nil {
		f.t.Fatalf("create upload: %v", err)
	}
	return u
}

func TestPG_CreateAndGet(t *testing.T) {
	f := newPG(t)
	u := f.upload(f.title("movie", f.creator), f.creator, nil)
	got, err := f.store.Get(f.ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	u.CompletedAt = nil
	if got.ID != u.ID || got.CreatorID != u.CreatorID || got.TitleID != u.TitleID || got.Filename != u.Filename ||
		got.ContentType != u.ContentType || got.Container != u.Container || got.SizeBytes != u.SizeBytes ||
		got.PartSizeBytes != u.PartSizeBytes || got.PartCount != u.PartCount || got.Bucket != u.Bucket ||
		got.ObjectKey != u.ObjectKey || got.S3UploadID != u.S3UploadID || got.Status != StatusInitiated ||
		!got.ExpiresAt.Equal(u.ExpiresAt) || !got.CreatedAt.Equal(u.CreatedAt) || got.CompletedAt != nil || got.RejectCode != "" {
		t.Fatalf("round trip lost data:\n got  %+v\n want %+v", got, u)
	}
	for _, id := range []string{"not-a-uuid", "", "00000000-0000-4000-8000-000000000000"} {
		if _, err := f.store.Get(f.ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestPG_Title(t *testing.T) {
	f := newPG(t)
	movie, series := f.title("movie", f.creator), f.title("series", f.creator)
	got, err := f.store.Title(f.ctx, movie)
	if err != nil || got.CreatorID != f.creator || got.Type != "movie" || got.HasAsset {
		t.Fatalf("%+v, %v", got, err)
	}
	if got, _ := f.store.Title(f.ctx, series); got.Type != "series" {
		t.Fatalf("%+v", got)
	}
	f.exec(`INSERT INTO media_assets (title_id) VALUES ($1::uuid)`, movie) // status 'processing'
	if got, _ := f.store.Title(f.ctx, movie); got.HasAsset {
		t.Fatal("an asset that is still processing (or failed) must not block a new upload")
	}
	f.exec(`UPDATE media_assets SET status = 'ready', hls_manifest_key = 'x/master.m3u8', duration_seconds = 60 WHERE title_id = $1::uuid`, movie)
	if got, _ := f.store.Title(f.ctx, movie); !got.HasAsset {
		t.Fatal("HasAsset must be true once the asset is ready")
	}
	for _, id := range []string{"garbage", "00000000-0000-4000-8000-000000000000"} {
		if _, err := f.store.Title(f.ctx, id); !errors.Is(err, ErrTitleNotFound) {
			t.Errorf("Title(%q) = %v", id, err)
		}
	}
}

func TestPG_OneUnfinishedUploadPerTitle(t *testing.T) {
	f := newPG(t)
	title := f.title("movie", f.creator)
	first := f.upload(title, f.creator, nil)
	dup := Upload{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", CreatorID: f.creator, TitleID: title, Filename: "x.mp4", ContentType: "video/mp4",
		Container: "mp4", SizeBytes: 1, PartSizeBytes: 1, PartCount: 1, Bucket: "b", ObjectKey: "k", Status: StatusInitiated,
		ExpiresAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}
	if err := f.store.Create(f.ctx, dup); !errors.Is(err, ErrTitleBusy) {
		t.Fatalf("second unfinished upload for a title: %v", err)
	}
	// uploaded and processing are still unfinished
	if _, err := f.store.MarkUploaded(f.ctx, first.ID, DefaultPolicy(60, 1920, 1080), f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Create(f.ctx, dup); !errors.Is(err, ErrTitleBusy) {
		t.Fatalf("an 'uploaded' upload still blocks the title: %v", err)
	}
	f.exec(`UPDATE uploads SET status = 'processing' WHERE id = $1::uuid`, first.ID)
	if err := f.store.Create(f.ctx, dup); !errors.Is(err, ErrTitleBusy) {
		t.Fatalf("a 'processing' upload still blocks the title: %v", err)
	}
	// terminal states release the title
	for _, st := range []string{"rejected", "aborted", "expired", "ready"} {
		f.exec(`UPDATE uploads SET status = $2::upload_status, reject_code = 'x' WHERE id = $1::uuid`, first.ID, st)
		d := dup
		d.ID, _ = newUUID()
		if err := f.store.Create(f.ctx, d); err != nil {
			t.Fatalf("after %s the title must be free: %v", st, err)
		}
		f.exec(`DELETE FROM uploads WHERE id = $1::uuid`, d.ID)
	}
}

func TestPG_MarkUploadedQueuesTheJobAtomically(t *testing.T) {
	f := newPG(t)
	u := f.upload(f.title("movie", f.creator), f.creator, nil)
	policy := DefaultPolicy(3600, 1920, 1080)

	job, err := f.store.MarkUploaded(f.ctx, u.ID, policy, f.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.UploadID != u.ID || job.State != "queued" || job.Attempts != 0 {
		t.Fatalf("job: %+v", job)
	}
	got, _ := f.store.Get(f.ctx, u.ID)
	if got.Status != StatusUploaded || got.S3UploadID != "" || got.CompletedAt == nil || !got.CompletedAt.Equal(f.now.Add(time.Minute)) {
		t.Fatalf("upload: %+v", got)
	}
	var raw []byte
	var maxAttempts int
	var runAfter time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT policy, max_attempts, run_after FROM transcode_jobs WHERE id = $1::uuid`, job.ID).Scan(&raw, &maxAttempts, &runAfter); err != nil {
		t.Fatal(err)
	}
	var stored Policy
	if err := json.Unmarshal(raw, &stored); err != nil || stored.MaxHeight != 1080 || stored.MaxDurationSeconds != 3600 || len(stored.Containers) != 4 {
		t.Fatalf("policy snapshot: %s (%v)", raw, err)
	}
	if maxAttempts != 3 || runAfter.After(f.now.Add(time.Minute)) {
		t.Fatalf("a new job is claimable at once and retried up to 3 times: %d %v", maxAttempts, runAfter)
	}
	if j, err := f.store.JobFor(f.ctx, u.ID); err != nil || j.ID != job.ID {
		t.Fatalf("JobFor: %+v, %v", j, err)
	}

	// second call: conflict, and still exactly one job
	if _, err := f.store.MarkUploaded(f.ctx, u.ID, policy, f.now); !errors.Is(err, ErrConflict) {
		t.Fatalf("second MarkUploaded: %v", err)
	}
	var n int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM transcode_jobs WHERE upload_id = $1::uuid`, u.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d jobs", n)
	}
}

func TestPG_MarkUploadedRollsBackWhenTheJobCannotBeQueued(t *testing.T) {
	f := newPG(t)
	u := f.upload(f.title("movie", f.creator), f.creator, nil)
	// an open job already exists, so queuing a second violates the one-open-job-per-upload index
	f.exec(`INSERT INTO transcode_jobs (upload_id, policy) VALUES ($1::uuid, '{}')`, u.ID)
	if _, err := f.store.MarkUploaded(f.ctx, u.ID, DefaultPolicy(60, 1, 1), f.now); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("want a database error, got %v", err)
	}
	if got, _ := f.store.Get(f.ctx, u.ID); got.Status != StatusInitiated || got.S3UploadID == "" {
		t.Fatalf("the status change must roll back with the failed job insert: %+v", got)
	}
}

func TestPG_JobForUnknown(t *testing.T) {
	f := newPG(t)
	u := f.upload(f.title("movie", f.creator), f.creator, nil)
	for _, id := range []string{u.ID, "garbage", "00000000-0000-4000-8000-000000000000"} {
		if _, err := f.store.JobFor(f.ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("JobFor(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestPG_RejectAndClose(t *testing.T) {
	f := newPG(t)
	reject := func(id string) error { return f.store.Reject(f.ctx, id, "unsupported_container", "not a video", f.now) }

	a := f.upload(f.title("movie", f.creator), f.creator, nil)
	if err := reject(a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.Get(f.ctx, a.ID)
	if got.Status != StatusRejected || got.RejectCode != "unsupported_container" || got.RejectReason != "not a video" || got.S3UploadID != "" {
		t.Fatalf("%+v", got)
	}
	if err := reject(a.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejecting twice: %v", err)
	}

	b := f.upload(f.title("movie", f.creator), f.creator, nil)
	if _, err := f.store.MarkUploaded(f.ctx, b.ID, DefaultPolicy(60, 1, 1), f.now); err != nil {
		t.Fatal(err)
	}
	if err := reject(b.ID); err != nil { // an 'uploaded' file can still be refused
		t.Fatalf("reject after upload: %v", err)
	}

	c := f.upload(f.title("movie", f.creator), f.creator, nil)
	if err := f.store.Close(f.ctx, c.ID, StatusAborted, f.now); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.store.Get(f.ctx, c.ID); got.Status != StatusAborted || got.S3UploadID != "" {
		t.Fatalf("%+v", got)
	}
	if err := f.store.Close(f.ctx, c.ID, StatusAborted, f.now); !errors.Is(err, ErrConflict) {
		t.Fatalf("closing twice: %v", err)
	}
	if err := reject(c.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejecting an aborted upload: %v", err)
	}
	d := f.upload(f.title("movie", f.creator), f.creator, nil)
	if err := f.store.Close(f.ctx, d.ID, StatusReady, f.now); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("Close must refuse targets other than aborted/expired: %v", err)
	}
	if err := f.store.Close(f.ctx, d.ID, StatusExpired, f.now); err != nil {
		t.Fatal(err)
	}
}

func TestPG_Quotas(t *testing.T) {
	f := newPG(t)
	size := func(n int64) func(*Upload) {
		return func(u *Upload) { u.SizeBytes = n; u.PartSizeBytes = n; u.PartCount = 1 }
	}
	at := func(d time.Duration) func(*Upload) { return func(u *Upload) { u.CreatedAt = f.now.Add(d) } }
	both := func(a, b func(*Upload)) func(*Upload) { return func(u *Upload) { a(u); b(u) } }

	live := f.upload(f.title("movie", f.creator), f.creator, both(size(100), at(-time.Hour)))
	done := f.upload(f.title("movie", f.creator), f.creator, both(size(200), at(-2*time.Hour)))
	cancelled := f.upload(f.title("movie", f.creator), f.creator, both(size(400), at(-3*time.Hour)))
	f.upload(f.title("movie", f.creator), f.creator, both(size(800), at(-30*time.Hour))) // outside the window
	f.upload(f.title("movie", f.other), f.other, size(1600))                             // someone else's

	if _, err := f.store.MarkUploaded(f.ctx, done.ID, DefaultPolicy(60, 1, 1), f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(f.ctx, cancelled.ID, StatusAborted, f.now); err != nil {
		t.Fatal(err)
	}
	_ = live

	active, err := f.store.CountActive(f.ctx, f.creator)
	if err != nil || active != 3 { // live (initiated), done (uploaded), and the 30 h old initiated one
		t.Fatalf("CountActive = %d, %v", active, err)
	}
	bytes, err := f.store.BytesSince(f.ctx, f.creator, f.now.Add(-24*time.Hour))
	if err != nil || bytes != 300 { // 100 + 200: the cancelled 400 and the out-of-window 800 do not count
		t.Fatalf("BytesSince = %d, %v", bytes, err)
	}
	if n, _ := f.store.BytesSince(f.ctx, f.creator, f.now.Add(time.Hour)); n != 0 {
		t.Fatalf("an empty window sums to %d, want 0", n)
	}
	if n, _ := f.store.CountActive(f.ctx, f.other); n != 1 {
		t.Fatalf("other creator: %d", n)
	}
}

func TestPG_Expired(t *testing.T) {
	f := newPG(t)
	exp := func(d time.Duration) func(*Upload) { return func(u *Upload) { u.ExpiresAt = f.now.Add(d) } }
	oldest := f.upload(f.title("movie", f.creator), f.creator, exp(-3*time.Hour))
	older := f.upload(f.title("movie", f.creator), f.creator, exp(-2*time.Hour))
	f.upload(f.title("movie", f.creator), f.creator, exp(time.Hour)) // not yet due
	finished := f.upload(f.title("movie", f.creator), f.creator, exp(-time.Hour))
	if _, err := f.store.MarkUploaded(f.ctx, finished.ID, DefaultPolicy(60, 1, 1), f.now); err != nil {
		t.Fatal(err)
	}

	got, err := f.store.Expired(f.ctx, f.now, 10)
	if err != nil || len(got) != 2 || got[0].ID != oldest.ID || got[1].ID != older.ID || got[0].S3UploadID == "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, _ = f.store.Expired(f.ctx, f.now, 1); len(got) != 1 || got[0].ID != oldest.ID {
		t.Fatalf("limit: %+v", got)
	}
	if got, _ = f.store.Expired(f.ctx, f.now.Add(-24*time.Hour), 10); len(got) != 0 {
		t.Fatalf("nothing was due yesterday: %+v", got)
	}
}

func TestPG_WithLock(t *testing.T) {
	f := newPG(t)
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.store.WithLock(f.ctx, 4242, func(context.Context) error {
			close(inside)
			<-release
			return nil
		})
		done <- err
	}()
	<-inside
	ran, err := f.store.WithLock(f.ctx, 4242, func(context.Context) error { t.Error("the second holder must not run"); return nil })
	if err != nil || ran {
		t.Fatalf("while held: ran=%v err=%v", ran, err)
	}
	if ran, err := f.store.WithLock(f.ctx, 4243, func(context.Context) error { return nil }); err != nil || !ran {
		t.Fatalf("a different lock key is independent: ran=%v err=%v", ran, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	ran, err = f.store.WithLock(f.ctx, 4242, func(context.Context) error { return boom })
	if !ran || !errors.Is(err, boom) {
		t.Fatalf("after release: ran=%v err=%v", ran, err)
	}
	// an error inside fn must still release the lock
	if ran, err = f.store.WithLock(f.ctx, 4242, func(context.Context) error { return nil }); err != nil || !ran {
		t.Fatalf("lock not released after an error: ran=%v err=%v", ran, err)
	}
}

func TestPG_SchemaConstraints(t *testing.T) {
	f := newPG(t)
	u := f.upload(f.title("movie", f.creator), f.creator, nil)
	bad := map[string]string{
		"zero size":               `UPDATE uploads SET size_bytes = 0 WHERE id = $1::uuid`,
		"zero parts":              `UPDATE uploads SET part_count = 0 WHERE id = $1::uuid`,
		"too many parts":          `UPDATE uploads SET part_count = 10001 WHERE id = $1::uuid`,
		"unknown container":       `UPDATE uploads SET container = 'avi' WHERE id = $1::uuid`,
		"empty filename":          `UPDATE uploads SET filename = '' WHERE id = $1::uuid`,
		"rejected without a code": `UPDATE uploads SET status = 'rejected' WHERE id = $1::uuid`,
		"unknown status":          `UPDATE uploads SET status = 'weird' WHERE id = $1::uuid`,
	}
	for name, sql := range bad {
		if _, err := f.pool.Exec(f.ctx, sql, u.ID); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	f.exec(`INSERT INTO transcode_jobs (upload_id, policy) VALUES ($1::uuid, '{}')`, u.ID)
	badJobs := map[string]string{
		"a second open job":       `INSERT INTO transcode_jobs (upload_id, policy) VALUES ($1::uuid, '{}')`,
		"percent over 100":        `UPDATE transcode_jobs SET percent = 101 WHERE upload_id = $1::uuid`,
		"negative attempts":       `UPDATE transcode_jobs SET attempts = -1 WHERE upload_id = $1::uuid`,
		"finished without a time": `UPDATE transcode_jobs SET state = 'failed' WHERE upload_id = $1::uuid`,
	}
	for name, sql := range badJobs {
		if _, err := f.pool.Exec(f.ctx, sql, u.ID); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// a finished job releases the upload for a new one
	f.exec(`UPDATE transcode_jobs SET state = 'failed', finished_at = now() WHERE upload_id = $1::uuid`, u.ID)
	f.exec(`INSERT INTO transcode_jobs (upload_id, policy) VALUES ($1::uuid, '{}')`, u.ID)
}

func TestPG_DeletingATitleRemovesItsUploadsAndJobs(t *testing.T) {
	f := newPG(t)
	title := f.title("movie", f.creator)
	u := f.upload(title, f.creator, nil)
	if _, err := f.store.MarkUploaded(f.ctx, u.ID, DefaultPolicy(60, 1, 1), f.now); err != nil {
		t.Fatal(err)
	}
	f.exec(`DELETE FROM titles WHERE id = $1::uuid`, title)
	var n int
	_ = f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM uploads) + (SELECT count(*) FROM transcode_jobs)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows survived the title", n)
	}
	// but a creator with uploads cannot be deleted by accident
	u2 := f.upload(f.title("movie", f.creator), f.creator, nil)
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM users WHERE id = $1::uuid`, f.creator); err == nil {
		t.Fatalf("deleting a user that still has upload %s must be refused", u2.ID)
	}
}
