package catalog

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"streamafrica/api/internal/auth"
)

func TestWatchlist_AddIsIdempotentAndOrdered(t *testing.T) {
	f := newFx(t)
	v := f.as(f.viewerA, auth.RoleViewer)
	ids := map[string]string{}
	for _, s := range []string{"w1", "w2", "w3"} {
		ids[s] = f.title(s, nil)
	}
	// add with explicit, spaced timestamps so ordering is unambiguous
	for i, s := range []string{"w1", "w2", "w3"} {
		f.exec(`INSERT INTO watchlist (user_id, title_id, added_at) VALUES ($1::uuid, $2::uuid, $3)`,
			f.viewerA, ids[s], f.base.Add(time.Duration(i)*time.Hour))
	}
	// adding an existing title: not created, original timestamp kept
	at, created, err := f.svc.WatchlistAdd(f.ctx, v, ids["w1"])
	if err != nil || created || !at.Equal(f.base) {
		t.Fatalf("re-add: at=%v created=%v err=%v", at, created, err)
	}
	w4 := f.title("w4", nil)
	if _, created, err := f.svc.WatchlistAdd(f.ctx, v, w4); err != nil || !created {
		t.Fatalf("first add: created=%v err=%v", created, err)
	}
	page, err := f.svc.Watchlist(f.ctx, v, nil, 10)
	if err != nil || page.NextCursor != nil {
		t.Fatal(err, page.NextCursor)
	}
	var got []string
	for _, it := range page.Items {
		got = append(got, it.Slug)
	}
	if !equal(got, []string{"w4", "w3", "w2", "w1"}) {
		t.Fatalf("newest first: %v", got)
	}
	if page.Items[3].AddedAt.IsZero() || page.Items[3].Name != "w1" || page.Items[0].Genres == nil {
		t.Fatalf("items must be full cards with added_at: %+v", page.Items[3])
	}
	// someone else's list is separate
	if other, _ := f.svc.Watchlist(f.ctx, f.as(f.viewerB, auth.RoleViewer), nil, 10); len(other.Items) != 0 {
		t.Fatalf("watchlists leaked between users: %+v", other.Items)
	}
}

func TestWatchlist_PaginationWithTies(t *testing.T) {
	f := newFx(t)
	v := f.as(f.viewerA, auth.RoleViewer)
	for i := 0; i < 7; i++ {
		id := f.title(fmt.Sprintf("p%d", i), nil)
		// all added in the same instant: the title id must break the tie
		f.exec(`INSERT INTO watchlist (user_id, title_id, added_at) VALUES ($1::uuid, $2::uuid, $3)`, f.viewerA, id, f.base)
	}
	var all []string
	var after *cursor
	for pages := 0; ; pages++ {
		page, err := f.svc.Watchlist(f.ctx, v, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			all = append(all, it.Slug)
		}
		if page.NextCursor == nil {
			if pages != 3 {
				t.Fatalf("%d pages, want 4", pages+1)
			}
			break
		}
		var err2 error
		if after, err2 = parseCursor(*page.NextCursor, "watchlist", ""); err2 != nil {
			t.Fatal(err2)
		}
	}
	seen := map[string]bool{}
	for _, s := range all {
		if seen[s] {
			t.Fatalf("%s repeated", s)
		}
		seen[s] = true
	}
	if len(all) != 7 {
		t.Fatalf("got %d of 7: %v", len(all), all)
	}
}

func TestWatchlist_RemoveIsIdempotentAndOwnersOnly(t *testing.T) {
	f := newFx(t)
	a, b := f.as(f.viewerA, auth.RoleViewer), f.as(f.viewerB, auth.RoleViewer)
	id := f.title("rm", nil)
	_, _, _ = f.svc.WatchlistAdd(f.ctx, a, id)
	_, _, _ = f.svc.WatchlistAdd(f.ctx, b, id)
	// B removing does not touch A's entry
	if err := f.svc.WatchlistRemove(f.ctx, b, id); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.svc.Watchlist(f.ctx, a, nil, 10); len(p.Items) != 1 {
		t.Fatal("removing from one list removed from another")
	}
	for i := 0; i < 2; i++ { // second removal is a no-op, not an error
		if err := f.svc.WatchlistRemove(f.ctx, a, id); err != nil {
			t.Fatalf("remove #%d: %v", i+1, err)
		}
	}
	if p, _ := f.svc.Watchlist(f.ctx, a, nil, 10); len(p.Items) != 0 {
		t.Fatal("entry not removed")
	}
}

func TestWatchlist_RulesOnAdd(t *testing.T) {
	f := newFx(t)
	v := f.as(f.viewerA, auth.RoleViewer)
	draft := f.title("hidden", func(s *tspec) { s.status = "draft" })
	if _, _, err := f.svc.WatchlistAdd(f.ctx, v, draft); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a viewer cannot save a draft: %v", err)
	}
	if _, _, err := f.svc.WatchlistAdd(f.ctx, v, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown title: %v", err)
	}
	// the owner may save their own draft (to preview it)
	if _, created, err := f.svc.WatchlistAdd(f.ctx, f.as(f.creatorA, auth.RoleCreator), draft); err != nil || !created {
		t.Fatalf("owner: %v %v", created, err)
	}
}

func TestWatchlist_SizeLimit(t *testing.T) {
	f := newFx(t)
	v := f.as(f.viewerA, auth.RoleViewer)
	first := f.title("cap-0", nil)
	f.exec(`INSERT INTO titles (type, slug, name, status, creator_id, published_at)
	        SELECT 'movie', 'cap-' || n, 'cap ' || n, 'published', $1::uuid, now() FROM generate_series(1, $2) n`, f.creatorA, MaxWatchlist)
	f.exec(`INSERT INTO watchlist (user_id, title_id) SELECT $1::uuid, id FROM titles WHERE slug <> 'cap-0'`, f.viewerA)
	if _, _, err := f.svc.WatchlistAdd(f.ctx, v, first); !errors.Is(err, ErrWatchlistFull) {
		t.Fatalf("501st entry: %v", err)
	}
	// re-adding something already on a full list is still fine
	var existing string
	_ = f.pool.QueryRow(f.ctx, `SELECT title_id::text FROM watchlist WHERE user_id = $1::uuid LIMIT 1`, f.viewerA).Scan(&existing)
	if _, created, err := f.svc.WatchlistAdd(f.ctx, v, existing); err != nil || created {
		t.Fatalf("existing entry on a full list: created=%v err=%v", created, err)
	}
}

func TestProgress_UpsertBehaviour(t *testing.T) {
	f := newFx(t)
	v := f.as(f.viewerA, auth.RoleViewer)
	id := f.title("movie", nil)
	save := func(pos, dur int) Progress {
		t.Helper()
		p, err := f.svc.SaveProgress(f.ctx, v, ProgressUpdate{TitleID: id, PositionSeconds: pos, DurationSeconds: dur})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	rows := func() int {
		var n int
		_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM watch_progress`).Scan(&n)
		return n
	}

	p1 := save(100, 1000)
	if p1.PositionSeconds != 100 || p1.Completed || p1.EpisodeID != nil || p1.TitleID != id {
		t.Fatalf("create: %+v", p1)
	}
	time.Sleep(20 * time.Millisecond)

	// IDEMPOTENT: the same report again changes nothing, not even updated_at
	p2 := save(100, 1000)
	if !p2.UpdatedAt.Equal(p1.UpdatedAt) || rows() != 1 {
		t.Fatalf("an identical update must be a no-op: %v vs %v (rows=%d)", p2.UpdatedAt, p1.UpdatedAt, rows())
	}

	// a new position updates the SAME row and moves updated_at forward
	p3 := save(250, 1000)
	if p3.PositionSeconds != 250 || !p3.UpdatedAt.After(p1.UpdatedAt) || rows() != 1 {
		t.Fatalf("update: %+v rows=%d", p3, rows())
	}
	// going backwards is allowed (the viewer seeked back)
	if p := save(40, 1000); p.PositionSeconds != 40 {
		t.Fatalf("seek back: %+v", p)
	}

	// completion at 95%
	if p := save(949, 1000); p.Completed {
		t.Fatal("94.9% must not be completed")
	}
	if p := save(950, 1000); !p.Completed {
		t.Fatal("95% must be completed")
	}
	if p := save(1000, 1000); !p.Completed || p.PositionSeconds != 1000 {
		t.Fatalf("end: %+v", p)
	}
	// position beyond the end is clamped to the duration
	if p := save(5000, 1000); p.PositionSeconds != 1000 || !p.Completed {
		t.Fatalf("clamp: %+v", p)
	}
	// rewatching from the start clears "completed" so it resumes properly
	if p := save(5, 1000); p.Completed || p.PositionSeconds != 5 {
		t.Fatalf("rewatch: %+v", p)
	}
	// the duration may be corrected by the player
	if p := save(5, 1200); p.DurationSeconds != 1200 {
		t.Fatalf("duration update: %+v", p)
	}
	if rows() != 1 {
		t.Fatalf("every update must reuse the one row, found %d", rows())
	}
	// progress is per user
	if _, err := f.svc.SaveProgress(f.ctx, f.as(f.viewerB, auth.RoleViewer), ProgressUpdate{TitleID: id, PositionSeconds: 1, DurationSeconds: 10}); err != nil {
		t.Fatal(err)
	}
	if rows() != 2 {
		t.Fatalf("a second user must get their own row: %d", rows())
	}
}

func TestProgress_SeriesAndEpisodes(t *testing.T) {
	f := newFx(t)
	v := f.as(f.viewerA, auth.RoleViewer)
	series := f.title("show", func(s *tspec) { s.typ = "series" })
	other := f.title("other-show", func(s *tspec) { s.typ = "series" })
	movie := f.title("film", nil)
	mkEp := func(title string, n int) string {
		var season, ep string
		_ = f.pool.QueryRow(f.ctx, `INSERT INTO seasons (title_id, number) VALUES ($1::uuid, $2)
		        ON CONFLICT (title_id, number) DO UPDATE SET number = EXCLUDED.number RETURNING id::text`, title, 1).Scan(&season)
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO episodes (season_id, title_id, number, name) VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
			season, title, n, fmt.Sprintf("Ep %d", n)).Scan(&ep); err != nil {
			t.Fatal(err)
		}
		return ep
	}
	e1, e2, foreign := mkEp(series, 1), mkEp(series, 2), mkEp(other, 1)

	save := func(title string, ep *string) (Progress, error) {
		return f.svc.SaveProgress(f.ctx, v, ProgressUpdate{TitleID: title, EpisodeID: ep, PositionSeconds: 10, DurationSeconds: 100})
	}
	if _, err := save(series, nil); !errors.Is(err, ErrInvalidProgress) {
		t.Fatalf("a series needs an episode: %v", err)
	}
	if _, err := save(movie, &e1); !errors.Is(err, ErrInvalidProgress) {
		t.Fatalf("a movie rejects an episode: %v", err)
	}
	if _, err := save(series, &foreign); !errors.Is(err, ErrInvalidProgress) {
		t.Fatalf("an episode of another title: %v", err)
	}
	missing := "00000000-0000-0000-0000-000000000000"
	if _, err := save(series, &missing); !errors.Is(err, ErrInvalidProgress) {
		t.Fatalf("unknown episode: %v", err)
	}
	if _, err := save(missing, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown title: %v", err)
	}
	p1, err := save(series, &e1)
	if err != nil || *p1.EpisodeID != e1 {
		t.Fatalf("episode 1: %+v %v", p1, err)
	}
	if _, err := save(series, &e2); err != nil {
		t.Fatal(err)
	}
	// repeating an episode update reuses its row; each episode has its own row
	if _, err := save(series, &e1); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM watch_progress WHERE title_id = $1::uuid`, series).Scan(&n)
	if n != 2 {
		t.Fatalf("one row per episode expected, got %d", n)
	}
}

func TestContinueWatching(t *testing.T) {
	f := newFx(t)
	v := f.as(f.viewerA, auth.RoleViewer)
	now := time.Now()
	movieA := f.title("movie-a", nil)
	movieDone := f.title("movie-done", nil)
	movieB := f.title("movie-b", nil)
	series := f.title("series", func(s *tspec) { s.typ = "series" })
	var season, ep1, ep2 string
	_ = f.pool.QueryRow(f.ctx, `INSERT INTO seasons (title_id, number) VALUES ($1::uuid, 2) RETURNING id::text`, series).Scan(&season)
	_ = f.pool.QueryRow(f.ctx, `INSERT INTO episodes (season_id, title_id, number, name) VALUES ($1::uuid, $2::uuid, 1, 'Pilot') RETURNING id::text`, season, series).Scan(&ep1)
	_ = f.pool.QueryRow(f.ctx, `INSERT INTO episodes (season_id, title_id, number, name) VALUES ($1::uuid, $2::uuid, 4, 'Four') RETURNING id::text`, season, series).Scan(&ep2)

	f.progress(f.viewerA, movieA, nil, 500, 1000, false, now.Add(-3*time.Hour))
	f.progress(f.viewerA, movieDone, nil, 990, 1000, true, now.Add(-1*time.Hour)) // finished: excluded
	f.progress(f.viewerA, movieB, nil, 100, 400, false, now.Add(-5*time.Hour))
	f.progress(f.viewerA, series, &ep1, 100, 2000, false, now.Add(-4*time.Hour))
	f.progress(f.viewerA, series, &ep2, 600, 2000, false, now.Add(-2*time.Hour)) // latest episode wins
	f.progress(f.viewerB, movieB, nil, 1, 100, false, now)                       // someone else's

	list, err := f.svc.ContinueWatching(f.ctx, v, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range list.Items {
		got = append(got, it.Title.Slug)
	}
	if !equal(got, []string{"series", "movie-a", "movie-b"}) {
		t.Fatalf("order must be most recently watched first, one entry per title, finished excluded: %v", got)
	}
	s := list.Items[0]
	if s.Episode == nil || s.Episode.ID != ep2 || s.Episode.SeasonNumber != 2 || s.Episode.Number != 4 || s.Episode.Name != "Four" {
		t.Fatalf("the series entry must point at the episode touched last: %+v", s.Episode)
	}
	if s.ProgressPercent != 30 || s.PositionSeconds != 600 || s.DurationSeconds != 2000 {
		t.Fatalf("progress fields: %+v", s)
	}
	if m := list.Items[1]; m.Episode != nil || m.ProgressPercent != 50 {
		t.Fatalf("movie entry: %+v", m)
	}
	if list.Items[0].Title.Genres == nil {
		t.Fatal("entries carry full cards")
	}
	if l2, _ := f.svc.ContinueWatching(f.ctx, v, 2); len(l2.Items) != 2 {
		t.Fatalf("limit: %d", len(l2.Items))
	}
	if other, _ := f.svc.ContinueWatching(f.ctx, f.as(f.viewerB, auth.RoleViewer), 10); len(other.Items) != 1 || other.Items[0].Title.Slug != "movie-b" {
		t.Fatalf("another user's list: %+v", other.Items)
	}
	if empty, _ := f.svc.ContinueWatching(f.ctx, f.as(f.user("viewer"), auth.RoleViewer), 10); empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty list must be [] not null: %+v", empty.Items)
	}
}
