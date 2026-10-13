package catalog

import (
	"errors"
	"testing"

	"streamafrica/api/internal/auth"
)

// seedVisibility creates one title per state across two creators.
func seedVisibility(f *fx) {
	f.title("pub-a", func(s *tspec) { s.name = "Pub A"; s.synopsis = "findme" })
	f.title("draft-a", func(s *tspec) { s.name = "Draft A"; s.status = "draft"; s.synopsis = "findme" })
	f.title("unpub-a", func(s *tspec) { s.name = "Unpub A"; s.status = "unpublished"; s.published = f.day(-30) })
	f.title("pub-b", func(s *tspec) { s.name = "Pub B"; s.creator = f.creatorB; s.synopsis = "findme" })
	f.title("proc-b", func(s *tspec) {
		s.name = "Proc B"
		s.creator = f.creatorB
		s.status = "processing"
		s.synopsis = "findme"
	})
}

func TestVisibility_ListByRole(t *testing.T) {
	f := newFx(t)
	seedVisibility(f)
	published := []string{"pub-a", "pub-b"}
	cases := []struct {
		name string
		v    Viewer
		want []string
	}{
		{"anonymous", Viewer{}, published},
		{"viewer", f.as(f.viewerA, auth.RoleViewer), published},
		{"creator A sees own drafts and unpublished", f.as(f.creatorA, auth.RoleCreator), []string{"pub-a", "pub-b", "draft-a", "unpub-a"}},
		{"creator B sees only own extra", f.as(f.creatorB, auth.RoleCreator), []string{"pub-a", "pub-b", "proc-b"}},
		{"admin sees everything", f.as(f.admin, auth.RoleAdmin), []string{"pub-a", "pub-b", "draft-a", "unpub-a", "proc-b"}},
		// ownership only matters for creators: a viewer-role account never sees drafts
		{"viewer role that owns a draft", f.as(f.creatorA, auth.RoleViewer), published},
		// a forged role without a verified user id must not widen access
		{"admin role without a user id", Viewer{Role: auth.RoleAdmin}, published},
		{"creator role without a user id", Viewer{Role: auth.RoleCreator}, published},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := slugs(f.list(c.v, ListParams{}).Items); !sameSet(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			// search and filters obey the same rule (a draft must not leak through q or genre)
			if got := slugs(f.list(c.v, ListParams{Q: "findme", Sort: SortRelevance}).Items); !sameSet(got, intersect(c.want, []string{"pub-a", "pub-b", "draft-a", "proc-b"})) {
				t.Fatalf("search leaked or lost titles: %v", got)
			}
		})
	}
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
			}
		}
	}
	return out
}

func TestVisibility_DetailAndTitleType(t *testing.T) {
	f := newFx(t)
	seedVisibility(f)
	draftID := ""
	if err := f.pool.QueryRow(f.ctx, `SELECT id::text FROM titles WHERE slug = 'draft-a'`).Scan(&draftID); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		v    Viewer
		ok   bool
	}{
		{"anonymous", Viewer{}, false},
		{"viewer", f.as(f.viewerA, auth.RoleViewer), false},
		{"other creator", f.as(f.creatorB, auth.RoleCreator), false},
		{"owner", f.as(f.creatorA, auth.RoleCreator), true},
		{"admin", f.as(f.admin, auth.RoleAdmin), true},
		{"forged admin role", Viewer{Role: auth.RoleAdmin}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := f.svc.Detail(f.ctx, c.v, "draft-a")
			if c.ok && (err != nil || d.Status != "draft") {
				t.Fatalf("detail: %+v %v", d, err)
			}
			if !c.ok && !errors.Is(err, ErrNotFound) {
				t.Fatalf("a draft must look like it does not exist: %v", err)
			}
			_, err = f.store.TitleType(f.ctx, c.v, draftID)
			if c.ok != (err == nil) {
				t.Fatalf("TitleType ok=%v err=%v", c.ok, err)
			}
		})
	}
	if _, err := f.svc.Detail(f.ctx, Viewer{}, "no-such-title"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown slug: %v", err)
	}
	if d, err := f.svc.Detail(f.ctx, Viewer{}, "pub-a"); err != nil || d.Viewer != nil {
		t.Fatalf("anonymous detail must not carry viewer state: %+v %v", d.Viewer, err)
	}
}

func TestVisibility_StatusChangeHidesTitleEverywhere(t *testing.T) {
	f := newFx(t)
	id := f.title("flip", nil)
	viewer := f.as(f.viewerA, auth.RoleViewer)
	if _, _, err := f.svc.WatchlistAdd(f.ctx, viewer, id); err != nil {
		t.Fatal(err)
	}
	f.progress(f.viewerA, id, nil, 100, 1000, false, f.base)
	if len(f.list(viewer, ListParams{}).Items) != 1 {
		t.Fatal("setup: title should be visible")
	}

	f.exec(`UPDATE titles SET status = 'unpublished' WHERE id = $1::uuid`, id)

	if n := len(f.list(viewer, ListParams{}).Items); n != 0 {
		t.Fatalf("unpublished title still listed (%d)", n)
	}
	if p, _ := f.svc.Watchlist(f.ctx, viewer, nil, 10); len(p.Items) != 0 {
		t.Fatalf("unpublished title still on the watchlist: %+v", p.Items)
	}
	if c, _ := f.svc.ContinueWatching(f.ctx, viewer, 10); len(c.Items) != 0 {
		t.Fatalf("unpublished title still in continue watching: %+v", c.Items)
	}
	if _, err := f.svc.SaveProgress(f.ctx, viewer, ProgressUpdate{TitleID: id, PositionSeconds: 1, DurationSeconds: 10}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("progress on an unpublished title: %v", err)
	}
	var rows int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM watchlist`).Scan(&rows)
	if rows != 1 {
		t.Fatal("the watchlist row must survive (the title may be republished)")
	}
	f.exec(`UPDATE titles SET status = 'published' WHERE id = $1::uuid`, id)
	if p, _ := f.svc.Watchlist(f.ctx, viewer, nil, 10); len(p.Items) != 1 {
		t.Fatal("republished title did not come back to the watchlist")
	}
}
