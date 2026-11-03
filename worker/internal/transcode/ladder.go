// Package transcode turns an uploaded video into an adaptive-bitrate HLS package: it probes and
// validates the source, builds the FFmpeg command lines, checks the output, and prepares the poster,
// thumbnail sprite and subtitles. Nothing here touches the network or the database.
package transcode

import "fmt"

// Rung is one quality level of the ladder.
type Rung struct {
	Name      string
	Height    int // the SHORT side of the frame (portrait videos are scaled by their width)
	VideoKbps int
	AudioKbps int
}

// Ladder is the bitrate ladder, tuned for mobile data in Africa: low rungs are cheap, and 1080p is
// kept modest (H.264 main profile, so every phone decodes it). Documented in docs/transcoding.md.
var Ladder = []Rung{
	{"240p", 240, 250, 48},   // data saver: about 300 kbps all in
	{"360p", 360, 600, 64},   // the default on a weak connection
	{"480p", 480, 1000, 96},  // about 450 MB per hour
	{"720p", 720, 2200, 128}, // about 1 GB per hour
	{"1080p", 1080, 4200, 128},
}

// SelectRungs picks the rungs for a source whose short side is shortSide pixels: every rung that
// does not exceed the source (never upscale). A source smaller than the lowest rung gets a single rung
// at its own size, so there is always at least one.
func SelectRungs(shortSide int) []Rung {
	var out []Rung
	for _, r := range Ladder {
		if r.Height <= shortSide {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		h := shortSide &^ 1 // even, as H.264 4:2:0 requires
		if h < 2 {
			h = 2
		}
		low := Ladder[0]
		out = []Rung{{Name: fmt.Sprintf("%dp", h), Height: h, VideoKbps: low.VideoKbps, AudioKbps: low.AudioKbps}}
	}
	return out
}

// BandwidthBps is the peak bandwidth a rung asks for, in bits per second, with 10 % headroom for
// the encoder's rate variation (matches -maxrate).
func (r Rung) BandwidthBps() int { return (r.VideoKbps*110/100 + r.AudioKbps) * 1000 }
