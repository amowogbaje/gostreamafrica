// Package pipeline runs one transcode job: download the raw upload, probe and validate it, encode
// the HLS ladder, make the poster and thumbnails, convert subtitles, and publish everything to
// object storage. It is safe to run again after any failure: output is built in a scratch directory,
// stale output under the asset's prefix is removed before the new files go up, and media files are
// uploaded before the playlists that reference them.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"streamafrica/worker/internal/jobs"
	"streamafrica/worker/internal/transcode"
)

// Objects is the object-storage side of the pipeline.
type Objects interface {
	Download(ctx context.Context, bucket, key, dest string) (int64, error)
	GetBytes(ctx context.Context, bucket, key string, limit int64) ([]byte, error)
	Upload(ctx context.Context, bucket, key, path, contentType, cacheControl string) error
	List(ctx context.Context, bucket, prefix string) ([]string, error)
	DeletePrefix(ctx context.Context, bucket, prefix string) (int, error)
}

// Store is the database side of the pipeline.
type Store interface {
	LoadUpload(ctx context.Context, uploadID string) (jobs.Upload, error)
	UpsertAsset(ctx context.Context, titleID, sourceKey string) (string, error)
	UpsertSubtitle(ctx context.Context, assetID, language, label, vttKey string) error
}

// Config holds what the pipeline needs.
type Config struct {
	TempDir      string
	MinFreeBytes int64
	BucketHLS    string
	BucketPublic string
	FFmpeg       string
	FFprobe      string
	Threads      int
	Preset       string
	UploadConc   int // parallel uploads of segments
}

// Result describes a finished job.
type Result struct {
	AssetID      string
	ManifestKey  string
	DurationSecs int
	Rungs        []string
}

// ErrNoDisk means the scratch disk cannot hold this job right now. It is transient: retried later.
var ErrNoDisk = errors.New("not enough free disk space for this job")

// Pipeline processes jobs.
type Pipeline struct {
	cfg   Config
	store Store
	objs  Objects
	log   *slog.Logger
	free  func(dir string) (uint64, error)
}

// New builds a Pipeline.
func New(cfg Config, store Store, objs Objects, log *slog.Logger) *Pipeline {
	if cfg.UploadConc <= 0 {
		cfg.UploadConc = 4
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.FFprobe == "" {
		cfg.FFprobe = "ffprobe"
	}
	return &Pipeline{cfg: cfg, store: store, objs: objs, log: log, free: freeBytes}
}

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// Progress reports the stage and percentage (0 to 100) of the whole job.
type Progress func(stage string, percent int)

// Stage boundaries on the 0-100 scale.
const (
	pctDownloaded  = 12
	pctTranscoded  = 85
	pctExtras      = 91
	pctUploadStart = 92
)

var langRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z]{2})?$`)

// normLang returns the language code the database accepts: "pt-br" -> "pt-BR".
func normLang(s string) string {
	primary, region, ok := strings.Cut(s, "-")
	if !ok {
		return strings.ToLower(primary)
	}
	return strings.ToLower(primary) + "-" + strings.ToUpper(region)
}

// Run processes one job. A *transcode.Rejection means the file is unacceptable (permanent); any other
// error is transient and may be retried; context errors mean the work was cancelled.
func (p *Pipeline) Run(ctx context.Context, job jobs.Job, progress Progress) (Result, error) {
	if progress == nil {
		progress = func(string, int) {}
	}
	up, err := p.store.LoadUpload(ctx, job.UploadID)
	if err != nil {
		return Result{}, fmt.Errorf("loading the upload: %w", err)
	}
	assetID, err := p.store.UpsertAsset(ctx, up.TitleID, up.Key)
	if err != nil {
		return Result{}, fmt.Errorf("preparing the media asset: %w", err)
	}
	res := Result{AssetID: assetID, ManifestKey: assetID + "/master.m3u8"}
	log := p.log.With("job_id", job.ID, "upload_id", up.ID, "asset_id", assetID, "attempt", job.Attempts)

	if err := os.MkdirAll(p.cfg.TempDir, 0o755); err != nil {
		return res, err
	}
	need := uint64(up.SizeBytes)*2 + uint64(p.cfg.MinFreeBytes)
	if free, err := p.free(p.cfg.TempDir); err == nil && free < need {
		return res, fmt.Errorf("%w: need %d MiB, have %d MiB", ErrNoDisk, need>>20, free>>20)
	}
	work := filepath.Join(p.cfg.TempDir, "job-"+job.ID)
	_ = os.RemoveAll(work) // a leftover from a crashed attempt
	if err := os.MkdirAll(work, 0o755); err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	// 1. download
	progress("download", 2)
	src := filepath.Join(work, "source"+path.Ext(up.Key))
	t0 := time.Now()
	n, err := p.objs.Download(ctx, up.Bucket, up.Key, src)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("downloading the source: %w", err)
	}
	log.Info("source downloaded", "bytes", n, "seconds", time.Since(t0).Round(time.Millisecond).Seconds())

	// 2. probe and validate
	progress("probe", pctDownloaded)
	probe, err := transcode.RunProbe(ctx, p.cfg.FFprobe, src)
	if err != nil {
		return res, err
	}
	if rj := probe.Validate(job.Policy); rj != nil {
		log.Info("file refused", "code", rj.Code, "reason", rj.Reason)
		return res, rj
	}
	rungs := transcode.SelectRungs(probe.ShortSide())
	for _, r := range rungs {
		res.Rungs = append(res.Rungs, r.Name)
	}
	res.DurationSecs = int(probe.DurationSecs + 0.999)
	log.Info("probed", "container", probe.FormatNames, "video", probe.VideoCodec, "audio", probe.AudioCodec,
		"width", probe.Width, "height", probe.Height, "fps", probe.FPS, "seconds", probe.DurationSecs, "rungs", res.Rungs)

	// 3. transcode
	hlsDir := filepath.Join(work, "hls")
	if err := transcode.MakeRungDirs(hlsDir, rungs); err != nil {
		return res, err
	}
	args := transcode.BuildHLSArgs(src, hlsDir, probe, rungs, transcode.Options{Threads: p.cfg.Threads, Preset: p.cfg.Preset})
	t0 = time.Now()
	err = transcode.RunFFmpeg(ctx, p.cfg.FFmpeg, args, probe.DurationSecs, func(pct float64) {
		progress("transcode", pctDownloaded+int(pct*float64(pctTranscoded-pctDownloaded)/100))
	})
	if err != nil {
		return res, err
	}
	log.Info("transcoded", "seconds", time.Since(t0).Round(time.Millisecond).Seconds())
	out, err := transcode.VerifyHLS(hlsDir, rungs, probe.DurationSecs)
	if err != nil {
		return res, fmt.Errorf("the encoder produced an invalid package: %w", err)
	}

	// 4. poster, thumbnails, subtitles. A failure here costs the extras, not the video.
	progress("extras", pctTranscoded)
	extras := p.makeExtras(ctx, log, src, work, probe, up)
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	subs := p.subtitles(ctx, log, up, work)
	progress("extras", pctExtras)

	// 5. publish: clear old output, media first, playlists last
	progress("upload", pctUploadStart)
	if n, err := p.objs.DeletePrefix(ctx, p.cfg.BucketHLS, assetID+"/"); err != nil {
		return res, fmt.Errorf("clearing old output: %w", err)
	} else if n > 0 {
		log.Info("removed stale output from an earlier attempt", "objects", n)
	}
	var media, last []outFile
	for _, f := range out.Files {
		item := outFile{filepath.Join(hlsDir, filepath.FromSlash(f)), p.cfg.BucketHLS, assetID + "/" + f}
		if strings.HasSuffix(f, ".m3u8") {
			last = append(last, item)
		} else {
			media = append(media, item)
		}
	}
	for _, f := range extras {
		media = append(media, outFile{f.local, p.cfg.BucketPublic, up.TitleID + "/" + f.name})
	}
	for _, f := range subs {
		media = append(media, outFile{f.local, p.cfg.BucketHLS, assetID + "/subtitles/" + f.name})
	}
	total := len(media) + len(last)
	var mu sync.Mutex
	var done int
	put := func(f outFile) error {
		if err := p.objs.Upload(ctx, f.bucket, f.key, f.local, transcode.ContentType(f.key), transcode.CacheControl(f.key)); err != nil {
			return err
		}
		mu.Lock()
		done++
		progress("upload", pctUploadStart+done*(99-pctUploadStart)/total)
		mu.Unlock()
		return nil
	}
	if err := p.uploadAll(ctx, media, put); err != nil {
		return res, fmt.Errorf("uploading: %w", err)
	}
	// Playlists last, the master the very last: nothing points at a file that is not there yet.
	for i := len(last) - 1; i >= 0; i-- {
		if err := put(last[i]); err != nil {
			return res, fmt.Errorf("uploading: %w", err)
		}
	}
	for _, s := range subs {
		if err := p.store.UpsertSubtitle(ctx, assetID, s.lang, s.label, assetID+"/subtitles/"+s.name); err != nil {
			return res, fmt.Errorf("saving subtitles: %w", err)
		}
	}
	log.Info("published", "files", total, "rungs", res.Rungs)
	return res, nil
}

type outFile struct{ local, bucket, key string }

type extraFile struct{ local, name string }

type subFile struct{ local, name, lang, label string }

// uploadAll uploads items with limited parallelism and returns the first error.
func (p *Pipeline) uploadAll(ctx context.Context, items []outFile, put func(outFile) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, p.cfg.UploadConc)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(it outFile) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := put(it); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}(it)
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// makeExtras renders the poster and the thumbnail sprite (+ its WebVTT index). Errors are logged and
// the extras skipped: they are nice to have, not a reason to fail a video.
func (p *Pipeline) makeExtras(ctx context.Context, log *slog.Logger, src, work string, probe transcode.Probe, up jobs.Upload) []extraFile {
	var out []extraFile
	dir := filepath.Join(work, "extras")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	poster := filepath.Join(dir, "poster.jpg")
	if err := transcode.RunSimple(ctx, p.cfg.FFmpeg, transcode.BuildPosterArgs(src, poster, probe)); err != nil {
		log.Warn("poster skipped", "error", err.Error())
	} else {
		out = append(out, extraFile{poster, "poster.jpg"})
	}
	plan := transcode.PlanSprite(probe.DurationSecs)
	sprite := filepath.Join(dir, "thumbs.jpg")
	if err := transcode.RunSimple(ctx, p.cfg.FFmpeg, transcode.BuildSpriteArgs(src, sprite, plan)); err != nil {
		log.Warn("thumbnail sprite skipped", "error", err.Error())
	} else {
		vtt := filepath.Join(dir, "thumbs.vtt")
		if err := os.WriteFile(vtt, []byte(transcode.SpriteVTT("thumbs.jpg", plan, probe.DurationSecs)), 0o644); err == nil {
			out = append(out, extraFile{sprite, "thumbs.jpg"}, extraFile{vtt, "thumbs.vtt"})
		}
	}
	return out
}

// subtitles converts any .srt files the creator uploaded next to the video
// (media-raw/<upload id>/subtitles/<language>.srt) to WebVTT. A bad file is skipped and logged.
func (p *Pipeline) subtitles(ctx context.Context, log *slog.Logger, up jobs.Upload, work string) []subFile {
	keys, err := p.objs.List(ctx, up.Bucket, up.ID+"/subtitles/")
	if err != nil {
		log.Warn("subtitles skipped: cannot list them", "error", err.Error())
		return nil
	}
	var out []subFile
	for _, k := range keys {
		if !strings.EqualFold(path.Ext(k), ".srt") {
			continue
		}
		lang := strings.TrimSuffix(path.Base(k), path.Ext(k))
		if !langRE.MatchString(lang) {
			log.Warn("subtitle skipped: file name is not a language code", "key", k)
			continue
		}
		raw, err := p.objs.GetBytes(ctx, up.Bucket, k, transcode.MaxSubtitleBytes)
		if err != nil {
			log.Warn("subtitle skipped: cannot read it", "key", k, "error", err.Error())
			continue
		}
		vtt, err := transcode.SRTToVTT(raw)
		if err != nil {
			log.Warn("subtitle skipped: invalid", "key", k, "error", err.Error())
			continue
		}
		dir := filepath.Join(work, "subs")
		_ = os.MkdirAll(dir, 0o755)
		local := filepath.Join(dir, normLang(lang)+".vtt")
		if err := os.WriteFile(local, vtt, 0o644); err != nil {
			continue
		}
		norm := normLang(lang)
		out = append(out, subFile{local: local, name: norm + ".vtt", lang: norm, label: strings.ToUpper(norm)})
	}
	return out
}
