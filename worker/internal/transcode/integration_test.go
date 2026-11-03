package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real ffmpeg/ffprobe. They are skipped when the binaries are not installed
// (the worker image and `make test-worker` have them).

func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
}

func makeSource(t *testing.T, name string, w, h int, seconds int, audio bool, extra ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=25:duration=%d", w, h, seconds)}
	if audio {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds))
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "25")
	if audio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, extra...)
	args = append(args, out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("making the source: %v\n%s", err, b)
	}
	return out
}

type streamInfo struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		Channels   int    `json:"channels"`
		SampleRate string `json:"sample_rate"`
		PixFmt     string `json:"pix_fmt"`
		Profile    string `json:"profile"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobeFile(t *testing.T, path string) streamInfo {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var si streamInfo
	if err := json.Unmarshal(out, &si); err != nil {
		t.Fatal(err)
	}
	return si
}

func transcode(t *testing.T, src string) (Probe, []Rung, string, []float64) {
	t.Helper()
	ctx := context.Background()
	p, err := RunProbe(ctx, "ffprobe", src)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if r := p.Validate(policy()); r != nil {
		t.Fatalf("validate: %v", r)
	}
	rungs := SelectRungs(p.ShortSide())
	outDir := filepath.Join(t.TempDir(), "hls")
	if err := MakeRungDirs(outDir, rungs); err != nil {
		t.Fatal(err)
	}
	var progress []float64
	args := BuildHLSArgs(src, outDir, p, rungs, Options{Threads: 2, Preset: "ultrafast"})
	if err := RunFFmpeg(ctx, "ffmpeg", args, p.DurationSecs, func(pct float64) { progress = append(progress, pct) }); err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	return p, rungs, outDir, progress
}

func TestFFmpeg_LandscapeWithAudio(t *testing.T) {
	needFFmpeg(t)
	src := makeSource(t, "wide.mp4", 854, 480, 10, true)
	p, rungs, dir, progress := transcode(t, src)
	if names(rungs) != "240p,360p,480p" || p.Width != 854 || p.Height != 480 || p.VideoCodec != "h264" || p.AudioCodec != "aac" || math.Abs(p.DurationSecs-10) > 0.5 {
		t.Fatalf("%v %+v", names(rungs), p)
	}
	if len(progress) < 2 || progress[len(progress)-1] < 99 {
		t.Fatalf("progress should climb to 100: %v", progress)
	}
	for i := 1; i < len(progress); i++ {
		if progress[i] < progress[i-1] {
			t.Fatalf("progress went backwards: %v", progress)
		}
	}

	out, err := VerifyHLS(dir, rungs, p.DurationSecs)
	if err != nil {
		t.Fatalf("the HLS package is not valid: %v", err)
	}
	master, _ := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	for _, want := range []string{"RESOLUTION=428x240", "RESOLUTION=640x360", "RESOLUTION=854x480", "avc1.", "mp4a.40.2", "240p/index.m3u8", "480p/index.m3u8"} {
		if !strings.Contains(string(master), want) {
			t.Errorf("master playlist lacks %q:\n%s", want, master)
		}
	}
	for _, r := range rungs {
		si := ffprobeFile(t, filepath.Join(dir, r.Name, "index.m3u8"))
		var v, a bool
		for _, s := range si.Streams {
			switch s.CodecType {
			case "video":
				v = true
				if s.CodecName != "h264" || s.Height != r.Height || s.PixFmt != "yuv420p" || (s.Profile != "Main" && s.Profile != "Constrained Baseline") {
					t.Errorf("%s video: %+v", r.Name, s)
				}
				if s.Width != r.Height*854/480&^1 && math.Abs(float64(s.Width)-float64(r.Height)*854/480) > 2 {
					t.Errorf("%s keeps the aspect ratio: %dx%d", r.Name, s.Width, s.Height)
				}
			case "audio":
				a = true
				if s.CodecName != "aac" || s.Channels != 2 || s.SampleRate != "48000" {
					t.Errorf("%s audio: %+v", r.Name, s)
				}
			}
		}
		if !v || !a {
			t.Errorf("%s: video=%v audio=%v", r.Name, v, a)
		}
		if d := ffprobeDuration(si); math.Abs(d-10) > 1 {
			t.Errorf("%s lasts %.1fs", r.Name, d)
		}
		segs, _ := filepath.Glob(filepath.Join(dir, r.Name, "seg_*.m4s"))
		if len(segs) != 2 { // 10 s in 6 s segments
			t.Errorf("%s has %d segments, want 2", r.Name, len(segs))
		}
	}
	if len(out.Files) != 3*(1+2)+1+3 { // per rung: init + 2 segments; master; 3 playlists
		t.Errorf("%d files: %v", len(out.Files), out.Files)
	}
}

func ffprobeDuration(si streamInfo) float64 {
	var d float64
	_, _ = fmt.Sscanf(si.Format.Duration, "%f", &d)
	return d
}

func TestFFmpeg_PortraitWithoutAudio(t *testing.T) {
	needFFmpeg(t)
	src := makeSource(t, "phone.mp4", 360, 640, 8, false)
	p, rungs, dir, _ := transcode(t, src)
	if names(rungs) != "240p,360p" || p.ShortSide() != 360 {
		t.Fatalf("a portrait video is laddered by its short side: %s %+v", names(rungs), p)
	}
	if _, err := VerifyHLS(dir, rungs, p.DurationSecs); err != nil {
		t.Fatal(err)
	}
	for _, r := range rungs {
		for _, s := range ffprobeFile(t, filepath.Join(dir, r.Name, "index.m3u8")).Streams {
			if s.CodecType == "audio" {
				t.Errorf("%s: no audio stream expected", r.Name)
			}
			if s.CodecType == "video" && (s.Width != r.Height || s.Height <= s.Width) {
				t.Errorf("%s: %dx%d should be portrait with width %d", r.Name, s.Width, s.Height, r.Height)
			}
		}
	}
}

func TestFFmpeg_TinySourceAndHighFrameRate(t *testing.T) {
	needFFmpeg(t)
	src := makeSource(t, "tiny.mp4", 176, 144, 4, true)
	p, rungs, dir, _ := transcode(t, src)
	if names(rungs) != "144p" {
		t.Fatalf("%s", names(rungs))
	}
	if _, err := VerifyHLS(dir, rungs, p.DurationSecs); err != nil {
		t.Fatal(err)
	}

	// 50 fps is capped to 30 fps
	out := filepath.Join(t.TempDir(), "fast.mp4")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=50:duration=4",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", out).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	p2, rungs2, dir2, _ := transcode(t, out)
	if p2.FPS < 49 {
		t.Fatalf("%+v", p2)
	}
	if _, err := VerifyHLS(dir2, rungs2, p2.DurationSecs); err != nil {
		t.Fatal(err)
	}
	b, _ := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=r_frame_rate", "-of", "csv=p=0",
		filepath.Join(dir2, rungs2[0].Name, "index.m3u8")).Output()
	for _, got := range strings.Fields(string(b)) {
		if got != "30/1" {
			t.Errorf("frame rate after the cap: %q", got)
		}
	}
}

func TestFFmpeg_PosterAndSprite(t *testing.T) {
	needFFmpeg(t)
	src := makeSource(t, "wide.mp4", 640, 360, 12, true)
	ctx := context.Background()
	p, err := RunProbe(ctx, "ffprobe", src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	poster := filepath.Join(dir, "poster.jpg")
	if err := RunSimple(ctx, "ffmpeg", BuildPosterArgs(src, poster, p)); err != nil {
		t.Fatal(err)
	}
	if s := ffprobeFile(t, poster).Streams[0]; s.Width != 640 || s.Height != 360 {
		t.Errorf("poster %dx%d", s.Width, s.Height)
	}
	plan := PlanSprite(p.DurationSecs)
	sprite := filepath.Join(dir, "thumbs.jpg")
	if err := RunSimple(ctx, "ffmpeg", BuildSpriteArgs(src, sprite, plan)); err != nil {
		t.Fatal(err)
	}
	if s := ffprobeFile(t, sprite).Streams[0]; s.Width != plan.Cols*ThumbW || s.Height != plan.Rows*ThumbH {
		t.Errorf("sprite %dx%d, want %dx%d", s.Width, s.Height, plan.Cols*ThumbW, plan.Rows*ThumbH)
	}
}

func TestFFmpeg_ProbeRefusesGarbageAndMissingFiles(t *testing.T) {
	needFFmpeg(t)
	ctx := context.Background()
	junk := filepath.Join(t.TempDir(), "junk.mp4")
	_ = os.WriteFile(junk, []byte(strings.Repeat("this is definitely not a video ", 500)), 0o600)
	if _, err := RunProbe(ctx, "ffprobe", junk); err == nil {
		t.Fatal("garbage must be refused")
	} else if rj := new(Rejection); !errors.As(err, &rj) || rj.Code != CodeCorruptFile {
		t.Fatalf("%v", err)
	}
	trunc := makeSource(t, "ok.mp4", 320, 240, 4, true)
	b, _ := os.ReadFile(trunc)
	cut := filepath.Join(t.TempDir(), "cut.mp4")
	_ = os.WriteFile(cut, b[:len(b)/3], 0o600) // an interrupted copy: the moov atom is gone
	if p, err := RunProbe(ctx, "ffprobe", cut); err == nil {
		t.Fatalf("a truncated MP4 must be refused, got %+v", p)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := RunProbe(cctx, "ffprobe", trunc); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled probe is not the file's fault: %v", err)
	}
}

func TestFFmpeg_CancelStopsTheProcess(t *testing.T) {
	needFFmpeg(t)
	src := makeSource(t, "long.mp4", 640, 360, 20, true)
	p, _ := RunProbe(context.Background(), "ffprobe", src)
	rungs := SelectRungs(p.ShortSide())
	dir := t.TempDir()
	_ = MakeRungDirs(dir, rungs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunFFmpeg(ctx, "ffmpeg", BuildHLSArgs(src, dir, p, rungs, Options{Preset: "veryslow"}), p.DurationSecs, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if err := RunFFmpeg(context.Background(), "ffmpeg", []string{"-i", "/does/not/exist.mp4", filepath.Join(dir, "x.mp4")}, 1, nil); err == nil ||
		!strings.Contains(err.Error(), "ffmpeg failed") {
		t.Fatalf("a failing ffmpeg returns its error text: %v", err)
	}
}
