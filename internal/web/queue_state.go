package web

import (
	"errors"
	"net/url"
	"strconv"
	"unicode/utf8"
)

// maxQueueQueryRunes caps the search text. An overlong query is rejected (400),
// never truncated or passed through unbounded.
const maxQueueQueryRunes = 200

// queueViewState is the parsed, validated URL state of one /queue/{bucket}
// page (#1234). It is the single place the page's parameters are read and the
// single place they are written back into a URL (href), so every pager link
// and the search form carry the same state. Later slices (sort #1242, filters
// #1235) add fields here and to parse/values, and every link follows.
type queueViewState struct {
	// Query is the artist/title search text, as typed (the repo normalizes it).
	Query string
	// After is the keyset cursor: rows with id > After. Zero is the top.
	After int64
}

// parseQueueViewState validates the page's query string. A repeated parameter
// is ambiguous and rejected, as the player's `from` is.
func parseQueueViewState(v url.Values) (queueViewState, error) {
	var s queueViewState
	for _, k := range []string{"q", "after"} {
		if len(v[k]) > 1 {
			return s, errors.New("repeated parameter " + k)
		}
	}
	if raw := v.Get("after"); raw != "" {
		after, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			return s, errors.New("invalid after cursor")
		}
		s.After = after
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
	if s.After > 0 {
		v.Set("after", strconv.FormatInt(s.After, 10))
	}
	return v
}

// href is the URL of bucket's page for this state at the given cursor.
func (s queueViewState) href(bucket string, after int64) string {
	s.After = after
	out := "/queue/" + bucket
	if enc := s.values().Encode(); enc != "" {
		out += "?" + enc
	}
	return out
}
