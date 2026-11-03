package transcode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Options controls how FFmpeg is run.
type Options struct {
	Threads        int    // 0 lets FFmpeg choose
	Preset         string // libx264 preset
	SegmentSeconds int    // HLS segment length
}

// SegmentSeconds is the HLS segment duration: 6 s is the usual balance between start-up speed and
// request overhead on mobile networks.
const SegmentSeconds = 6

// BuildHLSArgs builds the FFmpeg arguments that decode the source ONCE and write every rung of the
// ladder as fMP4 HLS under outDir (<rung>/index.m3u8, init.mp4, seg_%05d.m4s) plus master.m3u8.
// The flags are explained in docs/transcoding.md.
func BuildHLSArgs(in, outDir string, p Probe, rungs []Rung, o Options) []string {
	seg := o.SegmentSeconds
	if seg <= 0 {
		seg = SegmentSeconds
	}
	n := len(rungs)
	args := []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-nostats", "-progress", "pipe:1"}
	if o.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(o.Threads), "-filter_threads", strconv.Itoa(o.Threads), "-filter_complex_threads", strconv.Itoa(o.Threads))
	}
	args = append(args, "-i", in)

	// Video: optional frame-rate cap, split once per rung, scale the SHORT side to the rung height.
	var fg strings.Builder
	head := "[0:v:0]"
	if p.FPS > 30.5 {
		fg.WriteString(head + "fps=30[vf];")
		head = "[vf]"
	}
	fg.WriteString(head + fmt.Sprintf("split=%d", n))
	for i := range rungs {
		fg.WriteString(fmt.Sprintf("[s%d]", i))
	}
	fg.WriteString(";")
	for i, r := range rungs {
		fg.WriteString(fmt.Sprintf("[s%d]scale=w='if(gt(iw,ih),-2,%d)':h='if(gt(iw,ih),%d,-2)':flags=bicubic,format=yuv420p[v%d];", i, r.Height, r.Height, i))
	}
	// Audio: one loudness-normalised, stereo, 48 kHz stream, copied to every rung.
	if p.HasAudio() {
		fg.WriteString("[0:a:0]loudnorm=I=-16:TP=-1.5:LRA=11,aresample=48000,aformat=channel_layouts=stereo,asplit=" + strconv.Itoa(n))
		for i := range rungs {
			fg.WriteString(fmt.Sprintf("[a%d]", i))
		}
	}
	args = append(args, "-filter_complex", strings.TrimSuffix(fg.String(), ";"))

	var vsm []string
	for i, r := range rungs {
		args = append(args, "-map", fmt.Sprintf("[v%d]", i))
		if p.HasAudio() {
			args = append(args, "-map", fmt.Sprintf("[a%d]", i))
			vsm = append(vsm, fmt.Sprintf("v:%d,a:%d,name:%s", i, i, r.Name))
		} else {
			vsm = append(vsm, fmt.Sprintf("v:%d,name:%s", i, r.Name))
		}
	}
	args = append(args, "-c:v", "libx264", "-preset", o.Preset, "-profile:v", "main", "-sc_threshold", "0",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", seg))
	if o.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(o.Threads))
	}
	for i, r := range rungs {
		args = append(args,
			fmt.Sprintf("-b:v:%d", i), fmt.Sprintf("%dk", r.VideoKbps),
			fmt.Sprintf("-maxrate:v:%d", i), fmt.Sprintf("%dk", r.VideoKbps*110/100),
			fmt.Sprintf("-bufsize:v:%d", i), fmt.Sprintf("%dk", r.VideoKbps*2))
	}
	if p.HasAudio() {
		args = append(args, "-c:a", "aac", "-ac", "2", "-ar", "48000")
		for i, r := range rungs {
			args = append(args, fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", r.AudioKbps))
		}
	}
	args = append(args,
		"-f", "hls", "-hls_time", strconv.Itoa(seg), "-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
		"-master_pl_name", "master.m3u8",
		"-hls_segment_filename", filepath.Join(outDir, "%v", "seg_%05d.m4s"),
		"-var_stream_map", strings.Join(vsm, " "),
		filepath.Join(outDir, "%v", "index.m3u8"))
	return args
}

// BuildPosterArgs extracts one frame, at most 1280 px wide, as a JPEG.
func BuildPosterArgs(in, out string, p Probe) []string {
	at := math.Min(p.DurationSecs*0.1, 30)
	if at > p.DurationSecs-0.5 {
		at = 0
	}
	return []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-ss", strconv.FormatFloat(at, 'f', 3, 64), "-i", in,
		"-frames:v", "1", "-vf", "scale='min(1280,iw)':-2", "-q:v", "3", out}
}

// SpritePlan decides the thumbnail grid for a video of the given length: at most 100 thumbnails,
// one every `Interval` seconds, laid out `Cols` across.
type SpritePlan struct {
	Interval, Count, Cols, Rows int
}

// Thumbnail size inside the sprite.
const (
	ThumbW = 160
	ThumbH = 90
)

// PlanSprite returns the grid for a video of duration seconds.
func PlanSprite(duration float64) SpritePlan {
	interval := int(math.Ceil(duration / 100))
	if interval < 5 {
		interval = 5
	}
	count := int(math.Ceil(duration / float64(interval)))
	if count < 1 {
		count = 1
	}
	cols := min(10, count)
	return SpritePlan{Interval: interval, Count: count, Cols: cols, Rows: (count + cols - 1) / cols}
}

// BuildSpriteArgs builds the command that renders the thumbnail sprite. -skip_frame nokey decodes only
// keyframes, which keeps this fast on long videos.
func BuildSpriteArgs(in, out string, plan SpritePlan) []string {
	vf := fmt.Sprintf("fps=1/%d,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,tile=%dx%d",
		plan.Interval, ThumbW, ThumbH, ThumbW, ThumbH, plan.Cols, plan.Rows)
	return []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-skip_frame", "nokey", "-i", in,
		"-vf", vf, "-frames:v", "1", "-q:v", "5", out}
}

func vttTime(sec float64) string {
	ms := int(math.Round(sec * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// SpriteVTT writes the WebVTT file that maps playback time to a region of the sprite image.
func SpriteVTT(spriteName string, plan SpritePlan, duration float64) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for i := 0; i < plan.Count; i++ {
		start := float64(i * plan.Interval)
		end := math.Min(float64((i+1)*plan.Interval), duration)
		if end <= start {
			break
		}
		x, y := (i%plan.Cols)*ThumbW, (i/plan.Cols)*ThumbH
		fmt.Fprintf(&b, "%s --> %s\n%s#xywh=%d,%d,%d,%d\n\n", vttTime(start), vttTime(end), spriteName, x, y, ThumbW, ThumbH)
	}
	return b.String()
}

// ParseProgress reads one line of FFmpeg's `-progress` output and returns the percentage done.
func ParseProgress(line string, duration float64) (float64, bool) {
	k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok || duration <= 0 {
		return 0, false
	}
	switch k {
	case "out_time_us", "out_time_ms": // both are microseconds in FFmpeg's output despite the name
		us, err := strconv.ParseFloat(v, 64)
		if err != nil || us < 0 {
			return 0, false
		}
		return math.Min(us/1e6/duration*100, 100), true
	case "progress":
		if v == "end" {
			return 100, true
		}
	}
	return 0, false
}

// RunFFmpeg runs FFmpeg, calling onProgress with the percentage whenever it advances. The context
// kills the process; the child is also stopped if the worker dies (Pdeathsig is set by the caller's
// platform file where supported).
func RunFFmpeg(ctx context.Context, ffmpeg string, args []string, duration float64, onProgress func(pct float64)) error {
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.WaitDelay = 10 * time.Second
	setProcAttrs(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, left: 8 << 10}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	last := -1.0
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if pct, ok := ParseProgress(sc.Text(), duration); ok && pct-last >= 1 && onProgress != nil {
			last = pct
			onProgress(pct)
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RunSimple runs a short FFmpeg command (poster, sprite) with no progress.
func RunSimple(ctx context.Context, ffmpeg string, args []string) error {
	return RunFFmpeg(ctx, ffmpeg, args, 0, nil)
}

type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.left <= 0 {
		return n, nil
	}
	if len(p) > l.left {
		p = p[:l.left]
	}
	l.left -= len(p)
	_, _ = l.w.Write(p)
	return n, nil
}

// MakeRungDirs creates outDir/<rung> for every rung: FFmpeg's HLS muxer does not create them.
func MakeRungDirs(outDir string, rungs []Rung) error {
	for _, r := range rungs {
		if err := os.MkdirAll(filepath.Join(outDir, r.Name), 0o755); err != nil {
			return err
		}
	}
	return nil
}
