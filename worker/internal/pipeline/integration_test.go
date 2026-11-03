package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"streamafrica/worker/internal/jobs"
	"streamafrica/worker/internal/platform/s3"
	"streamafrica/worker/internal/testdb"
	"streamafrica/worker/internal/transcode"
)

// Full pipeline against real PostgreSQL, real object storage and real FFmpeg. Skipped unless
// TEST_DATABASE_URL, TEST_S3_ENDPOINT, TEST_S3_ACCESS_KEY and TEST_S3_SECRET_KEY are set and ffmpeg
// is installed (it is in the worker container: `make test-worker`).

type e2e struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	store  *jobs.Store
	s3     *s3.Client
	pipe   *Pipeline
	user   string
	public string // endpoint for anonymous reads
	raw    string
	hls    string
	pub    string
	work   string
}

func hexN(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set")
	}
	pool := testdb.Pool(t)
	client, err := s3.New(s3.Config{Endpoint: endpoint, AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("TEST_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	e := &e2e{t: t, ctx: context.Background(), pool: pool, s3: client, public: endpoint,
		raw: "media-raw", hls: "media-hls", pub: "media-public", work: t.TempDir()}
	e.store = jobs.NewStore(pool, "e2e-worker", time.Minute)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.pipe = New(Config{TempDir: filepath.Join(e.work, "scratch"), MinFreeBytes: 0, BucketHLS: e.hls, BucketPublic: e.pub,
		Threads: 2, Preset: "ultrafast"}, e.store, client, log)
	if err := pool.QueryRow(e.ctx, `INSERT INTO users (email, password_hash, role) VALUES ($1::citext, '!', 'creator') RETURNING id::text`, "e-"+hexN(4)+"@example.com").Scan(&e.user); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO profiles (user_id, display_name) VALUES ($1::uuid, 'e')`, e.user)
	return e
}

// get does an anonymous GET, like a browser without credentials.
func (e *e2e) get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func (e *e2e) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%v\n%s", err, sql)
	}
}

func (e *e2e) video(name string, w, h, secs int, audio bool) string {
	e.t.Helper()
	out := filepath.Join(e.work, name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=25:duration=%d", w, h, secs)}
	if audio {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", secs), "-c:a", "aac")
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "25", out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		e.t.Fatalf("%v\n%s", err, b)
	}
	return out
}

type scenario struct{ title, upload, job, key string }

// stage creates a title, uploads `file` as its raw source, and queues a job with the given policy.
func (e *e2e) stage(file string, policy string) scenario {
	e.t.Helper()
	var s scenario
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO titles (type, slug, name, original_language, status, creator_id)
		VALUES ('movie', $1, $1, 'en', 'draft', $2::uuid) RETURNING id::text`, "t-"+hexN(4), e.user).Scan(&s.title); err != nil {
		e.t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO uploads (creator_id, title_id, filename, content_type, container, size_bytes, part_size_bytes, part_count,
		bucket, object_key, status, expires_at) VALUES ($1::uuid, $2::uuid, 'f.mp4', 'video/mp4', 'mp4', $3, $3, 1, 'media-raw', 'pending', 'uploaded', now() + interval '1 day')
		RETURNING id::text`, e.user, s.title, fi.Size()).Scan(&s.upload); err != nil {
		e.t.Fatal(err)
	}
	s.key = s.upload + "/source.mp4"
	e.exec(`UPDATE uploads SET object_key = $2 WHERE id = $1::uuid`, s.upload, s.key)
	if err := e.s3.Upload(e.ctx, e.raw, s.key, file, "video/mp4", ""); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO transcode_jobs (upload_id, policy) VALUES ($1::uuid, $2::jsonb) RETURNING id::text`, s.upload, policy).Scan(&s.job); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.s3.DeletePrefix(bg, e.raw, s.upload+"/")
		_, _ = e.s3.DeletePrefix(bg, e.pub, s.title+"/")
		var asset string
		if e.pool.QueryRow(bg, `SELECT id::text FROM media_assets WHERE title_id = $1::uuid`, s.title).Scan(&asset) == nil {
			_, _ = e.s3.DeletePrefix(bg, e.hls, asset+"/")
		}
	})
	return s
}

const goodPolicy = `{"version":1,"containers":["mp4","mov","mkv","webm"],"video_codecs":["h264","hevc"],"audio_codecs":["aac"],"min_duration_seconds":1,"max_duration_seconds":600,"max_width":3840,"max_height":2160}`

func (e *e2e) claim(want string) jobs.Job {
	e.t.Helper()
	j, err := e.store.Claim(e.ctx)
	if err != nil || j == nil || j.ID != want {
		e.t.Fatalf("claim: %+v %v", j, err)
	}
	return *j
}

func (e *e2e) keys(bucket, prefix string) []string {
	e.t.Helper()
	k, err := e.s3.List(e.ctx, bucket, prefix)
	if err != nil {
		e.t.Fatal(err)
	}
	return k
}

func TestPipeline_EndToEnd(t *testing.T) {
	e := newE2E(t)
	src := e.video("film.mp4", 854, 480, 12, true)
	s := e.stage(src, goodPolicy)
	// subtitles uploaded next to the video
	srt := filepath.Join(e.work, "x.srt")
	_ = os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:03,000\nHabari yako\n\n2\n00:00:04,000 --> 00:00:06,500\nKaribu\n"), 0o600)
	latin := filepath.Join(e.work, "bad.srt")
	_ = os.WriteFile(latin, []byte("1\n00:00:01,000 --> 00:00:02,000\nfran\xe7ais\n"), 0o600)
	for name, file := range map[string]string{"en.srt": srt, "pt-br.srt": srt, "fr.srt": latin, "notalang.srt": srt, "readme.txt": srt} {
		if err := e.s3.Upload(e.ctx, e.raw, s.upload+"/subtitles/"+name, file, "text/plain", ""); err != nil {
			t.Fatal(err)
		}
	}

	job := e.claim(s.job)
	var stages []string
	last := -1
	res, err := e.pipe.Run(e.ctx, job, func(stage string, pct int) {
		if pct < last {
			t.Errorf("progress went backwards: %d after %d", pct, last)
		}
		last = pct
		if len(stages) == 0 || stages[len(stages)-1] != stage {
			stages = append(stages, stage)
		}
	})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if got := strings.Join(stages, ","); got != "download,probe,transcode,extras,upload" {
		t.Errorf("stages: %s", got)
	}
	if last < 90 || last > 99 {
		t.Errorf("progress ends just below 100 (Finish sets 100): %d", last)
	}
	if strings.Join(res.Rungs, ",") != "240p,360p,480p" || res.DurationSecs < 12 || res.DurationSecs > 13 || res.ManifestKey != res.AssetID+"/master.m3u8" {
		t.Fatalf("%+v", res)
	}

	// what is in storage
	hls := e.keys(e.hls, res.AssetID+"/")
	have := map[string]bool{}
	for _, k := range hls {
		have[strings.TrimPrefix(k, res.AssetID+"/")] = true
	}
	for _, want := range []string{"master.m3u8", "240p/index.m3u8", "240p/init_0.mp4", "240p/seg_00000.m4s", "240p/seg_00001.m4s",
		"480p/index.m3u8", "480p/seg_00001.m4s", "subtitles/en.vtt", "subtitles/pt-BR.vtt"} {
		if !have[want] {
			t.Errorf("missing %s in %v", want, hls)
		}
	}
	if have["subtitles/fr.vtt"] || have["subtitles/notalang.vtt"] || have["subtitles/readme.vtt"] {
		t.Errorf("bad subtitle files must be skipped: %v", hls)
	}
	master, err := e.s3.GetBytes(e.ctx, e.hls, res.AssetID+"/master.m3u8", 1<<20)
	if err != nil || !strings.Contains(string(master), "240p/index.m3u8") || !strings.Contains(string(master), "RESOLUTION=854x480") {
		t.Fatalf("master: %s %v", master, err)
	}
	vtt, _ := e.s3.GetBytes(e.ctx, e.hls, res.AssetID+"/subtitles/pt-BR.vtt", 1<<20)
	if !strings.HasPrefix(string(vtt), "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nHabari yako") {
		t.Fatalf("converted subtitles: %q", vtt)
	}
	// poster and thumbnails are public and cacheable
	for name, ctype := range map[string]string{"poster.jpg": "image/jpeg", "thumbs.jpg": "image/jpeg", "thumbs.vtt": "text/vtt"} {
		resp, err := e.get(e.public + "/" + e.pub + "/" + s.title + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) || len(body) < 100 ||
			!strings.Contains(resp.Header.Get("Cache-Control"), "max-age") {
			t.Errorf("%s: %d %s %q (%d bytes)", name, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"), len(body))
		}
	}
	// the private HLS bucket is not readable anonymously
	if resp, err := e.get(e.public + "/" + e.hls + "/" + res.AssetID + "/master.m3u8"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Error("the HLS bucket must be private")
		}
	}
	if left, _ := os.ReadDir(filepath.Join(e.work, "scratch")); len(left) != 0 {
		t.Errorf("scratch directory not cleaned up: %v", left)
	}

	// record success like the runner does
	if err := e.store.Finish(e.ctx, job, res.AssetID, res.ManifestKey, res.DurationSecs); err != nil {
		t.Fatal(err)
	}
	var status, assetStatus, key string
	var secs int
	var subs int
	_ = e.pool.QueryRow(e.ctx, `SELECT u.status::text, a.status::text, a.hls_manifest_key, a.duration_seconds,
		(SELECT count(*) FROM subtitles WHERE asset_id = a.id) FROM uploads u JOIN media_assets a ON a.title_id = u.title_id WHERE u.id = $1::uuid`, s.upload).
		Scan(&status, &assetStatus, &key, &secs, &subs)
	if status != "ready" || assetStatus != "ready" || key != res.ManifestKey || secs != res.DurationSecs || subs != 2 {
		t.Fatalf("%s %s %s %d %d", status, assetStatus, key, secs, subs)
	}
}

func TestPipeline_RerunIsIdempotentAndClearsStaleOutput(t *testing.T) {
	e := newE2E(t)
	s := e.stage(e.video("a.mp4", 640, 360, 8, true), goodPolicy)
	job := e.claim(s.job)
	res, err := e.pipe.Run(e.ctx, job, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := e.keys(e.hls, res.AssetID+"/")

	// an earlier crashed attempt left junk behind
	junk := filepath.Join(e.work, "junk")
	_ = os.WriteFile(junk, []byte("old"), 0o600)
	for _, k := range []string{"480p/seg_00099.m4s", "720p/index.m3u8", "stale.txt"} {
		if err := e.s3.Upload(e.ctx, e.hls, res.AssetID+"/"+k, junk, "text/plain", ""); err != nil {
			t.Fatal(err)
		}
	}
	res2, err := e.pipe.Run(e.ctx, job, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := e.keys(e.hls, res2.AssetID+"/")
	if res2.AssetID != res.AssetID {
		t.Fatalf("the same asset id on a re-run: %s vs %s", res.AssetID, res2.AssetID)
	}
	if strings.Join(after, "|") != strings.Join(before, "|") {
		t.Fatalf("stale files must be gone and the output identical:\nbefore %v\nafter  %v", before, after)
	}
}

func TestPipeline_Refusals(t *testing.T) {
	e := newE2E(t)
	cases := map[string]struct {
		file   func() string
		policy string
		code   string
	}{
		"too long": {func() string { return e.video("l.mp4", 320, 240, 8, true) },
			strings.Replace(goodPolicy, `"max_duration_seconds":600`, `"max_duration_seconds":5`, 1), transcode.CodeTooLong},
		"resolution": {func() string { return e.video("r.mp4", 640, 360, 4, true) },
			strings.Replace(strings.Replace(goodPolicy, `"max_width":3840`, `"max_width":320`, 1), `"max_height":2160`, `"max_height":180`, 1), transcode.CodeResolutionTooBig},
		"codec": {func() string { return e.video("c.mp4", 320, 240, 4, true) },
			strings.Replace(goodPolicy, `["h264","hevc"]`, `["vp9"]`, 1), transcode.CodeUnsupportedCodec},
		"garbage": {func() string {
			p := filepath.Join(e.work, "garbage.mp4")
			_ = os.WriteFile(p, []byte(strings.Repeat("not a video ", 4000)), 0o600)
			return p
		}, goodPolicy, transcode.CodeCorruptFile},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := e.stage(c.file(), c.policy)
			job := e.claim(s.job)
			res, err := e.pipe.Run(e.ctx, job, nil)
			var rj *transcode.Rejection
			if !errors.As(err, &rj) || rj.Code != c.code || rj.Reason == "" {
				t.Fatalf("got %v, want a %s refusal", err, c.code)
			}
			if n := len(e.keys(e.hls, res.AssetID+"/")); n != 0 {
				t.Fatalf("a refused file publishes nothing, found %d objects", n)
			}
			if err := e.store.Reject(e.ctx, job, res.AssetID, rj); err != nil {
				t.Fatal(err)
			}
			var status, code string
			_ = e.pool.QueryRow(e.ctx, `SELECT status::text, reject_code FROM uploads WHERE id = $1::uuid`, s.upload).Scan(&status, &code)
			if status != "rejected" || code != c.code {
				t.Fatalf("%s %s", status, code)
			}
		})
	}
}

func TestPipeline_TransientProblems(t *testing.T) {
	e := newE2E(t)
	s := e.stage(e.video("a.mp4", 320, 240, 4, true), goodPolicy)
	job := e.claim(s.job)

	e.pipe.free = func(string) (uint64, error) { return 1, nil } // one byte free
	if _, err := e.pipe.Run(e.ctx, job, nil); !errors.Is(err, ErrNoDisk) {
		t.Fatalf("not enough disk is a retryable condition, got %v", err)
	}
	e.pipe.free = freeBytes

	e.exec(`UPDATE uploads SET object_key = 'does/not/exist.mp4' WHERE id = $1::uuid`, s.upload)
	_, err := e.pipe.Run(e.ctx, job, nil)
	var rj *transcode.Rejection
	if err == nil || errors.As(err, &rj) {
		t.Fatalf("a missing source is transient (the upload may still be settling), not a refusal: %v", err)
	}

	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	e.exec(`UPDATE uploads SET object_key = $2 WHERE id = $1::uuid`, s.upload, s.key)
	if _, err := e.pipe.Run(ctx, job, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must surface as a context error, got %v", err)
	}
}
