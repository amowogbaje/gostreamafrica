package catalog

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	defaultLimit = 20
	maxLimit     = 50
	maxQueryLen  = 100
)

var (
	slugRE     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	languageRE = regexp.MustCompile(`^[a-z]{2,3}$`)
)

func parseLimit(q url.Values, fields map[string]string) int {
	raw := q.Get("limit")
	if raw == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		fields["limit"] = "must be a whole number from 1 to 50"
		return defaultLimit
	}
	return n
}

// parseListQuery validates the query string of GET /v1/titles. Field problems are returned for a
// 422; a cursor that does not belong to this request is ErrInvalidCursor (400).
func parseListQuery(q url.Values) (ListParams, map[string]string, error) {
	fields := map[string]string{}
	p := ListParams{Limit: parseLimit(q, fields)}

	if t := q.Get("type"); t != "" {
		if t != "movie" && t != "series" {
			fields["type"] = "must be movie or series"
		}
		p.Type = t
	}
	if g := q.Get("genre"); g != "" {
		if len(g) > 60 || !slugRE.MatchString(g) {
			fields["genre"] = "must be a genre slug such as drama"
		}
		p.Genre = g
	}
	if l := q.Get("language"); l != "" {
		if !languageRE.MatchString(l) {
			fields["language"] = "must be a language code such as en, yo, ig, ha or pcm"
		}
		p.Language = l
	}
	if s := strings.TrimSpace(q.Get("q")); s != "" {
		switch {
		case utf8.RuneCountInString(s) > maxQueryLen:
			fields["q"] = "must be at most 100 characters"
		case strings.IndexFunc(s, unicode.IsControl) >= 0:
			fields["q"] = "must not contain control characters"
		}
		p.Q = s
	}

	switch sort := Sort(q.Get("sort")); sort {
	case "":
		p.Sort = SortNewest
		if p.Q != "" {
			p.Sort = SortRelevance
		}
	case SortNewest, SortName:
		p.Sort = sort
	case SortRelevance:
		p.Sort = sort
		if p.Q == "" {
			fields["sort"] = "relevance needs a search query (q)"
		}
	default:
		p.Sort = SortNewest
		fields["sort"] = "must be newest, name or relevance"
	}
	if len(fields) > 0 {
		return p, fields, nil
	}
	c, err := parseCursor(q.Get("cursor"), "titles", p.Sort)
	if err != nil {
		return p, nil, err
	}
	p.After = c
	return p, nil, nil
}
