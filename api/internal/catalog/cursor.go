package catalog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// Sort is a title list ordering.
type Sort string

// Supported orderings. Relevance needs a search query.
const (
	SortNewest    Sort = "newest"
	SortName      Sort = "name"
	SortRelevance Sort = "relevance"
)

// ErrInvalidCursor means a pagination cursor is malformed or does not belong to this request.
var ErrInvalidCursor = errors.New("invalid cursor")

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const maxCursorLen = 600

// cursor is a keyset position: the sort key(s) of the last row returned plus its id, so the next
// page starts strictly after it. Cursors are opaque to clients. They carry no authority (every
// value is bound as a typed parameter), so they need no signature.
type cursor struct {
	Kind string     `json:"k"` // "titles" or "watchlist"
	Sort Sort       `json:"s,omitempty"`
	TS   *time.Time `json:"t,omitempty"` // published_at (newest) or added_at (watchlist); absent = NULL
	Name string     `json:"n,omitempty"`
	Rank float32    `json:"r,omitempty"`
	ID   string     `json:"i"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c) // cannot fail for this struct
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseCursor decodes raw and checks it belongs to the same kind of list and sort.
func parseCursor(raw, kind string, sort Sort) (*cursor, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxCursorLen {
		return nil, ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var c cursor
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return nil, ErrInvalidCursor
	}
	if c.Kind != kind || !uuidRE.MatchString(c.ID) {
		return nil, ErrInvalidCursor
	}
	if kind == "titles" && c.Sort != sort {
		return nil, ErrInvalidCursor
	}
	if kind == "watchlist" && c.TS == nil {
		return nil, ErrInvalidCursor
	}
	if kind == "titles" && c.Sort == SortName && c.Name == "" {
		return nil, ErrInvalidCursor
	}
	return &c, nil
}
