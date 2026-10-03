package web

import (
	"errors"
	"net/url"
	"strconv"
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/reports"
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
	// Tier, Edited and MisSynced are the #1235 chips. A chip the bucket does not
	// offer (reports.BucketChips) is dropped at parse, so it never reaches the
	// filter or a link. Tier is "line" when set. Tier and MisSynced are mutually
	// exclusive (see parseQueueViewState).
	Tier      string
	Edited    bool
	MisSynced bool
	// Library is the Library filter's id (0 = all libraries). Parsing keeps any
	// positive id; the handler drops one that names no library.
	Library int64
}

// parseQueueViewState validates the page's query string. A repeated parameter
// is ambiguous and rejected, as the player's `from` is. A sort key is kept only
// if the bucket's own spec sorts on it: a key that is merely in the shared
// vocabulary (status) would otherwise resolve to the default order in SQL while
// the state, and every link built from it, kept naming an unsupported sort.
func parseQueueViewState(v url.Values, bucket reports.Bucket) (queueViewState, error) {
	var s queueViewState
	spec := reports.BucketSpec(bucket)
	keys := []string{"q", "after", "sort", "dir", "library"}
	// A repeated chip param is ambiguous only where the bucket offers that chip;
	// elsewhere it is ignored like any other chip param.
	for _, c := range reports.BucketChips(bucket) {
		keys = append(keys, string(c))
	}
	for _, k := range keys {
		if len(v[k]) > 1 {
			return s, errors.New("repeated parameter " + k)
		}
	}
	if raw := v.Get("after"); len(raw) <= tablesort.MaxCursorBytes {
		s.After = raw
	}
	if sort := v.Get("sort"); tablesort.KnownKey(sort) {
		if _, ok := spec.Columns[sort]; ok {
			s.Sort = sort
		}
	}
	if dir := v.Get("dir"); tablesort.ValidDir(dir) {
		s.Dir = dir
	}
	// Chips: an unknown value is ignored, never an error, and a chip the bucket
	// does not offer is dropped so no link ever carries a filter that cannot
	// apply (or one that is always empty or a no-op there).
	if reports.HasChip(bucket, reports.ChipLineSynced) {
		if t := v.Get("tier"); reports.ValidTier(t) {
			s.Tier = t
		}
	}
	if reports.HasChip(bucket, reports.ChipEdited) {
		s.Edited = v.Get("edited") == "1"
	}
	if reports.HasChip(bucket, reports.ChipMissynced) {
		s.MisSynced = v.Get("missync") == "1"
	}
	// The line tier predicate excludes mis_synced rows, so the two chips can
	// never both match. A URL carrying both resolves to Mis-synced, the narrower
	// and more deliberate filter, and the line tier is dropped.
	if s.MisSynced {
		s.Tier = ""
	}
	// Library: a positive id; anything else (empty, text, zero, overflow) is ignored.
	if n, err := strconv.ParseInt(v.Get("library"), 10, 64); err == nil && n > 0 {
		s.Library = n
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
	v := s.filterValues()
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

// filterValues is the part of the state that selects WHICH rows are listed
// (search and chips), as the sort header links and the search form carry it.
func (s queueViewState) filterValues() url.Values {
	v := url.Values{}
	if s.Query != "" {
		v.Set("q", s.Query)
	}
	if s.Tier != "" {
		v.Set("tier", s.Tier)
	}
	if s.Edited {
		v.Set("edited", "1")
	}
	if s.MisSynced {
		v.Set("missync", "1")
	}
	if s.Library > 0 {
		v.Set("library", strconv.FormatInt(s.Library, 10))
	}
	return v
}

// chipsActive reports whether any chip narrows the list.
func (s queueViewState) chipsActive() bool {
	return s.Tier != "" || s.Edited || s.MisSynced || s.Library > 0
}

// filter is the repo filter for this state.
func (s queueViewState) filter() reports.BucketFilter {
	return reports.BucketFilter{Query: s.Query, Tier: s.Tier, Edited: s.Edited, MisSynced: s.MisSynced, LibraryID: s.Library}
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
