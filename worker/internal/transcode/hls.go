package transcode

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Output is a verified HLS package on disk.
type Output struct {
	Files []string // paths relative to the package directory, segments first, playlists last
}

// VerifyHLS checks a finished package: the master playlist lists every expected rung, every playlist
// ends with #EXT-X-ENDLIST, every referenced init file and segment exists and is not empty, and the
// segment durations add up to the source duration. It returns the files in upload order: media files
// first, playlists last, so a half-uploaded package never has a playlist pointing at missing data.
func VerifyHLS(dir string, rungs []Rung, duration float64) (Output, error) {
	master, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		return Output{}, fmt.Errorf("master playlist missing: %w", err)
	}
	if !strings.HasPrefix(string(master), "#EXTM3U") {
		return Output{}, fmt.Errorf("master playlist is not an HLS playlist")
	}
	var variants []string
	sc := bufio.NewScanner(strings.NewReader(string(master)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			variants = append(variants, line)
		}
	}
	if len(variants) != len(rungs) {
		return Output{}, fmt.Errorf("master lists %d variants, expected %d", len(variants), len(rungs))
	}
	var media, playlists []string
	playlists = append(playlists, "master.m3u8")
	for i, v := range variants {
		if want := rungs[i].Name + "/index.m3u8"; v != want {
			return Output{}, fmt.Errorf("variant %d is %q, expected %q", i, v, want)
		}
		files, err := verifyMediaPlaylist(dir, v, duration)
		if err != nil {
			return Output{}, fmt.Errorf("%s: %w", v, err)
		}
		media = append(media, files...)
		playlists = append(playlists, v)
	}
	return Output{Files: append(media, playlists...)}, nil
}

func verifyMediaPlaylist(dir, rel string, duration float64) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return nil, err
	}
	base := path.Dir(rel)
	var files []string
	var total float64
	segs, ended := 0, false
	seen := map[string]bool{}
	add := func(name string) error {
		name = path.Join(base, name)
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("referenced file missing: %s", name)
		}
		if fi.Size() == 0 {
			return fmt.Errorf("referenced file is empty: %s", name)
		}
		if !seen[name] {
			seen[name] = true
			files = append(files, name)
		}
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			uri := attr(line, "URI")
			if uri == "" {
				return nil, fmt.Errorf("EXT-X-MAP without URI")
			}
			if err := add(uri); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			d, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			if err != nil {
				d2, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
				if d, err = strconv.ParseFloat(d2, 64); err != nil {
					return nil, fmt.Errorf("bad EXTINF %q", line)
				}
			}
			total += d
		case line == "#EXT-X-ENDLIST":
			ended = true
		case line != "" && !strings.HasPrefix(line, "#"):
			segs++
			if err := add(line); err != nil {
				return nil, err
			}
		}
	}
	if !ended {
		return nil, fmt.Errorf("playlist is not finished (no #EXT-X-ENDLIST)")
	}
	if segs == 0 {
		return nil, fmt.Errorf("playlist has no segments")
	}
	if diff := total - duration; diff > 2 || diff < -2 {
		return nil, fmt.Errorf("segments add up to %.1fs but the video is %.1fs", total, duration)
	}
	return files, nil
}

func attr(line, name string) string {
	i := strings.Index(line, name+`="`)
	if i < 0 {
		return ""
	}
	rest := line[i+len(name)+2:]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// ContentType and CacheControl give the HTTP headers each output file is stored with.
// Playlists are short-lived; media files never change once written (a new asset gets a new id).
func ContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".m4s", ".mp4":
		return "video/mp4"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".vtt":
		return "text/vtt; charset=utf-8"
	}
	return "application/octet-stream"
}

// CacheControl returns the Cache-Control header for an output file.
func CacheControl(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".m3u8", ".vtt":
		return "public, max-age=300"
	}
	return "public, max-age=31536000, immutable"
}
