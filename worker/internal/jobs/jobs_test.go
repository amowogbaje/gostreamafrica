package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/worker/internal/testdb"
	"streamafrica/worker/internal/transcode"
)

type fx struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	a, b *Store // two workers
	user string
}

func hexN(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newFx(t *testing.T) *fx {
	t.Helper()
	pool := testdb.Pool(t)
	f := &fx{t: t, ctx: context.Background(), pool: pool, a: NewStore(pool, "worker-a", 2*time.Minute), b: NewStore(pool, "worker-b", 2*time.Minute)}
	if err := pool.QueryRow(f.ctx, `INSERT INTO users (email, password_hash, role) VALUES ($1::citext, '!', 'creator') RETURNING id::text`, "c-"+hexN(4)+"@example.com").Scan(&f.user); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO profiles (user_id, display_name) VALUES ($1::uuid, 'c')`, f.user)
	return f
}

func (f *fx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%v\n%s", err, sql)
	}
}

func (f *fx) scan(sql string, args ...any) []any {
	f.t.Helper()
	var out []any
	rows, err := f.pool.Query(f.ctx, sql, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			f.t.Fatal(err)
		}
		out = vals
	}
	return out
}

type seeded struct{ title, upload, job string }

// queued creates a title, an 'uploaded' upload and a queued job.
func (f *fx) queued(maxAttempts int) seeded {
	f.t.Helper()
	var s seeded
	slug := "t-" + hexN(4)
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO titles (type, slug, name, original_language, status, creator_id)
		VALUES ('movie', $1, $1, 'en', 'draft', $2::uuid) RETURNING id::text`, slug, f.user).Scan(&s.title); err != nil {
		f.t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO uploads (creator_id, title_id, filename, content_type, container, size_bytes, part_size_bytes, part_count,
		bucket, object_key, status, expires_at) VALUES ($1::uuid, $2::uuid, 'f.mp4', 'video/mp4', 'mp4', 1000, 1000, 1, 'media-raw', $3, 'uploaded', now() + interval '1 day')
		RETURNING id::text`, f.user, s.title, "u-"+hexN(4)+"/source.mp4").Scan(&s.upload); err != nil {
		f.t.Fatal(err)
	}
	policy := `{"version":1,"containers":["mp4"],"video_codecs":["h264"],"audio_codecs":["aac"],"min_duration_seconds":1,"max_duration_seconds":600,"max_width":1920,"max_height":1080}`
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO transcode_jobs (upload_id, policy, max_attempts) VALUES ($1::uuid, $2::jsonb, $3) RETURNING id::text`,
		s.upload, policy, maxAttempts).Scan(&s.job); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fx) jobState(id string) (state string, attempts int, lockedBy *string, lastErr *string) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, `SELECT state::text, attempts, locked_by, last_error FROM transcode_jobs WHERE id = $1::uuid`, id).Scan(&state, &attempts, &lockedBy, &lastErr); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fx) uploadState(id string) (status, code string) {
	f.t.Helper()
	var c *string
	if err := f.pool.QueryRow(f.ctx, `SELECT status::text, reject_code FROM uploads WHERE id = $1::uuid`, id).Scan(&status, &c); err != nil {
		f.t.Fatal(err)
	}
	if c != nil {
		code = *c
	}
	return
}

func (f *fx) expireLease(jobID string) {
	f.exec(`UPDATE transcode_jobs SET locked_until = now() - interval '1 minute' WHERE id = $1::uuid`, jobID)
}

func TestClaim_TakesTheOldestRunnableJobOnly(t *testing.T) {
	f := newFx(t)
	if j, err := f.a.Claim(f.ctx); err != nil || j != nil {
		t.Fatalf("an empty queue: %v %v", j, err)
	}
	first, second, later := f.queued(3), f.queued(3), f.queued(3)
	f.exec(`UPDATE transcode_jobs SET run_after = now() - interval '2 minutes' WHERE id = $1::uuid`, first.job)
	f.exec(`UPDATE transcode_jobs SET run_after = now() - interval '1 minute' WHERE id = $1::uuid`, second.job)
	f.exec(`UPDATE transcode_jobs SET run_after = now() + interval '1 hour' WHERE id = $1::uuid`, later.job)

	j, err := f.a.Claim(f.ctx)
	if err != nil || j == nil || j.ID != first.job || j.UploadID != first.upload || j.Attempts != 1 || j.MaxAttempts != 3 {
		t.Fatalf("%+v %v", j, err)
	}
	if j.Policy.MaxDurationSeconds != 600 || j.Policy.MaxHeight != 1080 || len(j.Policy.VideoCodecs) != 1 {
		t.Fatalf("the policy snapshot must come through: %+v", j.Policy)
	}
	state, attempts, by, _ := f.jobState(first.job)
	if state != "running" || attempts != 1 || by == nil || *by != "worker-a" {
		t.Fatalf("%s %d %v", state, attempts, by)
	}
	if st, _ := f.uploadState(first.upload); st != "processing" {
		t.Fatalf("upload is %s", st)
	}
	j2, _ := f.b.Claim(f.ctx)
	if j2 == nil || j2.ID != second.job {
		t.Fatalf("the next job: %+v", j2)
	}
	if j3, _ := f.a.Claim(f.ctx); j3 != nil {
		t.Fatalf("a job scheduled for later must not be claimed: %+v", j3)
	}
}

func TestClaim_ConcurrentWorkersNeverShareAJob(t *testing.T) {
	f := newFx(t)
	const jobs = 6
	want := map[string]bool{}
	for i := 0; i < jobs; i++ {
		want[f.queued(3).job] = true
	}
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := NewStore(f.pool, "w"+string(rune('a'+i)), time.Minute)
			for {
				j, err := s.Claim(f.ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				got[j.ID]++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(got) != jobs {
		t.Fatalf("claimed %d of %d jobs: %v", len(got), jobs, got)
	}
	for id, n := range got {
		if n != 1 || !want[id] {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}

func TestHeartbeatAndLease(t *testing.T) {
	f := newFx(t)
	s := f.queued(3)
	j, _ := f.a.Claim(f.ctx)
	f.exec(`UPDATE transcode_jobs SET locked_until = now() + interval '1 second' WHERE id = $1::uuid`, j.ID)
	if err := f.a.Heartbeat(f.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	var secs float64
	_ = f.pool.QueryRow(f.ctx, `SELECT extract(epoch FROM locked_until - now()) FROM transcode_jobs WHERE id = $1::uuid`, j.ID).Scan(&secs)
	if secs < 100 {
		t.Fatalf("the lease should be extended to about 2 minutes, got %.0fs", secs)
	}
	if err := f.b.Heartbeat(f.ctx, j.ID); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("another worker cannot extend our lease: %v", err)
	}
	// a worker whose lease expired and whose job was taken by someone else
	f.expireLease(j.ID)
	if n, err := f.b.Reap(f.ctx); err != nil || n != 1 {
		t.Fatalf("reap: %d %v", n, err)
	}
	if err := f.a.Heartbeat(f.ctx, j.ID); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("after the job was re-queued: %v", err)
	}
	if st, _ := f.uploadState(s.upload); st != "uploaded" {
		t.Fatalf("a re-queued job's upload goes back to 'uploaded', got %s", st)
	}
}

func TestProgress(t *testing.T) {
	f := newFx(t)
	f.queued(3)
	j, _ := f.a.Claim(f.ctx)
	if err := f.a.Progress(f.ctx, j.ID, "transcode", 42); err != nil {
		t.Fatal(err)
	}
	v := f.scan(`SELECT stage, percent FROM transcode_jobs WHERE id = $1::uuid`, j.ID)
	if v[0] != "transcode" || v[1].(int16) != 42 {
		t.Fatalf("%v", v)
	}
	_ = f.b.Progress(f.ctx, j.ID, "hijack", 1) // not the owner: silently ignored
	_ = f.a.Progress(f.ctx, j.ID, "transcode", 250)
	v = f.scan(`SELECT stage, percent FROM transcode_jobs WHERE id = $1::uuid`, j.ID)
	if v[0] != "transcode" || v[1].(int16) != 100 {
		t.Fatalf("percent is clamped and only the owner writes: %v", v)
	}
}

func TestFinish(t *testing.T) {
	f := newFx(t)
	s := f.queued(3)
	j, _ := f.a.Claim(f.ctx)
	up, err := f.a.LoadUpload(f.ctx, j.UploadID)
	if err != nil || up.TitleID != s.title || up.Bucket != "media-raw" || up.SizeBytes != 1000 || up.Container != "mp4" {
		t.Fatalf("%+v %v", up, err)
	}
	asset, err := f.a.UpsertAsset(f.ctx, up.TitleID, up.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.Finish(f.ctx, *j, asset, asset+"/master.m3u8", 61); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a worker that does not own the job cannot finish it: %v", err)
	}
	if v := f.scan(`SELECT status::text FROM media_assets WHERE id = $1::uuid`, asset); v[0] != "processing" {
		t.Fatalf("a refused Finish must change nothing: %v", v)
	}
	if err := f.a.Finish(f.ctx, *j, asset, asset+"/master.m3u8", 61); err != nil {
		t.Fatal(err)
	}
	if st, _, by, _ := f.jobState(j.ID); st != "succeeded" || by != nil {
		t.Fatalf("%s %v", st, by)
	}
	if st, _ := f.uploadState(s.upload); st != "ready" {
		t.Fatalf("upload %s", st)
	}
	v := f.scan(`SELECT status::text, hls_manifest_key, duration_seconds FROM media_assets WHERE id = $1::uuid`, asset)
	if v[0] != "ready" || v[1] != asset+"/master.m3u8" || v[2].(int32) != 61 {
		t.Fatalf("%v", v)
	}
	if v := f.scan(`SELECT stage, percent, finished_at IS NOT NULL FROM transcode_jobs WHERE id = $1::uuid`, j.ID); v[0] != "done" || v[1].(int16) != 100 || v[2] != true {
		t.Fatalf("%v", v)
	}
	if err := f.a.Finish(f.ctx, *j, asset, "x", 1); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("finishing twice: %v", err)
	}
}

func TestReject(t *testing.T) {
	f := newFx(t)
	s := f.queued(3)
	j, _ := f.a.Claim(f.ctx)
	asset, _ := f.a.UpsertAsset(f.ctx, s.title, "k")
	rj := &transcode.Rejection{Code: transcode.CodeUnsupportedCodec, Reason: "the video codec \"wmv3\" is not supported"}
	if err := f.b.Reject(f.ctx, *j, asset, rj); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("%v", err)
	}
	if err := f.a.Reject(f.ctx, *j, asset, rj); err != nil {
		t.Fatal(err)
	}
	st, _, by, lastErr := f.jobState(j.ID)
	if st != "failed" || by != nil || lastErr == nil {
		t.Fatalf("a refused file's job is final: %s %v %v", st, by, lastErr)
	}
	if status, code := f.uploadState(s.upload); status != "rejected" || code != "unsupported_codec" {
		t.Fatalf("%s %s", status, code)
	}
	var reason string
	_ = f.pool.QueryRow(f.ctx, `SELECT reject_reason FROM uploads WHERE id = $1::uuid`, s.upload).Scan(&reason)
	if reason != rj.Reason {
		t.Fatalf("the creator sees the reason: %q", reason)
	}
	if v := f.scan(`SELECT status::text FROM media_assets WHERE id = $1::uuid`, asset); v[0] != "failed" {
		t.Fatalf("asset %v", v)
	}
	if again, _ := f.a.Claim(f.ctx); again != nil {
		t.Fatalf("a refused job must never run again: %+v", again)
	}
}

func TestRetryThenDeadLetter(t *testing.T) {
	f := newFx(t)
	s := f.queued(2)
	boom := errors.New("ffmpeg crashed: out of memory")

	j, _ := f.a.Claim(f.ctx)
	asset, _ := f.a.UpsertAsset(f.ctx, s.title, "k")
	if _, err := f.b.Retry(f.ctx, *j, boom, time.Minute, asset); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("%v", err)
	}
	dead, err := f.a.Retry(f.ctx, *j, boom, 90*time.Second, asset)
	if err != nil || dead {
		t.Fatalf("first failure: dead=%v %v", dead, err)
	}
	st, attempts, by, lastErr := f.jobState(j.ID)
	if st != "queued" || attempts != 1 || by != nil || lastErr == nil || *lastErr != boom.Error() {
		t.Fatalf("%s %d %v %v", st, attempts, by, lastErr)
	}
	if status, _ := f.uploadState(s.upload); status != "uploaded" {
		t.Fatalf("while waiting to retry the upload is 'uploaded': %s", status)
	}
	var wait float64
	_ = f.pool.QueryRow(f.ctx, `SELECT extract(epoch FROM run_after - now()) FROM transcode_jobs WHERE id = $1::uuid`, j.ID).Scan(&wait)
	if wait < 60 || wait > 95 {
		t.Fatalf("backoff of 90 s, got %.0f", wait)
	}
	if early, _ := f.a.Claim(f.ctx); early != nil {
		t.Fatal("a job in backoff must not be claimed")
	}
	f.exec(`UPDATE transcode_jobs SET run_after = now() WHERE id = $1::uuid`, j.ID)

	j2, _ := f.b.Claim(f.ctx)
	if j2 == nil || j2.Attempts != 2 {
		t.Fatalf("%+v", j2)
	}
	dead, err = f.b.Retry(f.ctx, *j2, boom, time.Minute, asset)
	if err != nil || !dead {
		t.Fatalf("second failure of two attempts must dead-letter: dead=%v %v", dead, err)
	}
	if st, _, _, _ := f.jobState(j.ID); st != "failed" {
		t.Fatalf("dead letter state: %s", st)
	}
	if status, code := f.uploadState(s.upload); status != "rejected" || code != ProcessingFailedCode {
		t.Fatalf("%s %s", status, code)
	}
	if v := f.scan(`SELECT status::text FROM media_assets WHERE id = $1::uuid`, asset); v[0] != "failed" {
		t.Fatalf("%v", v)
	}
	if v := f.scan(`SELECT finished_at IS NOT NULL FROM transcode_jobs WHERE id = $1::uuid`, j.ID); v[0] != true {
		t.Fatal("a dead job has finished_at")
	}
	if more, _ := f.a.Claim(f.ctx); more != nil {
		t.Fatalf("%+v", more)
	}
}

func TestRelease_DoesNotCostAnAttempt(t *testing.T) {
	f := newFx(t)
	s := f.queued(3)
	j, _ := f.a.Claim(f.ctx)
	if err := f.b.Release(f.ctx, *j); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("%v", err)
	}
	if err := f.a.Release(f.ctx, *j); err != nil {
		t.Fatal(err)
	}
	st, attempts, by, _ := f.jobState(j.ID)
	if st != "queued" || attempts != 0 || by != nil {
		t.Fatalf("%s %d %v", st, attempts, by)
	}
	if status, _ := f.uploadState(s.upload); status != "uploaded" {
		t.Fatal(status)
	}
	if again, _ := f.b.Claim(f.ctx); again == nil || again.Attempts != 1 {
		t.Fatalf("a released job runs at once with its attempt intact: %+v", again)
	}
}

func TestReap(t *testing.T) {
	f := newFx(t)
	stale, fresh, doomed := f.queued(3), f.queued(3), f.queued(1)
	for range 3 {
		if j, _ := f.a.Claim(f.ctx); j == nil {
			t.Fatal("expected a job")
		}
	}
	f.expireLease(stale.job)
	f.expireLease(doomed.job)
	n, err := f.b.Reap(f.ctx)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if st, _, by, lastErr := f.jobState(stale.job); st != "queued" || by != nil || lastErr == nil {
		t.Fatalf("%s %v %v", st, by, lastErr)
	}
	if st, _, _, _ := f.jobState(fresh.job); st != "running" {
		t.Fatalf("a live lease must be left alone: %s", st)
	}
	if st, _, _, _ := f.jobState(doomed.job); st != "failed" {
		t.Fatalf("a job out of attempts is dead-lettered: %s", st)
	}
	if status, code := f.uploadState(doomed.upload); status != "rejected" || code != ProcessingFailedCode {
		t.Fatalf("%s %s", status, code)
	}
	if status, _ := f.uploadState(stale.upload); status != "uploaded" {
		t.Fatal(status)
	}
	if n, _ := f.b.Reap(f.ctx); n != 0 {
		t.Fatalf("nothing more to reap, got %d", n)
	}
	if again, _ := f.b.Claim(f.ctx); again == nil || again.ID != stale.job || again.Attempts != 2 {
		t.Fatalf("the reaped job is claimable again: %+v", again)
	}
}

func TestUpsertAssetAndSubtitle(t *testing.T) {
	f := newFx(t)
	s := f.queued(3)
	id1, err := f.a.UpsertAsset(f.ctx, s.title, "u/source.mp4")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE media_assets SET status = 'ready', hls_manifest_key = 'x', duration_seconds = 5 WHERE id = $1::uuid`, id1)
	id2, err := f.a.UpsertAsset(f.ctx, s.title, "u2/source.mp4")
	if err != nil || id2 != id1 {
		t.Fatalf("reprocessing reuses the asset (and so its storage prefix): %s %s %v", id1, id2, err)
	}
	v := f.scan(`SELECT status::text, source_key, hls_manifest_key, duration_seconds FROM media_assets WHERE id = $1::uuid`, id1)
	if v[0] != "processing" || v[1] != "u2/source.mp4" || v[2] != nil || v[3] != nil {
		t.Fatalf("a re-run starts clean: %v", v)
	}

	if err := f.a.UpsertSubtitle(f.ctx, id1, "en", "EN", id1+"/subtitles/en.vtt"); err != nil {
		t.Fatal(err)
	}
	if err := f.a.UpsertSubtitle(f.ctx, id1, "en", "English", id1+"/subtitles/en.vtt"); err != nil {
		t.Fatal(err)
	}
	if err := f.a.UpsertSubtitle(f.ctx, id1, "pt-BR", "PT-BR", id1+"/subtitles/pt-BR.vtt"); err != nil {
		t.Fatal(err)
	}
	if v := f.scan(`SELECT count(*), max(label) FILTER (WHERE language = 'en') FROM subtitles WHERE asset_id = $1::uuid`, id1); v[0].(int64) != 2 || v[1] != "English" {
		t.Fatalf("%v", v)
	}
	if err := f.a.UpsertSubtitle(f.ctx, id1, "english", "x", "y"); err == nil {
		t.Fatal("the database refuses a malformed language code")
	}
}

func TestBackoff(t *testing.T) {
	base, mx := 30*time.Second, 15*time.Minute
	cases := map[int]time.Duration{0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 4: 4 * time.Minute, 5: 8 * time.Minute, 6: 15 * time.Minute, 50: 15 * time.Minute}
	for attempt, want := range cases {
		if got := Backoff(attempt, base, mx); got != want {
			t.Errorf("attempt %d: %v, want %v", attempt, got, want)
		}
	}
	if Backoff(1, time.Hour, time.Minute) != time.Minute {
		t.Error("the cap applies even to the first retry")
	}
}
