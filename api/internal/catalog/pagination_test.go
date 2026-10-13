package catalog

import (
	"fmt"
	"testing"

	"streamafrica/api/internal/auth"
)

func (f *fx) walk(v Viewer, p ListParams) (all []string, pages int) {
	f.t.Helper()
	if p.Sort == "" {
		p.Sort = SortNewest
	}
	for {
		page, err := f.svc.List(f.ctx, v, p)
		if err != nil {
			f.t.Fatal(err)
		}
		pages++
		all = append(all, slugs(page.Items)...)
		if page.NextCursor == nil {
			return all, pages
		}
		c, err := parseCursor(*page.NextCursor, "titles", p.Sort)
		if err != nil {
			f.t.Fatal(err)
		}
		p.After = c
		if pages > 100 {
			f.t.Fatal("pagination does not terminate")
		}
	}
}

// seedMany: 23 published titles where groups of 4 share a published_at (to test tie-breaking),
// plus 3 drafts (published_at NULL) owned by creator A.
func seedMany(f *fx) {
	for i := 0; i < 23; i++ {
		i := i
		f.title(fmt.Sprintf("t%02d", i), func(s *tspec) {
			s.name = fmt.Sprintf("Lagos %s %02d", []string{"Nights", "Market", "Road"}[i%3], i)
			s.published = f.day(-(i / 4)) // ties within each group of four
			s.synopsis = []string{"", "lagos story", "a story about lagos and lagos"}[i%3]
		})
	}
	for i := 0; i < 3; i++ {
		i := i
		f.title(fmt.Sprintf("d%d", i), func(s *tspec) { s.name = fmt.Sprintf("Draft %d", i); s.status = "draft" })
	}
}

func TestPagination_WalkEqualsOnePage(t *testing.T) {
	f := newFx(t)
	seedMany(f)
	creator := f.as(f.creatorA, auth.RoleCreator)
	cases := []struct {
		name string
		v    Viewer
		p    ListParams
		want int
	}{
		{"newest anonymous", Viewer{}, ListParams{Sort: SortNewest}, 23},
		{"name anonymous", Viewer{}, ListParams{Sort: SortName}, 23},
		{"relevance anonymous", Viewer{}, ListParams{Sort: SortRelevance, Q: "lagos"}, 23},
		{"newest creator (drafts first, NULL published_at)", creator, ListParams{Sort: SortNewest}, 26},
		{"name creator", creator, ListParams{Sort: SortName}, 26},
	}
	for _, c := range cases {
		for _, limit := range []int{1, 2, 5, 7, 23, 50} {
			t.Run(fmt.Sprintf("%s/limit=%d", c.name, limit), func(t *testing.T) {
				whole := f.list(c.v, ListParams{Sort: c.p.Sort, Q: c.p.Q, Limit: 50})
				p := c.p
				p.Limit = limit
				got, pages := f.walk(c.v, p)
				if !equal(got, slugs(whole.Items)) {
					t.Fatalf("walking with limit %d gave a different sequence:\n%v\nvs\n%v", limit, got, slugs(whole.Items))
				}
				if len(got) != c.want {
					t.Fatalf("got %d titles, want %d", len(got), c.want)
				}
				seen := map[string]bool{}
				for _, s := range got {
					if seen[s] {
						t.Fatalf("%s returned twice", s)
					}
					seen[s] = true
				}
				if wantPages := (c.want + limit - 1) / limit; pages != wantPages {
					t.Fatalf("%d pages, want %d", pages, wantPages)
				}
			})
		}
	}
}

func TestPagination_Boundaries(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 5; i++ {
		f.title(fmt.Sprintf("b%d", i), func(s *tspec) { s.published = f.day(-i) })
	}
	// exactly limit items: one page, no cursor
	if p := f.list(Viewer{}, ListParams{Limit: 5}); len(p.Items) != 5 || p.NextCursor != nil {
		t.Fatalf("limit == total: %d items cursor=%v", len(p.Items), p.NextCursor)
	}
	// one more than limit: cursor, then a final page of one
	p := f.list(Viewer{}, ListParams{Limit: 4})
	if len(p.Items) != 4 || p.NextCursor == nil {
		t.Fatalf("limit < total: %d items cursor=%v", len(p.Items), p.NextCursor)
	}
	c, _ := parseCursor(*p.NextCursor, "titles", SortNewest)
	last := f.list(Viewer{}, ListParams{Limit: 4, After: c})
	if len(last.Items) != 1 || last.NextCursor != nil {
		t.Fatalf("final page: %d items cursor=%v", len(last.Items), last.NextCursor)
	}
	// a filter that matches nothing is an empty page, not an error
	empty := f.list(Viewer{}, ListParams{Genre: "horror"})
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("empty: %+v", empty)
	}
	// a cursor pointing past the end is an empty page
	after := f.list(Viewer{}, ListParams{Limit: 5, After: c})
	_ = after
	far := cursor{Kind: "titles", Sort: SortNewest, TS: f.day(-3650), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	if p := f.list(Viewer{}, ListParams{After: &far}); len(p.Items) != 0 || p.NextCursor != nil {
		t.Fatalf("cursor past the end: %+v", p)
	}
}

func TestPagination_StableWhenDataChangesBetweenPages(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 6; i++ {
		f.title(fmt.Sprintf("s%d", i), func(s *tspec) { s.published = f.day(-i) })
	}
	first := f.list(Viewer{}, ListParams{Limit: 3})
	c, _ := parseCursor(*first.NextCursor, "titles", SortNewest)

	// a brand-new title and a deletion happen while the client is paging
	f.title("brand-new", func(s *tspec) { s.published = f.day(10) })
	f.exec(`DELETE FROM titles WHERE slug = 's4'`)

	second := f.list(Viewer{}, ListParams{Limit: 3, After: c})
	got := slugs(second.Items)
	for _, s := range slugs(first.Items) {
		for _, g := range got {
			if s == g {
				t.Fatalf("%s repeated on the second page: %v", s, got)
			}
		}
	}
	if !equal(got, []string{"s3", "s5"}) || second.NextCursor != nil {
		t.Fatalf("second page = %v (cursor %v), want [s3 s5]", got, second.NextCursor)
	}
}

func TestPagination_DraftsFirstThenPublishedAcrossTheNullBoundary(t *testing.T) {
	f := newFx(t)
	f.title("pub1", func(s *tspec) { s.published = f.day(-1) })
	f.title("pub2", func(s *tspec) { s.published = f.day(-2) })
	f.title("dr1", func(s *tspec) { s.status = "draft" })
	f.title("dr2", func(s *tspec) { s.status = "draft" })
	creator := f.as(f.creatorA, auth.RoleCreator)
	got, pages := f.walk(creator, ListParams{Limit: 1})
	if pages != 4 || len(got) != 4 {
		t.Fatalf("pages=%d got=%v", pages, got)
	}
	if !sameSet(got[:2], []string{"dr1", "dr2"}) || !equal(got[2:], []string{"pub1", "pub2"}) {
		t.Fatalf("order = %v, want the two drafts first and then pub1, pub2", got)
	}
}

func TestPagination_NameSortUsesIDAsTieBreak(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 7; i++ {
		f.title(fmt.Sprintf("same%d", i), func(s *tspec) { s.name = "Identical Name" })
	}
	got, _ := f.walk(Viewer{}, ListParams{Sort: SortName, Limit: 2})
	if len(got) != 7 {
		t.Fatalf("titles with identical names were lost or repeated across pages: %v", got)
	}
}

func TestPagination_FiltersAreKeptAcrossPages(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 9; i++ {
		i := i
		f.title(fmt.Sprintf("m%d", i), func(s *tspec) {
			s.published = f.day(-i)
			s.genres = []string{"drama"}
			if i%3 == 0 {
				s.lang = "yo"
			}
			if i%2 == 0 {
				s.typ = "series"
			}
		})
	}
	got, pages := f.walk(Viewer{}, ListParams{Type: "series", Language: "yo", Genre: "drama", Limit: 1})
	if !equal(got, []string{"m0", "m6"}) || pages != 2 {
		t.Fatalf("got %v in %d pages", got, pages)
	}
}
