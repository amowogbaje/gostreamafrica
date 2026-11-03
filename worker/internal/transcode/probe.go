package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Policy is the set of limits a source must satisfy. It arrives as JSON in transcode_jobs.policy
// (written by the API when the upload was accepted; docs/upload-flow.md).
type Policy struct {
	Version            int      `json:"version"`
	Containers         []string `json:"containers"`
	VideoCodecs        []string `json:"video_codecs"`
	AudioCodecs        []string `json:"audio_codecs"`
	MinDurationSeconds int      `json:"min_duration_seconds"`
	MaxDurationSeconds int      `json:"max_duration_seconds"`
	MaxWidth           int      `json:"max_width"`
	MaxHeight          int      `json:"max_height"`
}

// Probe is what we learned about the source.
type Probe struct {
	FormatNames   []string
	DurationSecs  float64
	Width, Height int // as displayed: already swapped when the video is rotated by 90 or 270 degrees
	FPS           float64
	VideoCodec    string
	AudioCodec    string // empty when there is no audio
	AudioChannels int
}

// ShortSide is the smaller of width and height.
func (p Probe) ShortSide() int { return min(p.Width, p.Height) }

// HasAudio reports whether the source has an audio stream.
func (p Probe) HasAudio() bool { return p.AudioCodec != "" }

// Rejection is a permanent refusal of the file, with a code for clients and a sentence for the creator.
type Rejection struct{ Code, Reason string }

func (r *Rejection) Error() string { return r.Code + ": " + r.Reason }

// Rejection codes written to uploads.reject_code by the worker.
const (
	CodeCorruptFile       = "corrupt_file"
	CodeNoVideo           = "no_video_stream"
	CodeUnsupportedFormat = "unsupported_container"
	CodeUnsupportedCodec  = "unsupported_codec"
	CodeTooShort          = "too_short"
	CodeTooLong           = "too_long"
	CodeResolutionTooBig  = "resolution_too_large"
	CodeBadSubtitles      = "invalid_subtitles"
)

type ffprobeJSON struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType    string `json:"codec_type"`
		CodecName    string `json:"codec_name"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		RFrameRate   string `json:"r_frame_rate"`
		AvgFrameRate string `json:"avg_frame_rate"`
		Duration     string `json:"duration"`
		Channels     int    `json:"channels"`
		Disposition  struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		Tags         map[string]string `json:"tags"`
		SideDataList []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	a, _ := strconv.ParseFloat(num, 64)
	b, _ := strconv.ParseFloat(den, 64)
	if b == 0 {
		return 0
	}
	return a / b
}

// ParseProbe reads the JSON printed by `ffprobe -print_format json -show_format -show_streams`.
// Garbage in, a *Rejection (corrupt_file) out.
func ParseProbe(raw []byte) (Probe, error) {
	var j ffprobeJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return Probe{}, &Rejection{CodeCorruptFile, "the file could not be read as a video"}
	}
	var p Probe
	p.FormatNames = strings.Split(j.Format.FormatName, ",")
	gotVideo := false
	for _, s := range j.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition.AttachedPic == 1 || gotVideo { // cover art, or a second video track
				continue
			}
			gotVideo = true
			p.VideoCodec, p.Width, p.Height = s.CodecName, s.Width, s.Height
			p.FPS = parseRate(s.RFrameRate)
			if p.FPS <= 0 || p.FPS > 240 {
				p.FPS = parseRate(s.AvgFrameRate)
			}
			rot := 0.0
			if len(s.SideDataList) > 0 {
				rot = s.SideDataList[0].Rotation
			} else if v, err := strconv.ParseFloat(s.Tags["rotate"], 64); err == nil {
				rot = v
			}
			if int(math.Abs(rot))%180 == 90 {
				p.Width, p.Height = p.Height, p.Width
			}
			if d, err := strconv.ParseFloat(s.Duration, 64); err == nil && p.DurationSecs == 0 {
				p.DurationSecs = d
			}
		case "audio":
			if p.AudioCodec == "" {
				p.AudioCodec, p.AudioChannels = s.CodecName, s.Channels
			}
		}
	}
	if d, err := strconv.ParseFloat(j.Format.Duration, 64); err == nil && d > 0 {
		p.DurationSecs = d
	}
	if !gotVideo {
		return Probe{}, &Rejection{CodeNoVideo, "the file has no video track"}
	}
	if p.Width < 2 || p.Height < 2 || p.DurationSecs <= 0 || math.IsNaN(p.DurationSecs) || math.IsInf(p.DurationSecs, 0) {
		return Probe{}, &Rejection{CodeCorruptFile, "the video has no readable size or duration; the file may be damaged or incomplete"}
	}
	return p, nil
}

// RunProbe executes ffprobe on a file.
func RunProbe(ctx context.Context, ffprobe, file string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", file)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Probe{}, ctx.Err() // cancelled or timed out: not the file's fault
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return Probe{}, &Rejection{CodeCorruptFile, "the file could not be read as a video"}
		}
		return Probe{}, fmt.Errorf("running ffprobe: %w", err)
	}
	return ParseProbe(out.Bytes())
}

// formatToExt maps ffprobe's demuxer names to the extensions the policy uses.
var formatToExt = map[string][]string{"mov": {"mp4", "mov"}, "mp4": {"mp4", "mov"}, "matroska": {"mkv", "webm"}, "webm": {"webm", "mkv"}}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Validate checks the probe against the policy and returns the first refusal, or nil.
func (p Probe) Validate(pol Policy) *Rejection {
	ok := false
	for _, name := range p.FormatNames {
		for _, ext := range formatToExt[name] {
			ok = ok || contains(pol.Containers, ext)
		}
	}
	if !ok {
		return &Rejection{CodeUnsupportedFormat, "this file format is not supported; use MP4, MOV, MKV or WebM"}
	}
	if !contains(pol.VideoCodecs, p.VideoCodec) {
		return &Rejection{CodeUnsupportedCodec, fmt.Sprintf("the video codec %q is not supported; re-export with H.264 (recommended), H.265, VP9 or AV1", p.VideoCodec)}
	}
	if p.HasAudio() && !contains(pol.AudioCodecs, p.AudioCodec) {
		return &Rejection{CodeUnsupportedCodec, fmt.Sprintf("the audio codec %q is not supported; re-export with AAC (recommended), MP3, Opus or AC-3", p.AudioCodec)}
	}
	if pol.MinDurationSeconds > 0 && p.DurationSecs < float64(pol.MinDurationSeconds) {
		return &Rejection{CodeTooShort, fmt.Sprintf("the video is shorter than %d second(s)", pol.MinDurationSeconds)}
	}
	if pol.MaxDurationSeconds > 0 && p.DurationSecs > float64(pol.MaxDurationSeconds) {
		return &Rejection{CodeTooLong, fmt.Sprintf("the video is longer than %d minutes", pol.MaxDurationSeconds/60)}
	}
	long, short := max(p.Width, p.Height), min(p.Width, p.Height)
	if pol.MaxWidth > 0 && pol.MaxHeight > 0 && (long > max(pol.MaxWidth, pol.MaxHeight) || short > min(pol.MaxWidth, pol.MaxHeight)) {
		return &Rejection{CodeResolutionTooBig, fmt.Sprintf("the video is %dx%d; the largest accepted size is %dx%d", p.Width, p.Height, pol.MaxWidth, pol.MaxHeight)}
	}
	return nil
}
