package transcode

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxSubtitleBytes bounds a subtitle file.
const MaxSubtitleBytes = 2 << 20

var (
	srtTime = regexp.MustCompile(`^(\d{1,2}:\d{2}:\d{2})[,.](\d{1,3})\s+-->\s+(\d{1,2}:\d{2}:\d{2})[,.](\d{1,3})(.*)$`)
	allNums = regexp.MustCompile(`^\d+$`)
)

func pad3(ms string) string { return (ms + "000")[:3] }

func hms(s string) string {
	if len(s) == 7 { // h:mm:ss
		return "0" + s
	}
	return s
}

// SRTToVTT converts SubRip subtitles to WebVTT. The input must be UTF-8 (a BOM is accepted and dropped);
// anything else is refused rather than guessed at, because a wrongly guessed encoding shows garbage to viewers.
func SRTToVTT(srt []byte) ([]byte, error) {
	if len(srt) > MaxSubtitleBytes {
		return nil, &Rejection{CodeBadSubtitles, "the subtitle file is too large"}
	}
	srt = bytes.TrimPrefix(srt, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(srt) {
		return nil, &Rejection{CodeBadSubtitles, "the subtitle file is not UTF-8; re-save it with UTF-8 encoding"}
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(srt), "\r\n", "\n"), "\r", "\n")
	var out strings.Builder
	out.WriteString("WEBVTT\n\n")
	cues := 0
	for _, block := range strings.Split(strings.TrimSpace(text), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		i := 0
		if i < len(lines) && allNums.MatchString(strings.TrimSpace(lines[i])) {
			i++
		}
		if i >= len(lines) {
			continue
		}
		m := srtTime.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		body := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		if body == "" {
			continue
		}
		fmt.Fprintf(&out, "%s.%s --> %s.%s\n%s\n\n", hms(m[1]), pad3(m[2]), hms(m[3]), pad3(m[4]), body)
		cues++
	}
	if cues == 0 {
		return nil, &Rejection{CodeBadSubtitles, "no subtitle cues were found; is this a .srt file?"}
	}
	return []byte(out.String()), nil
}
