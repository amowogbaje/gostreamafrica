package creators

// Policy is the set of limits a source file must satisfy. The API copies it into each
// transcode job (transcode_jobs.policy) so the worker validates against the limits that were
// in force when the upload was accepted, even if configuration changes while the job waits.
// The JSON shape is part of the contract with the worker: docs/upload-flow.md.
type Policy struct {
	Version            int      `json:"version"`
	Containers         []string `json:"containers"`
	VideoCodecs        []string `json:"video_codecs"`
	AudioCodecs        []string `json:"audio_codecs"` // applies only when the file has an audio stream
	MinDurationSeconds int      `json:"min_duration_seconds"`
	MaxDurationSeconds int      `json:"max_duration_seconds"`
	MaxWidth           int      `json:"max_width"`
	MaxHeight          int      `json:"max_height"`
}

// DefaultPolicy returns the policy for the given limits. The codec lists are what FFmpeg decodes
// reliably and what creators actually send; widen them here, in one place.
func DefaultPolicy(maxDurationSecs, maxWidth, maxHeight int) Policy {
	return Policy{
		Version:            1,
		Containers:         []string{"mp4", "mov", "mkv", "webm"},
		VideoCodecs:        []string{"h264", "hevc", "vp8", "vp9", "av1", "mpeg4", "mpeg2video", "prores"},
		AudioCodecs:        []string{"aac", "mp3", "opus", "vorbis", "ac3", "eac3", "flac", "alac", "pcm_s16le", "pcm_s24le"},
		MinDurationSeconds: 1,
		MaxDurationSeconds: maxDurationSecs,
		MaxWidth:           maxWidth,
		MaxHeight:          maxHeight,
	}
}

// containerTypes maps an accepted file extension to the Content-Type stored with the object.
var containerTypes = map[string]string{
	"mp4":  "video/mp4",
	"mov":  "video/quicktime",
	"mkv":  "video/x-matroska",
	"webm": "video/webm",
}

// containerFamily groups extensions by what their first bytes must look like: MP4 and MOV share
// the ISO base media format, MKV and WebM share EBML. See sniffFamily.
func containerFamily(ext string) string {
	switch ext {
	case "mp4", "mov":
		return "iso"
	case "mkv", "webm":
		return "ebml"
	}
	return ""
}
