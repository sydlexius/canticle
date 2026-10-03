package web

import (
	"errors"
	"net/url"
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/tablesort"
)

// maxQueueQueryRunes caps the search text. An overlong query is rejected (400),
// never truncated or passed through unbounded.
const maxQueueQueryRunes = 200

// queueViewState is the parsed, validated URL state of one /queue/{bucket}
// page (#1234, #1242). It is the single place the page's parameters are read and
// the single place they are written back into a URL (href), so every pager
// link, header link and the search form carry the same state. Later slices
// (filters #1235) add fields here and to parse/values, and every link follows.
type queueViewState struct {
	// Query is the artist/title search text, as typed (the repo normalizes it).
	Query string
	// After is the raw keyset cursor (tablesort.Cursor.Encode form); "" is the
	// top. It is validated against the active sort by the handler, and a forged
	// one falls back to the first page.
	After string
	// Sort and Dir are the explicitly requested sort column and direction, kept
	// only when valid ("" means the bucket default). An invalid value is dropped
	// at parse, never carried or reflected.
	Sort string
	Dir  string
}

// parseQueueViewState validates the page's query string. A repeated parameter
// is ambiguous and rejected, as the player's `from` is.
func parseQueueViewState(v url.Values) (queueViewState, error) {
	var s queueViewState
	for _, k := range []string{"q", "after", "sort", "dir"} {
		if len(v[k]) > 1 {
			return s, errors.New("repeated parameter " + k)
		}
	}
	if raw := v.Get("after"); len(raw) <= tablesort.MaxCursorBytes {
		s.After = raw
	}
	if sort := v.Get("sort"); tablesort.KnownKey(sort) {
		s.Sort = sort
	}
	if dir := v.Get("dir"); tablesort.ValidDir(dir) {
		s.Dir = dir
	}
	q := v.Get("q")
	if utf8.RuneCountInString(q) > maxQueueQueryRunes {
		return s, errors.New("search text too long")
	}
	s.Query = q
	return s, nil
}

// values renders the state, omitting zero values so a default page has a clean URL.
func (s queueViewState) values() url.Values {
	v := url.Values{}
	if s.Query != "" {
		v.Set("q", s.Query)
	}
	if s.After != "" {
		v.Set("after", s.After)
	}
	if s.Sort != "" {
		v.Set("sort", s.Sort)
	}
	if s.Dir != "" {
		v.Set("dir", s.Dir)
	}
	return v
}

// href is the URL of bucket's page for this state at the given cursor.
func (s queueViewState) href(bucket string, after string) string {
	s.After = after
	out := "/queue/" + bucket
	if enc := s.values().Encode(); enc != "" {
		out += "?" + enc
	}
	return out
}

// withoutQuery is the state with the search cleared, sort kept.
func (s queueViewState) withoutQuery() queueViewState {
	s.Query = ""
	return s
}

// backLinkState is the part of the state the preview player carries so its
// Back link returns to the same view: search, sort and direction, never the
// cursor (a stale position would hide rows).
func (s queueViewState) backLinkState() queueViewState {
	s.After = ""
	return s
}
