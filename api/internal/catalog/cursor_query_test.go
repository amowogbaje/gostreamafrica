package catalog

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

const someID = "123e4567-e89b-12d3-a456-426614174000"

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 3, 1, 12, 30, 45, 123456000, time.UTC)
	cases := []struct {
		name string
		c    cursor
		kind string
		sort Sort
	}{
		{"newest", cursor{Kind: "titles", Sort: SortNewest, TS: &ts, ID: someID}, "titles", SortNewest},
		{"newest null ts", cursor{Kind: "titles", Sort: SortNewest, ID: someID}, "titles", SortNewest},
		{"name", cursor{Kind: "titles", Sort: SortName, Name: "Ọmọ Aládé", ID: someID}, "titles", SortName},
		{"relevance", cursor{Kind: "titles", Sort: SortRelevance, Rank: 0.0607927, ID: someID}, "titles", SortRelevance},
		{"watchlist", cursor{Kind: "watchlist", TS: &ts, ID: someID}, "watchlist", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseCursor(c.c.encode(), c.kind, c.sort)
			if err != nil || got == nil {
				t.Fatalf("err=%v got=%v", err, got)
			}
			if got.ID != c.c.ID || got.Name != c.c.Name || got.Rank != c.c.Rank || (got.TS == nil) != (c.c.TS == nil) {
				t.Fatalf("round trip changed the cursor: %+v vs %+v", got, c.c)
			}
			if got.TS != nil && !got.TS.Equal(*c.c.TS) {
				t.Fatalf("timestamp precision lost: %v vs %v", got.TS, c.c.TS)
			}
		})
	}
}

func TestParseCursorRejects(t *testing.T) {
	ts := time.Now().UTC()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	good := cursor{Kind: "titles", Sort: SortNewest, ID: someID}.encode()
	cases := map[string]struct {
		raw  string
		kind string
		sort Sort
	}{
		"not base64":             {"!!!not-base64!!!", "titles", SortNewest},
		"not json":               {enc("hello"), "titles", SortNewest},
		"unknown field":          {enc(`{"k":"titles","s":"newest","i":"` + someID + `","x":1}`), "titles", SortNewest},
		"trailing data":          {enc(`{"k":"titles","s":"newest","i":"` + someID + `"}{}`), "titles", SortNewest},
		"bad id":                 {enc(`{"k":"titles","s":"newest","i":"not-a-uuid"}`), "titles", SortNewest},
		"sql in id":              {enc(`{"k":"titles","s":"newest","i":"'; DROP TABLE titles;--"}`), "titles", SortNewest},
		"wrong kind":             {good, "watchlist", ""},
		"cursor from other sort": {good, "titles", SortName},
		"name cursor no name":    {cursor{Kind: "titles", Sort: SortName, ID: someID}.encode(), "titles", SortName},
		"watchlist no time":      {cursor{Kind: "watchlist", ID: someID}.encode(), "watchlist", ""},
		"too long":               {strings.Repeat("A", maxCursorLen+1), "titles", SortNewest},
	}
	for name, c := range cases {
		if got, err := parseCursor(c.raw, c.kind, c.sort); !errors.Is(err, ErrInvalidCursor) || got != nil {
			t.Errorf("%s: got=%v err=%v", name, got, err)
		}
	}
	_ = ts
	if c, err := parseCursor("", "titles", SortNewest); c != nil || err != nil {
		t.Errorf("empty cursor must mean first page: %v %v", c, err)
	}
}

func TestParseListQuery(t *testing.T) {
	q := func(kv ...string) url.Values {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	t.Run("defaults", func(t *testing.T) {
		p, f, err := parseListQuery(q())
		if err != nil || len(f) != 0 || p.Limit != 20 || p.Sort != SortNewest || p.After != nil {
			t.Fatalf("p=%+v f=%v err=%v", p, f, err)
		}
	})
	t.Run("search defaults to relevance", func(t *testing.T) {
		p, _, _ := parseListQuery(q("q", "  lagos  "))
		if p.Q != "lagos" || p.Sort != SortRelevance {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("blank q is ignored", func(t *testing.T) {
		p, f, _ := parseListQuery(q("q", "   "))
		if p.Q != "" || len(f) != 0 || p.Sort != SortNewest {
			t.Fatalf("%+v %v", p, f)
		}
	})
	t.Run("everything valid", func(t *testing.T) {
		p, f, err := parseListQuery(q("type", "series", "genre", "epic-historical", "language", "pcm", "sort", "name", "limit", "50"))
		if err != nil || len(f) != 0 || p.Type != "series" || p.Genre != "epic-historical" || p.Language != "pcm" || p.Sort != SortName || p.Limit != 50 {
			t.Fatalf("%+v %v %v", p, f, err)
		}
	})
	bad := []struct {
		name, field string
		kv          []string
	}{
		{"limit zero", "limit", []string{"limit", "0"}},
		{"limit 51", "limit", []string{"limit", "51"}},
		{"limit negative", "limit", []string{"limit", "-1"}},
		{"limit text", "limit", []string{"limit", "many"}},
		{"limit decimal", "limit", []string{"limit", "2.5"}},
		{"bad type", "type", []string{"type", "documentary"}},
		{"uppercase genre", "genre", []string{"genre", "Drama"}},
		{"genre with injection", "genre", []string{"genre", "x' OR '1'='1"}},
		{"bad language", "language", []string{"language", "English"}},
		{"q too long", "q", []string{"q", strings.Repeat("a", 101)}},
		{"q control char", "q", []string{"q", "a\x00b"}},
		{"unknown sort", "sort", []string{"sort", "popularity"}},
		{"relevance without q", "sort", []string{"sort", "relevance"}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, f, _ := parseListQuery(q(c.kv...))
			if f[c.field] == "" {
				t.Fatalf("%s not flagged: %v", c.field, f)
			}
		})
	}
	t.Run("several errors at once", func(t *testing.T) {
		_, f, _ := parseListQuery(q("limit", "0", "type", "x", "language", "!!"))
		if len(f) != 3 {
			t.Fatalf("want 3 field errors, got %v", f)
		}
	})
	t.Run("cursor must match the sort", func(t *testing.T) {
		cur := cursor{Kind: "titles", Sort: SortNewest, ID: someID}.encode()
		if _, _, err := parseListQuery(q("sort", "name", "cursor", cur)); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("err=%v", err)
		}
		if p, _, err := parseListQuery(q("cursor", cur)); err != nil || p.After == nil || p.After.ID != someID {
			t.Fatalf("p=%+v err=%v", p, err)
		}
	})
}

func TestETagMatching(t *testing.T) {
	const tag = `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false}, {tag, true}, {`W/` + tag, true}, {"*", true}, {`"other"`, false},
		{`"other", ` + tag, true}, {`"other",W/` + tag + ` , "x"`, true}, {`abc123`, false},
	}
	for _, c := range cases {
		if got := etagMatches(c.header, tag); got != c.want {
			t.Errorf("%q: got %v want %v", c.header, got, c.want)
		}
	}
	first, again, other := etagFor([]byte("a")), etagFor([]byte("a")), etagFor([]byte("b"))
	if first == other || first != again {
		t.Error("etag must depend on the body and be stable")
	}
}
