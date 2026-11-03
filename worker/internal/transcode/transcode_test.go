package transcode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func names(rs []Rung) string {
	var n []string
	for _, r := range rs {
		n = append(n, r.Name)
	}
	return strings.Join(n, ",")
}

func TestSelectRungs(t *testing.T) {
	cases := []struct {
		short int
		want  string
	}{
		{2160, "240p,360p,480p,720p,1080p"},
		{1080, "240p,360p,480p,720p,1080p"},
		{1079, "240p,360p,480p,720p"},
		{720, "240p,360p,480p,720p"},
		{480, "240p,360p,480p"},
		{360, "240p,360p"},
		{240, "240p"},
		{239, "238p"}, // smaller than the lowest rung: one rung at the source's own (even) size
		{144, "144p"},
		{3, "2p"},
	}
	for _, c := range cases {
		if got := names(SelectRungs(c.short)); got != c.want {
			t.Errorf("short side %d: %s, want %s", c.short, got, c.want)
		}
	}
	for _, r := range SelectRungs(144) {
		if r.Height != 144 || r.VideoKbps != Ladder[0].VideoKbps {
			t.Errorf("a tiny source keeps its size and the data-saver bitrate: %+v", r)
		}
	}
}

func TestLadderIsSane(t *testing.T) {
	prev := Rung{}
	for _, r := range Ladder {
		if r.Height <= prev.Height || r.VideoKbps <= prev.VideoKbps || r.AudioKbps < prev.AudioKbps {
			t.Errorf("rung %s is not above %s", r.Name, prev.Name)
		}
		if r.Height%2 != 0 {
			t.Errorf("%s: odd height", r.Name)
		}
		prev = r
	}
	if total := Ladder[0].VideoKbps + Ladder[0].AudioKbps; total < 250 || total > 320 {
		t.Errorf("the data-saver rung should be about 300 kbps all in, got %d", total)
	}
	if Ladder[0].BandwidthBps() != (275+48)*1000 {
		t.Errorf("bandwidth %d", Ladder[0].BandwidthBps())
	}
}

const probeOK = `{"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"125.5"},"streams":[
 {"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"r_frame_rate":"30000/1001","avg_frame_rate":"30000/1001"},
 {"codec_type":"audio","codec_name":"aac","channels":2}]}`

func policy() Policy {
	return Policy{Version: 1, Containers: []string{"mp4", "mov", "mkv", "webm"},
		VideoCodecs: []string{"h264", "hevc", "vp9"}, AudioCodecs: []string{"aac", "opus"},
		MinDurationSeconds: 1, MaxDurationSeconds: 3600, MaxWidth: 3840, MaxHeight: 2160}
}

func TestParseProbe(t *testing.T) {
	p, err := ParseProbe([]byte(probeOK))
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 1920 || p.Height != 1080 || p.VideoCodec != "h264" || p.AudioCodec != "aac" || p.DurationSecs != 125.5 ||
		p.FPS < 29.9 || p.FPS > 30 || !p.HasAudio() || p.ShortSide() != 1080 || p.FormatNames[0] != "mov" {
		t.Fatalf("%+v", p)
	}
}

func TestParseProbe_RotationAndOddities(t *testing.T) {
	rotated := `{"format":{"format_name":"mov,mp4","duration":"10"},"streams":[{"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"r_frame_rate":"30/1","side_data_list":[{"rotation":-90}]}]}`
	if p, err := ParseProbe([]byte(rotated)); err != nil || p.Width != 1080 || p.Height != 1920 || p.HasAudio() {
		t.Fatalf("a phone video rotated by metadata is portrait: %+v %v", p, err)
	}
	tag := `{"format":{"format_name":"mov,mp4","duration":"10"},"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,"r_frame_rate":"30/1","tags":{"rotate":"270"}}]}`
	if p, _ := ParseProbe([]byte(tag)); p.Width != 720 || p.Height != 1280 {
		t.Fatalf("rotate tag: %+v", p)
	}
	upside := `{"format":{"format_name":"mov,mp4","duration":"10"},"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,"r_frame_rate":"30/1","side_data_list":[{"rotation":180}]}]}`
	if p, _ := ParseProbe([]byte(upside)); p.Width != 1280 {
		t.Fatalf("180 degrees keeps the orientation: %+v", p)
	}
	cover := `{"format":{"format_name":"mov,mp4","duration":"10"},"streams":[
	 {"codec_type":"video","codec_name":"mjpeg","width":500,"height":500,"disposition":{"attached_pic":1}},
	 {"codec_type":"video","codec_name":"h264","width":640,"height":360,"r_frame_rate":"25/1"}]}`
	if p, _ := ParseProbe([]byte(cover)); p.VideoCodec != "h264" || p.Width != 640 {
		t.Fatalf("cover art must be ignored: %+v", p)
	}
	odd := `{"format":{"format_name":"matroska,webm","duration":"10"},"streams":[{"codec_type":"video","codec_name":"vp9","width":640,"height":360,"r_frame_rate":"0/0","avg_frame_rate":"24/1"}]}`
	if p, _ := ParseProbe([]byte(odd)); p.FPS != 24 {
		t.Fatalf("falls back to the average frame rate: %+v", p)
	}
	streamDur := `{"format":{"format_name":"matroska,webm"},"streams":[{"codec_type":"video","codec_name":"vp9","width":640,"height":360,"r_frame_rate":"24/1","duration":"12.5"}]}`
	if p, err := ParseProbe([]byte(streamDur)); err != nil || p.DurationSecs != 12.5 {
		t.Fatalf("stream duration when the container has none: %+v %v", p, err)
	}
}

func TestParseProbe_Refusals(t *testing.T) {
	cases := map[string]struct{ in, code string }{
		"not json":      {`this is not json`, CodeCorruptFile},
		"empty":         {``, CodeCorruptFile},
		"audio only":    {`{"format":{"format_name":"mp3","duration":"10"},"streams":[{"codec_type":"audio","codec_name":"mp3"}]}`, CodeNoVideo},
		"no streams":    {`{"format":{"format_name":"mov,mp4","duration":"10"},"streams":[]}`, CodeNoVideo},
		"no duration":   {`{"format":{"format_name":"mov,mp4"},"streams":[{"codec_type":"video","codec_name":"h264","width":640,"height":360}]}`, CodeCorruptFile},
		"zero size":     {`{"format":{"format_name":"mov,mp4","duration":"5"},"streams":[{"codec_type":"video","codec_name":"h264","width":0,"height":0}]}`, CodeCorruptFile},
		"negative time": {`{"format":{"format_name":"mov,mp4","duration":"-5"},"streams":[{"codec_type":"video","codec_name":"h264","width":64,"height":64}]}`, CodeCorruptFile},
	}
	for name, c := range cases {
		_, err := ParseProbe([]byte(c.in))
		var rj *Rejection
		ok := errors.As(err, &rj)
		if !ok || rj.Code != c.code || rj.Reason == "" {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
}

func TestValidate(t *testing.T) {
	base := func() Probe {
		p, _ := ParseProbe([]byte(probeOK))
		return p
	}
	if r := base().Validate(policy()); r != nil {
		t.Fatalf("a good file: %v", r)
	}
	cases := map[string]struct {
		mod  func(*Probe)
		pol  func(*Policy)
		code string
	}{
		"container":        {func(p *Probe) { p.FormatNames = []string{"avi"} }, nil, CodeUnsupportedFormat},
		"container policy": {nil, func(p *Policy) { p.Containers = []string{"mkv"} }, CodeUnsupportedFormat},
		"video codec":      {func(p *Probe) { p.VideoCodec = "wmv3" }, nil, CodeUnsupportedCodec},
		"audio codec":      {func(p *Probe) { p.AudioCodec = "wmav2" }, nil, CodeUnsupportedCodec},
		"too short":        {func(p *Probe) { p.DurationSecs = 0.4 }, nil, CodeTooShort},
		"too long":         {func(p *Probe) { p.DurationSecs = 3601 }, nil, CodeTooLong},
		"too wide":         {func(p *Probe) { p.Width, p.Height = 7680, 4320 }, nil, CodeResolutionTooBig},
		"portrait 4k+":     {func(p *Probe) { p.Width, p.Height = 4320, 7680 }, nil, CodeResolutionTooBig},
	}
	for name, c := range cases {
		p, pol := base(), policy()
		if c.mod != nil {
			c.mod(&p)
		}
		if c.pol != nil {
			c.pol(&pol)
		}
		r := p.Validate(pol)
		if r == nil || r.Code != c.code || r.Reason == "" {
			t.Errorf("%s: %v, want %s", name, r, c.code)
		}
	}
	silent := base()
	silent.AudioCodec = ""
	if r := silent.Validate(policy()); r != nil {
		t.Errorf("a silent film is fine: %v", r)
	}
	portrait := base()
	portrait.Width, portrait.Height = 2160, 3840
	if r := portrait.Validate(policy()); r != nil {
		t.Errorf("a 4K portrait video is within a 3840x2160 limit: %v", r)
	}
	mkv := base()
	mkv.FormatNames = []string{"matroska", "webm"}
	if r := mkv.Validate(policy()); r != nil {
		t.Errorf("matroska: %v", r)
	}
	lenient := policy()
	lenient.MaxDurationSeconds, lenient.MaxWidth, lenient.MaxHeight = 0, 0, 0
	long := base()
	long.DurationSecs, long.Width, long.Height = 99999, 9999, 9999
	if r := long.Validate(lenient); r != nil {
		t.Errorf("zero limits mean no limit: %v", r)
	}
}

func has(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		ok := true
		for j := range seq {
			ok = ok && args[i+j] == seq[j]
		}
		if ok {
			return true
		}
	}
	return false
}

func TestBuildHLSArgs(t *testing.T) {
	p := Probe{Width: 1280, Height: 720, FPS: 30, VideoCodec: "h264", AudioCodec: "aac", DurationSecs: 60}
	rungs := SelectRungs(p.ShortSide())
	args := BuildHLSArgs("/in/source.mp4", "/out", p, rungs, Options{Threads: 2, Preset: "veryfast"})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-nostdin", "-progress pipe:1", "-i /in/source.mp4",
		"-c:v libx264", "-preset veryfast", "-profile:v main", "-sc_threshold 0",
		"-force_key_frames expr:gte(t,n_forced*6)",
		"-b:v:0 250k", "-maxrate:v:0 275k", "-bufsize:v:0 500k", "-b:v:3 2200k", "-maxrate:v:3 2420k",
		"-c:a aac", "-ac 2", "-ar 48000", "-b:a:0 48k", "-b:a:3 128k",
		"-f hls", "-hls_time 6", "-hls_playlist_type vod", "-hls_segment_type fmp4", "-hls_flags independent_segments",
		"-hls_fmp4_init_filename init.mp4", "-master_pl_name master.m3u8",
		"-hls_segment_filename /out/%v/seg_%05d.m4s",
		"-var_stream_map v:0,a:0,name:240p v:1,a:1,name:360p v:2,a:2,name:480p v:3,a:3,name:720p",
		"/out/%v/index.m3u8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	fg := args[indexOf(args, "-filter_complex")+1]
	for _, want := range []string{"split=4[s0][s1][s2][s3]", "loudnorm=I=-16:TP=-1.5:LRA=11", "asplit=4[a0][a1][a2][a3]",
		"if(gt(iw,ih),-2,240)", "format=yuv420p[v3]"} {
		if !strings.Contains(fg, want) {
			t.Errorf("filter graph lacks %q: %s", want, fg)
		}
	}
	if strings.Contains(fg, "fps=30") {
		t.Errorf("a 30 fps source needs no frame-rate cap: %s", fg)
	}
	if args[len(args)-1] != "/out/%v/index.m3u8" {
		t.Errorf("the output must come last")
	}
	for _, a := range args {
		if strings.Contains(a, "-hls_list_size") {
			t.Error("a VOD playlist keeps every segment")
		}
	}
}

func indexOf(a []string, s string) int {
	for i, v := range a {
		if v == s {
			return i
		}
	}
	return -1
}

func TestBuildHLSArgs_VariantsOfTheSource(t *testing.T) {
	t.Run("60 fps is capped to 30", func(t *testing.T) {
		p := Probe{Width: 640, Height: 360, FPS: 59.94, AudioCodec: "aac"}
		args := BuildHLSArgs("in", "/o", p, SelectRungs(360), Options{Preset: "fast"})
		if fg := args[indexOf(args, "-filter_complex")+1]; !strings.HasPrefix(fg, "[0:v:0]fps=30[vf];[vf]split=2") {
			t.Fatalf("%s", fg)
		}
	})
	t.Run("no audio", func(t *testing.T) {
		p := Probe{Width: 640, Height: 360, FPS: 24}
		args := BuildHLSArgs("in", "/o", p, SelectRungs(360), Options{Preset: "fast"})
		j := strings.Join(args, " ")
		if strings.Contains(j, "aac") || strings.Contains(j, "loudnorm") || strings.Contains(j, "[a0]") || !strings.Contains(j, "-var_stream_map v:0,name:240p v:1,name:360p") {
			t.Fatalf("%s", j)
		}
	})
	t.Run("one rung and no thread limit", func(t *testing.T) {
		p := Probe{Width: 320, Height: 200, FPS: 24, AudioCodec: "aac"}
		args := BuildHLSArgs("in", "/o", p, SelectRungs(200), Options{Preset: "fast"})
		j := strings.Join(args, " ")
		if strings.Contains(j, "-threads") || !strings.Contains(j, "split=1[s0]") || !strings.Contains(j, "name:200p") {
			t.Fatalf("%s", j)
		}
	})
	t.Run("portrait scales by its short side", func(t *testing.T) {
		p := Probe{Width: 720, Height: 1280, FPS: 30}
		args := BuildHLSArgs("in", "/o", p, SelectRungs(p.ShortSide()), Options{Preset: "fast"})
		if fg := args[indexOf(args, "-filter_complex")+1]; !strings.Contains(fg, "w='if(gt(iw,ih),-2,720)':h='if(gt(iw,ih),720,-2)'") {
			t.Fatalf("%s", fg)
		}
	})
}

func TestPosterAndSprite(t *testing.T) {
	a := BuildPosterArgs("in.mp4", "poster.jpg", Probe{DurationSecs: 100})
	if !has(a, "-ss", "10.000") || !has(a, "-frames:v", "1") || a[len(a)-1] != "poster.jpg" {
		t.Fatalf("%v", a)
	}
	if a := BuildPosterArgs("in", "p.jpg", Probe{DurationSecs: 1000}); !has(a, "-ss", "30.000") {
		t.Fatalf("the poster frame is at most 30 s in: %v", a)
	}
	if a := BuildPosterArgs("in", "p.jpg", Probe{DurationSecs: 0.5}); !has(a, "-ss", "0.000") {
		t.Fatalf("a very short clip uses its first frame: %v", a)
	}

	cases := []struct {
		dur                         float64
		interval, count, cols, rows int
	}{
		{10, 5, 2, 2, 1},
		{60, 5, 12, 10, 2},
		{600, 6, 100, 10, 10},
		{7200, 72, 100, 10, 10},
		{0.5, 5, 1, 1, 1},
	}
	for _, c := range cases {
		pl := PlanSprite(c.dur)
		if pl.Interval != c.interval || pl.Count != c.count || pl.Cols != c.cols || pl.Rows != c.rows {
			t.Errorf("%vs: %+v, want %d/%d/%dx%d", c.dur, pl, c.interval, c.count, c.cols, c.rows)
		}
		if pl.Count > 100 {
			t.Errorf("at most 100 thumbnails, got %d", pl.Count)
		}
	}
	args := strings.Join(BuildSpriteArgs("in", "t.jpg", PlanSprite(60)), " ")
	if !strings.Contains(args, "-skip_frame nokey") || !strings.Contains(args, "fps=1/5") || !strings.Contains(args, "tile=10x2") {
		t.Fatalf("%s", args)
	}
	vtt := SpriteVTT("thumbs.jpg", PlanSprite(12), 12)
	want := "WEBVTT\n\n00:00:00.000 --> 00:00:05.000\nthumbs.jpg#xywh=0,0,160,90\n\n00:00:05.000 --> 00:00:10.000\nthumbs.jpg#xywh=160,0,160,90\n\n00:00:10.000 --> 00:00:12.000\nthumbs.jpg#xywh=320,0,160,90\n\n"
	if vtt != want {
		t.Fatalf("\n%s\nwant\n%s", vtt, want)
	}
	if v := SpriteVTT("t.jpg", PlanSprite(120), 120); !strings.Contains(v, "xywh=0,90,160,90") {
		t.Fatalf("the 11th thumbnail starts the second row: %s", v)
	}
}

func TestParseProgress(t *testing.T) {
	cases := []struct {
		line string
		dur  float64
		pct  float64
		ok   bool
	}{
		{"out_time_us=5000000", 10, 50, true},
		{"out_time_ms=2500000", 10, 25, true},
		{"out_time_us=99000000", 10, 100, true},
		{"progress=end", 10, 100, true},
		{"progress=continue", 10, 0, false},
		{"frame=120", 10, 0, false},
		{"out_time_us=N/A", 10, 0, false},
		{"out_time_us=-5", 10, 0, false},
		{"out_time_us=5000000", 0, 0, false},
		{"garbage", 10, 0, false},
	}
	for _, c := range cases {
		pct, ok := ParseProgress(c.line, c.dur)
		if ok != c.ok || (ok && pct != c.pct) {
			t.Errorf("%q: %v %v, want %v %v", c.line, pct, ok, c.pct, c.ok)
		}
	}
}

func TestSRTToVTT(t *testing.T) {
	srt := "\xEF\xBB\xBF1\r\n00:00:01,000 --> 00:00:02,500\r\nHello <i>there</i>\r\n\r\n2\r\n0:00:03,5 --> 0:00:05,25 X1:10\r\nSecond line\r\nthird line\r\n\r\n\r\nnot a cue\r\n\r\n3\r\n00:00:06,000 --> 00:00:07,000\r\n\r\n"
	got, err := SRTToVTT([]byte(srt))
	if err != nil {
		t.Fatal(err)
	}
	want := "WEBVTT\n\n00:00:01.000 --> 00:00:02.500\nHello <i>there</i>\n\n00:00:03.500 --> 00:00:05.250\nSecond line\nthird line\n\n"
	if string(got) != want {
		t.Fatalf("\n%q\nwant\n%q", got, want)
	}
	utf := "1\n00:00:01,000 --> 00:00:02,000\nKiswahili: Habari yako? Français: ça va, où est l'hôtel? 日本語\n\n"
	if g, err := SRTToVTT([]byte(utf)); err != nil || !strings.Contains(string(g), "où est l'hôtel? 日本語") {
		t.Fatalf("non-ASCII text must survive: %s %v", g, err)
	}
	for name, in := range map[string][]byte{
		"latin-1":     []byte("1\n00:00:01,000 --> 00:00:02,000\nfran\xe7ais\n\n"),
		"no cues":     []byte("just some text\n"),
		"empty":       nil,
		"only a time": []byte("1\n00:00:01,000 --> 00:00:02,000\n\n"),
		"too large":   append([]byte("1\n00:00:01,000 --> 00:00:02,000\n"), make([]byte, MaxSubtitleBytes)...),
	} {
		_, err := SRTToVTT(in)
		var rj *Rejection
		if !errors.As(err, &rj) || rj.Code != CodeBadSubtitles {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestContentTypeAndCache(t *testing.T) {
	for name, want := range map[string][3]string{
		"master.m3u8":        {"application/vnd.apple.mpegurl", "public, max-age=300"},
		"360p/index.m3u8":    {"application/vnd.apple.mpegurl", "public, max-age=300"},
		"360p/init.mp4":      {"video/mp4", "public, max-age=31536000, immutable"},
		"360p/seg_00001.m4s": {"video/mp4", "public, max-age=31536000, immutable"},
		"poster.jpg":         {"image/jpeg", "public, max-age=31536000, immutable"},
		"thumbs.vtt":         {"text/vtt; charset=utf-8", "public, max-age=300"},
		"mystery.bin":        {"application/octet-stream", "public, max-age=31536000, immutable"},
	} {
		if ContentType(name) != want[0] || CacheControl(name) != want[1] {
			t.Errorf("%s: %s / %s", name, ContentType(name), CacheControl(name))
		}
	}
}

func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestVerifyHLS(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.000000,\nseg_00000.m4s\n#EXTINF:4.000000,\nseg_00001.m4s\n#EXT-X-ENDLIST\n"
	good := map[string]string{
		"master.m3u8":     "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n240p/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2\n360p/index.m3u8\n",
		"240p/index.m3u8": media, "240p/init.mp4": "i", "240p/seg_00000.m4s": "a", "240p/seg_00001.m4s": "b",
		"360p/index.m3u8": media, "360p/init.mp4": "i", "360p/seg_00000.m4s": "a", "360p/seg_00001.m4s": "b",
	}
	rungs := SelectRungs(360)
	out, err := VerifyHLS(writePkg(t, good), rungs, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 9 || out.Files[len(out.Files)-1] != "360p/index.m3u8" || out.Files[len(out.Files)-3] != "master.m3u8" {
		t.Fatalf("media files first, then master, then the media playlists: %v", out.Files)
	}
	for _, f := range out.Files[:6] {
		if strings.HasSuffix(f, ".m3u8") {
			t.Fatalf("a playlist was listed before the media files: %v", out.Files)
		}
	}

	mutate := func(m func(map[string]string)) map[string]string {
		c := map[string]string{}
		for k, v := range good {
			c[k] = v
		}
		m(c)
		return c
	}
	bad := map[string]map[string]string{
		"no master":       mutate(func(c map[string]string) { delete(c, "master.m3u8") }),
		"not a playlist":  mutate(func(c map[string]string) { c["master.m3u8"] = "hello" }),
		"missing variant": mutate(func(c map[string]string) { c["master.m3u8"] = "#EXTM3U\n240p/index.m3u8\n" }),
		"wrong variant":   mutate(func(c map[string]string) { c["master.m3u8"] = "#EXTM3U\n240p/index.m3u8\n480p/index.m3u8\n" }),
		"missing segment": mutate(func(c map[string]string) { delete(c, "360p/seg_00001.m4s") }),
		"empty segment":   mutate(func(c map[string]string) { c["240p/seg_00000.m4s"] = "" }),
		"missing init":    mutate(func(c map[string]string) { delete(c, "240p/init.mp4") }),
		"unfinished":      mutate(func(c map[string]string) { c["240p/index.m3u8"] = strings.Replace(media, "#EXT-X-ENDLIST\n", "", 1) }),
		"no segments":     mutate(func(c map[string]string) { c["240p/index.m3u8"] = "#EXTM3U\n#EXT-X-ENDLIST\n" }),
		"wrong total":     mutate(func(c map[string]string) { c["240p/index.m3u8"] = strings.Replace(media, "4.000000", "40.000000", 1) }),
		"map without URI": mutate(func(c map[string]string) { c["240p/index.m3u8"] = strings.Replace(media, `URI="init.mp4"`, "", 1) }),
	}
	for name, files := range bad {
		if _, err := VerifyHLS(writePkg(t, files), rungs, 10); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
