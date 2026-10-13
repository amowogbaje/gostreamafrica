package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
)

func TestFilters(t *testing.T) {
	f := newFx(t)
	f.title("yo-movie-drama", func(s *tspec) { s.lang = "yo"; s.genres = []string{"drama"}; s.published = f.day(-1) })
	f.title("yo-series-epic", func(s *tspec) {
		s.lang = "yo"
		s.typ = "series"
		s.genres = []string{"epic-historical", "drama"}
		s.published = f.day(-2)
	})
	f.title("ig-movie-family", func(s *tspec) { s.lang = "ig"; s.genres = []string{"family"}; s.published = f.day(-3) })
	f.title("pcm-movie-comedy", func(s *tspec) { s.lang = "pcm"; s.genres = []string{"comedy"}; s.published = f.day(-4) })
	cases := []struct {
		name string
		p    ListParams
		want []string
	}{
		{"no filter", ListParams{}, []string{"yo-movie-drama", "yo-series-epic", "ig-movie-family", "pcm-movie-comedy"}},
		{"type movie", ListParams{Type: "movie"}, []string{"yo-movie-drama", "ig-movie-family", "pcm-movie-comedy"}},
		{"type series", ListParams{Type: "series"}, []string{"yo-series-epic"}},
		{"language yo", ListParams{Language: "yo"}, []string{"yo-movie-drama", "yo-series-epic"}},
		{"language pcm", ListParams{Language: "pcm"}, []string{"pcm-movie-comedy"}},
		{"genre drama", ListParams{Genre: "drama"}, []string{"yo-movie-drama", "yo-series-epic"}},
		{"genre epic", ListParams{Genre: "epic-historical"}, []string{"yo-series-epic"}},
		{"genre + language", ListParams{Genre: "drama", Language: "yo", Type: "movie"}, []string{"yo-movie-drama"}},
		{"unknown genre", ListParams{Genre: "no-such-genre"}, nil},
		{"unknown language", ListParams{Language: "zz"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := slugs(f.list(Viewer{}, c.p).Items); !equal(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
	// each card carries its genre slugs
	for _, it := range f.list(Viewer{}, ListParams{Genre: "epic-historical"}).Items {
		if !equal(it.Genres, []string{"drama", "epic-historical"}) {
			t.Fatalf("genres on the card: %v", it.Genres)
		}
	}
}

func TestSearch(t *testing.T) {
	f := newFx(t)
	f.title("omo", func(s *tspec) { s.name = "Ọmọ Aládé"; s.synopsis = "a young prince returns" })
	f.title("ilu", func(s *tspec) { s.name = "Ìlú Òkè"; s.synopsis = "rival families on a hilltop" })
	f.title("name-hit", func(s *tspec) { s.name = "Prince of Lagos"; s.synopsis = "a story" })
	f.title("synopsis-hit", func(s *tspec) { s.name = "Another Film"; s.synopsis = "the prince of nothing, a prince indeed" })
	f.title("lagos", func(s *tspec) { s.name = "Lagos After Midnight" })

	search := func(q string) []string {
		return slugs(f.list(Viewer{}, ListParams{Q: q, Sort: SortRelevance}).Items)
	}
	if got := search("omo alade"); !equal(got, []string{"omo"}) {
		t.Fatalf("accent-insensitive search for 'omo alade': %v", got)
	}
	if got := search("Ọmọ"); !equal(got, []string{"omo"}) {
		t.Fatalf("search typed with accents: %v", got)
	}
	if got := search("ILU OKE"); !equal(got, []string{"ilu"}) {
		t.Fatalf("case and accent insensitive: %v", got)
	}
	// a match in the NAME (weight A) outranks a match only in the synopsis (weight B)
	if got := search("prince"); len(got) < 3 || got[0] != "name-hit" {
		t.Fatalf("name matches must rank first: %v", got)
	}
	if got := search("lagos midnight"); !equal(got, []string{"lagos"}) {
		t.Fatalf("all terms must match: %v", got)
	}
	if got := search("zzzzzz"); len(got) != 0 {
		t.Fatalf("no match: %v", got)
	}
	// operators and punctuation never break the query or reach SQL
	for _, q := range []string{`a' OR 1=1 --`, `"unbalanced`, `:* & | !`, `'); DROP TABLE titles;--`, `\`, `((((`, `-`, `日本語`} {
		if _, err := f.svc.List(f.ctx, Viewer{}, ListParams{Q: q, Sort: SortRelevance, Limit: 10}); err != nil {
			t.Errorf("q=%q: %v", q, err)
		}
	}
	var n int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM titles`).Scan(&n)
	if n != 5 {
		t.Fatalf("titles table damaged: %d rows", n)
	}
	// a search can also be sorted by name or newest
	if got := slugs(f.list(Viewer{}, ListParams{Q: "prince", Sort: SortName}).Items); len(got) != 3 {
		t.Fatalf("search sorted by name: %v", got)
	}
}

func TestDetail_MovieAndSeries(t *testing.T) {
	f := newFx(t)
	movie := f.title("the-movie", func(s *tspec) {
		s.name = "The Movie"
		s.synopsis = "Full synopsis."
		s.genres = []string{"thriller", "drama"}
	})
	f.exec(`UPDATE titles SET release_year = 2023, maturity_rating = '15', runtime_minutes = 112, poster_url = 'https://cdn.example.com/p.jpg', backdrop_url = 'https://cdn.example.com/b.jpg' WHERE id = $1::uuid`, movie)
	person := func(name string) string {
		var id string
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO people (name) VALUES ($1) RETURNING id::text`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	dir, wri, c1, c2 := person("Dara Director"), person("Wale Writer"), person("Ada Lead"), person("Ben Second")
	f.exec(`INSERT INTO title_people (title_id, person_id, role, character_name, billing_order) VALUES
	        ($1::uuid, $2::uuid, 'director', NULL, 0), ($1::uuid, $3::uuid, 'writer', NULL, 0),
	        ($1::uuid, $5::uuid, 'cast', 'Second Role', 1), ($1::uuid, $4::uuid, 'cast', 'Lead Role', 0)`, movie, dir, wri, c1, c2)
	f.exec(`INSERT INTO media_assets (title_id, status, hls_manifest_key, duration_seconds) VALUES ($1::uuid, 'ready', 'hls/the-movie/master.m3u8', 6720)`, movie)
	f.exec(`INSERT INTO subtitles (asset_id, language, label, vtt_url) SELECT id, v.l, v.n, 'https://cdn.example.com/' || v.l || '.vtt'
	        FROM media_assets, (VALUES ('yo','Yoruba'), ('en','English')) v(l, n) WHERE title_id = $1::uuid`, movie)

	d, err := f.svc.Detail(f.ctx, Viewer{}, "the-movie")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "The Movie" || *d.ReleaseYear != 2023 || *d.MaturityRating != "15" || *d.RuntimeMinutes != 112 ||
		*d.Synopsis != "Full synopsis." || *d.BackdropURL != "https://cdn.example.com/b.jpg" || d.Type != "movie" {
		t.Fatalf("core fields: %+v", d)
	}
	if len(d.Genres) != 2 || d.Genres[0].Slug != "drama" || d.Genres[1].Name != "Thriller" {
		t.Fatalf("genres (sorted by name): %+v", d.Genres)
	}
	if len(d.Directors) != 1 || d.Directors[0].Name != "Dara Director" || len(d.Writers) != 1 {
		t.Fatalf("crew: %+v %+v", d.Directors, d.Writers)
	}
	if len(d.Cast) != 2 || d.Cast[0].Name != "Ada Lead" || *d.Cast[0].Character != "Lead Role" || d.Cast[1].Order != 1 {
		t.Fatalf("cast must be in billing order: %+v", d.Cast)
	}
	if !d.Playable || len(d.Subtitles) != 2 || d.Subtitles[0].Language != "en" || d.Subtitles[1].URL != "https://cdn.example.com/yo.vtt" {
		t.Fatalf("media: playable=%v subtitles=%+v", d.Playable, d.Subtitles)
	}
	if len(d.Seasons) != 0 {
		t.Fatalf("a movie has no seasons: %+v", d.Seasons)
	}

	// a movie whose asset is still processing is not playable
	f.exec(`UPDATE media_assets SET status = 'processing', hls_manifest_key = NULL, duration_seconds = NULL WHERE title_id = $1::uuid`, movie)
	if d, _ := f.svc.Detail(f.ctx, Viewer{}, "the-movie"); d.Playable {
		t.Fatal("a processing asset must not be playable")
	}

	// series
	series := f.title("the-series", func(s *tspec) { s.typ = "series"; s.name = "The Series" })
	var s1, s2 string
	_ = f.pool.QueryRow(f.ctx, `INSERT INTO seasons (title_id, number, name) VALUES ($1::uuid, 1, 'Season 1') RETURNING id::text`, series).Scan(&s1)
	_ = f.pool.QueryRow(f.ctx, `INSERT INTO seasons (title_id, number) VALUES ($1::uuid, 2) RETURNING id::text`, series).Scan(&s2)
	var ep1 string
	_ = f.pool.QueryRow(f.ctx, `INSERT INTO episodes (season_id, title_id, number, name, runtime_minutes, air_date) VALUES ($1::uuid, $2::uuid, 1, 'Pilot', 35, '2024-02-01') RETURNING id::text`, s1, series).Scan(&ep1)
	f.exec(`INSERT INTO episodes (season_id, title_id, number, name) VALUES ($1::uuid, $2::uuid, 2, 'Second')`, s1, series)
	f.exec(`INSERT INTO media_assets (episode_id, status, hls_manifest_key, duration_seconds) VALUES ($1::uuid, 'ready', 'k', 2100)`, ep1)
	f.exec(`INSERT INTO subtitles (asset_id, language, label, vtt_url) SELECT id, 'en', 'English', 'https://cdn.example.com/e1.vtt' FROM media_assets WHERE episode_id = $1::uuid`, ep1)

	sd, err := f.svc.Detail(f.ctx, Viewer{}, "the-series")
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.Seasons) != 2 || sd.Seasons[0].Number != 1 || *sd.Seasons[0].Name != "Season 1" || len(sd.Seasons[0].Episodes) != 2 {
		t.Fatalf("seasons: %+v", sd.Seasons)
	}
	e1, e2 := sd.Seasons[0].Episodes[0], sd.Seasons[0].Episodes[1]
	if e1.Name != "Pilot" || !e1.Playable || *e1.AirDate != "2024-02-01" || *e1.RuntimeMinutes != 35 || len(e1.Subtitles) != 1 {
		t.Fatalf("episode 1: %+v", e1)
	}
	if e2.Playable || len(e2.Subtitles) != 0 || e2.Subtitles == nil {
		t.Fatalf("episode 2 (no asset) must be unplayable with empty subtitles: %+v", e2)
	}
	if sd.Seasons[1].Episodes == nil || len(sd.Seasons[1].Episodes) != 0 {
		t.Fatalf("an empty season must render episodes as []: %+v", sd.Seasons[1])
	}
	if len(sd.Subtitles) != 0 || sd.Playable {
		t.Fatalf("series-level playable/subtitles must be empty: %+v", sd)
	}

	// a title with nothing attached renders [] and never null
	f.title("bare", nil)
	bare, _ := f.svc.Detail(f.ctx, Viewer{}, "bare")
	raw, _ := json.Marshal(bare)
	for _, field := range []string{`"genres":[]`, `"directors":[]`, `"writers":[]`, `"cast":[]`, `"subtitles":[]`, `"seasons":[]`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("bare title JSON lacks %s: %s", field, raw)
		}
	}
}

func TestDetail_ViewerState(t *testing.T) {
	f := newFx(t)
	id := f.title("watched", nil)
	v := f.as(f.viewerA, auth.RoleViewer)
	if d, _ := f.svc.Detail(f.ctx, v, "watched"); d.Viewer == nil || d.Viewer.InWatchlist || len(d.Viewer.Progress) != 0 {
		t.Fatalf("fresh viewer state: %+v", d.Viewer)
	}
	if _, _, err := f.svc.WatchlistAdd(f.ctx, v, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SaveProgress(f.ctx, v, ProgressUpdate{TitleID: id, PositionSeconds: 30, DurationSeconds: 100}); err != nil {
		t.Fatal(err)
	}
	d, _ := f.svc.Detail(f.ctx, v, "watched")
	if !d.Viewer.InWatchlist || len(d.Viewer.Progress) != 1 || d.Viewer.Progress[0].PositionSeconds != 30 || d.Viewer.Progress[0].EpisodeID != nil {
		t.Fatalf("viewer state: %+v", d.Viewer)
	}
	// someone else's state never leaks
	if other, _ := f.svc.Detail(f.ctx, f.as(f.viewerB, auth.RoleViewer), "watched"); other.Viewer.InWatchlist || len(other.Viewer.Progress) != 0 {
		t.Fatalf("another viewer's state leaked: %+v", other.Viewer)
	}
}

func TestGenres_CountOnlyPublishedAndIncludeEmpty(t *testing.T) {
	f := newFx(t)
	f.title("g1", func(s *tspec) { s.genres = []string{"drama", "comedy"} })
	f.title("g2", func(s *tspec) { s.genres = []string{"drama"} })
	f.title("g3", func(s *tspec) { s.genres = []string{"drama"}; s.status = "draft" })
	f.title("g4", func(s *tspec) { s.genres = []string{"horror"}; s.status = "unpublished" })
	body, err := f.svc.GenresJSON(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Items []GenreCount }
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, g := range out.Items {
		counts[g.Slug] = g.TitleCount
	}
	if len(out.Items) != 10 || counts["drama"] != 2 || counts["comedy"] != 1 || counts["horror"] != 0 || counts["documentary"] != 0 {
		t.Fatalf("counts: %v (%d genres)", counts, len(out.Items))
	}
	for i := 1; i < len(out.Items); i++ {
		if out.Items[i-1].Name > out.Items[i].Name {
			t.Fatalf("genres must be sorted by name: %v", out.Items)
		}
	}
}

func TestHome(t *testing.T) {
	f := newFx(t)
	now := time.Now()
	mk := func(slug, lang string, days int) string {
		return f.title(slug, func(s *tspec) { s.lang = lang; s.published = f.day(days) })
	}
	a := mk("a-yo", "yo", -1)
	b := mk("b-yo", "yo", -2)
	mk("c-yo", "yo", -3)
	mk("d-en", "en", -4)
	e := mk("e-en", "en", -5)
	mk("f-ig", "ig", -6)
	f.title("hidden-draft", func(s *tspec) { s.status = "draft"; s.lang = "ha" })

	// trending = distinct recent viewers: a has 3, b has 1; e has 2 viewers but only OLD activity
	u3 := f.user("viewer")
	for _, u := range []string{f.viewerA, f.viewerB, u3} {
		f.progress(u, a, nil, 10, 100, false, now.Add(-time.Hour))
	}
	f.progress(f.viewerA, b, nil, 10, 100, false, now.Add(-2*time.Hour))
	f.progress(f.viewerA, e, nil, 10, 100, false, now.Add(-30*24*time.Hour))
	f.progress(f.viewerB, e, nil, 10, 100, false, now.Add(-30*24*time.Hour))

	body, shared, err := f.svc.HomeJSON(f.ctx, Viewer{})
	if err != nil || !shared {
		t.Fatalf("anonymous home: shared=%v err=%v", shared, err)
	}
	var h Home
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	var keys []string
	rows := map[string]HomeRow{}
	for _, r := range h.Rows {
		keys = append(keys, r.Key)
		rows[r.Key] = r
		if r.Kind != "titles" || len(r.Items) == 0 || len(r.Items) > 12 {
			t.Fatalf("bad row %+v", r)
		}
	}
	if !equal(keys, []string{"trending", "new-releases", "language-yo", "language-en", "language-ig"}) {
		t.Fatalf("rows (languages ordered by number of titles): %v", keys)
	}
	trend := slugs(rows["trending"].Items)
	if trend[0] != "a-yo" || trend[1] != "b-yo" {
		t.Fatalf("trending must rank by recent distinct viewers, ignoring old activity: %v", trend)
	}
	if len(trend) != 6 || trend[2] != "c-yo" {
		t.Fatalf("zero-view titles fill the row newest-first: %v", trend)
	}
	if got := slugs(rows["new-releases"].Items); got[0] != "a-yo" || got[5] != "f-ig" {
		t.Fatalf("new releases: %v", got)
	}
	if got := slugs(rows["language-yo"].Items); !equal(got, []string{"a-yo", "b-yo", "c-yo"}) {
		t.Fatalf("yoruba row: %v", got)
	}
	if rows["language-yo"].Title != "Yoruba" || *rows["language-yo"].Language != "yo" {
		t.Fatalf("row labels: %+v", rows["language-yo"])
	}
	for _, r := range h.Rows {
		for _, it := range r.Items {
			if it.Slug == "hidden-draft" {
				t.Fatal("a draft leaked into the home page")
			}
		}
	}
	for _, r := range h.Rows {
		if r.Key == "continue-watching" {
			t.Fatal("anonymous visitors have no continue-watching row")
		}
	}

	// signed in: a live Continue Watching row goes first
	body, shared, err = f.svc.HomeJSON(f.ctx, f.as(f.viewerA, auth.RoleViewer))
	if err != nil || shared {
		t.Fatalf("authenticated home: shared=%v err=%v", shared, err)
	}
	var hv Home
	_ = json.Unmarshal(body, &hv)
	if hv.Rows[0].Key != "continue-watching" || hv.Rows[0].Kind != "continue" || len(hv.Rows[0].Continue) != 3 || len(hv.Rows[0].Items) != 0 {
		t.Fatalf("continue row: %+v", hv.Rows[0])
	}
	if len(hv.Rows) != len(h.Rows)+1 {
		t.Fatalf("a signed-in home is the public rows plus one: %d vs %d", len(hv.Rows), len(h.Rows))
	}
	// a viewer with nothing in progress gets the public home unchanged
	if _, _, err := f.svc.HomeJSON(f.ctx, f.as(f.user("viewer"), auth.RoleViewer)); err != nil {
		t.Fatal(err)
	}
}

func TestHome_EmptyCatalogRendersEmptyRows(t *testing.T) {
	f := newFx(t)
	body, _, err := f.svc.HomeJSON(f.ctx, Viewer{})
	if err != nil || strings.TrimSpace(string(body)) != `{"rows":[]}` {
		t.Fatalf("%s %v", body, err)
	}
}
