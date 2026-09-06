package innertube

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture loads a fixture file. It calls t.Fatalf, so it must be called
// from the test goroutine (854-R4F4).
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// --- search response parsing ---

// TestParseSearchCandidates_ExtractsVideoID pins the fields this parser is
// responsible for carrying out of a real captured search response.
func TestParseSearchCandidates_ExtractsVideoID(t *testing.T) {
	got, err := parseSearchCandidates(readFixture(t, "search.json"))
	if err != nil {
		t.Fatalf("parseSearchCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	cand := got[0]
	if cand.VideoID != "NrgmdOz227I" {
		t.Errorf("videoId = %q, want NrgmdOz227I", cand.VideoID)
	}
	if cand.Title != "Placeholder Song Title" {
		t.Errorf("title = %q", cand.Title)
	}
	if cand.Artist != "Placeholder Artist Name" {
		t.Errorf("artist = %q", cand.Artist)
	}
	if cand.DurationSeconds != 126 {
		t.Errorf("duration = %d, want 126 (2:06)", cand.DurationSeconds)
	}
}

// TestParseSearchCandidates_ArtistNotOverwrittenByAlbum guards 854-F2: a
// realistic subtitle carries both an artist run and an album run, each
// bearing a browseEndpoint, so a last-browse-run-wins loop would report the
// ALBUM as the artist. The shipped search.json has only one such run and
// cannot exercise it; search_artist_album.json supplies both.
func TestParseSearchCandidates_ArtistNotOverwrittenByAlbum(t *testing.T) {
	got, err := parseSearchCandidates(readFixture(t, "search_artist_album.json"))
	if err != nil {
		t.Fatalf("parseSearchCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	if got[0].Artist != "Placeholder Artist Name" {
		t.Errorf("Artist = %q, want the artist run, not the album run", got[0].Artist)
	}
	if got[0].DurationSeconds != 126 {
		t.Errorf("duration = %d, want 126 (2:06)", got[0].DurationSeconds)
	}
}

// TestParseSearchCandidates_NonsenseQueryStillYieldsACandidate pins the trap
// doc.go documents: search never signals "no match", so a nonsense query
// returns a confident, fully-timed, unrelated candidate that decodes exactly
// like a real hit. This parser must NOT try to detect that -- the
// correspondence guard is the downstream caller's job -- so the fixture must
// parse successfully rather than being rejected here.
func TestParseSearchCandidates_NonsenseQueryStillYieldsACandidate(t *testing.T) {
	got, err := parseSearchCandidates(readFixture(t, "search_nonsense.json"))
	if err != nil {
		t.Fatalf("parseSearchCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate even on a nonsense query (see doc.go), got %d", len(got))
	}
	if got[0].VideoID != "PLACEHOLDvi" {
		t.Errorf("videoId = %q, want PLACEHOLDvi", got[0].VideoID)
	}
}

// TestParseSearchCandidates_EmptyShelfYieldsNothing covers a response with
// no musicCardShelfRenderer at all -- a genuine empty shelf. The parser
// returns no candidates and NO error; classifying an empty result as a miss
// belongs to the caller, which is what lets a caller distinguish it from a
// parse failure.
func TestParseSearchCandidates_EmptyShelfYieldsNothing(t *testing.T) {
	got, err := parseSearchCandidates([]byte(`{"contents":{"tabbedSearchResultsRenderer":{"tabs":[]}}}`))
	if err != nil {
		t.Fatalf("an empty shelf is not a parse error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no candidates, got %d", len(got))
	}
}

// TestParseSearchCandidates_SkipsUnusableShelves pins candidateFromShelf's
// rejection rules: a shelf with no title runs, no navigationEndpoint, no
// watchEndpoint, or an empty videoId yields nothing rather than a candidate
// with an empty VideoID that a caller would then try to fetch.
func TestParseSearchCandidates_SkipsUnusableShelves(t *testing.T) {
	const envelope = `{"contents":{"tabbedSearchResultsRenderer":{"tabs":[{"tabRenderer":` +
		`{"content":{"sectionListRenderer":{"contents":[{"musicCardShelfRenderer":%s}]}}}}]}}}`

	for _, tc := range []struct {
		name  string
		shelf string
	}{
		{"no_title_runs", `{"title":{"runs":[]}}`},
		{"no_navigation_endpoint", `{"title":{"runs":[{"text":"T"}]}}`},
		{"no_watch_endpoint", `{"title":{"runs":[{"text":"T","navigationEndpoint":{"browseEndpoint":{"browseId":"X"}}}]}}`},
		{"empty_video_id", `{"title":{"runs":[{"text":"T","navigationEndpoint":{"watchEndpoint":{"videoId":""}}}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(envelope, "%s", tc.shelf, 1)
			got, err := parseSearchCandidates([]byte(body))
			if err != nil {
				t.Fatalf("parseSearchCandidates: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("want no candidate from an unusable shelf, got %+v", got)
			}
		})
	}
}

// TestParseSearchCandidates_UnusableBodyIsErrNotFound and its sibling below
// pin the boundary this package draws between a benign miss and a real
// failure: a hollow or non-JSON body is ErrNotFound, while a body that opens
// like JSON and then breaks stays an unclassified error rather than being
// quietly bucketed as benign.
func TestParseSearchCandidates_UnusableBodyIsErrNotFound(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"whitespace", "   \n\t"},
		{"html_error_page", "<html><body>captive portal</body></html>"},
		{"plain_text", "service unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSearchCandidates([]byte(tc.body)); !errors.Is(err, ErrNotFound) {
				t.Errorf("want ErrNotFound, got %v", err)
			}
		})
	}
}

func TestParseSearchCandidates_GenuineParseErrorStaysUnclassified(t *testing.T) {
	_, err := parseSearchCandidates([]byte(`{"contents":{"tabbedSearchResultsRenderer":`))
	if err == nil {
		t.Fatal("want an error for a truncated mid-document body")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a genuine mid-document parse error must not classify as ErrNotFound")
	}
}

// --- next response parsing ---

// nextTabsBody wraps a tabs array in the next-response envelope, so the
// matching tests below vary only the part under test.
func nextTabsBody(tabs string) string {
	return `{"contents":{"singleColumnMusicWatchNextResultsRenderer":{"tabbedRenderer":{` +
		`"watchNextTabbedResultsRenderer":{"tabs":[` + tabs + `]}}}}}`
}

// lyricsTabJSON renders one tabRenderer with the given display title and
// pageType (pageType omitted entirely when empty).
func lyricsTabJSON(title, pageType, browseID string) string {
	configs := ""
	if pageType != "" {
		configs = `,"browseEndpointContextSupportedConfigs":{` +
			`"browseEndpointContextMusicConfig":{"pageType":"` + pageType + `"}}`
	}
	return `{"tabRenderer":{"title":"` + title + `","endpoint":{"browseEndpoint":{` +
		`"browseId":"` + browseID + `"` + configs + `}}}}`
}

func TestParseLyricsBrowseID_ExtractsFromFixture(t *testing.T) {
	got, err := parseLyricsBrowseID(readFixture(t, "next.json"))
	if err != nil {
		t.Fatalf("parseLyricsBrowseID: %v", err)
	}
	if got != "MPLYt_Cn67yAcHym7-13" {
		t.Errorf("browseId = %q, want MPLYt_Cn67yAcHym7-13", got)
	}
	if !strings.HasPrefix(got, lyricsBrowseIDPrefix) {
		t.Errorf("lyrics browseId must carry the %s prefix, got %q", lyricsBrowseIDPrefix, got)
	}
}

// TestParseLyricsBrowseID_MatchesPageTypeNotTitle is 854-R4F1's required
// proof. The tab TITLE is a localized display string the gateway chooses
// from the caller's IP or account; matching on it made a non-English
// response return ErrNoLyricsTab for every track -- a silent total-lane
// outage indistinguishable from a catalog miss. The pageType is a stable
// machine token, so a tab titled in any language must still resolve.
func TestParseLyricsBrowseID_MatchesPageTypeNotTitle(t *testing.T) {
	for _, tc := range []struct{ name, title string }{
		{"english", "Lyrics"},
		{"spanish", "Letra"},
		{"german", "Liedtext"},
		{"japanese", "歌詞"},
		{"empty_title", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := nextTabsBody(`{"tabRenderer":{"title":"Up next"}},` +
				lyricsTabJSON(tc.title, lyricsPageType, "MPLYlocalized"))

			got, err := parseLyricsBrowseID([]byte(body))
			if err != nil {
				t.Fatalf("a %s tab title must still resolve: %v", tc.name, err)
			}
			if got != "MPLYlocalized" {
				t.Errorf("browseId = %q, want MPLYlocalized", got)
			}
		})
	}
}

// TestParseLyricsBrowseID_NonLyricsPageTypeWinsOverTitle guards the other
// direction: a stamped pageType that is not the lyrics one must win over a
// tab that merely happens to be titled "Lyrics", so the title fallback
// cannot resurrect a wrong tab.
func TestParseLyricsBrowseID_NonLyricsPageTypeWinsOverTitle(t *testing.T) {
	body := nextTabsBody(lyricsTabJSON("Lyrics", "MUSIC_PAGE_TYPE_ALBUM", "MPLYbutanalbum"))
	if _, err := parseLyricsBrowseID([]byte(body)); !errors.Is(err, ErrNoLyricsTab) {
		t.Fatalf("want ErrNoLyricsTab for a non-lyrics pageType, got %v", err)
	}
}

// TestParseLyricsBrowseID_FallsBackToEnglishTitle pins the fallback lane: a
// tab carrying no pageType at all is matched on the English title. That match
// only becomes locale-deterministic once the CALLS slice pins hl/gl (see the
// note on lyricsTabTitleEn in parse.go); on this branch the title is whatever
// locale the gateway picks.
func TestParseLyricsBrowseID_FallsBackToEnglishTitle(t *testing.T) {
	got, err := parseLyricsBrowseID([]byte(nextTabsBody(lyricsTabJSON("Lyrics", "", "MPLYnopagetype"))))
	if err != nil {
		t.Fatalf("parseLyricsBrowseID: %v", err)
	}
	if got != "MPLYnopagetype" {
		t.Errorf("browseId = %q, want MPLYnopagetype", got)
	}
}

// TestParseLyricsBrowseID_NoLyricsTab covers a response whose tabs never
// include a lyrics entry at all.
func TestParseLyricsBrowseID_NoLyricsTab(t *testing.T) {
	_, err := parseLyricsBrowseID([]byte(nextTabsBody(`{"tabRenderer":{"title":"Up next"}}`)))
	if !errors.Is(err, ErrNoLyricsTab) {
		t.Fatalf("want ErrNoLyricsTab, got %v", err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Error("ErrNoLyricsTab must wrap ErrNotFound")
	}
}

// TestParseLyricsBrowseID_NonMPLYBrowseIDIsErrNoLyricsTab guards 854-F7: a
// lyrics tab whose browseId does not carry the documented prefix must not be
// returned as a trusted lyrics browseId, since passing an unrecognized ID
// through would feed an unrelated browse target to a caller that assumes
// every browseId it receives is a lyrics tab.
func TestParseLyricsBrowseID_NonMPLYBrowseIDIsErrNoLyricsTab(t *testing.T) {
	body := nextTabsBody(lyricsTabJSON("Lyrics", lyricsPageType, "VLPLnotalyricsbrowseid"))
	if _, err := parseLyricsBrowseID([]byte(body)); !errors.Is(err, ErrNoLyricsTab) {
		t.Fatalf("want ErrNoLyricsTab for a non-MPLY browseId, got %v", err)
	}
}

// TestParseLyricsBrowseID_SkipsTabsWithoutABrowseID pins the endpoint
// guards: a lyrics tab with no endpoint, no browseEndpoint, or an empty
// browseId is skipped rather than returning an empty string as a browseId.
func TestParseLyricsBrowseID_SkipsTabsWithoutABrowseID(t *testing.T) {
	for _, tc := range []struct{ name, tab string }{
		{"no_endpoint", `{"tabRenderer":{"title":"Lyrics"}}`},
		{"no_browse_endpoint", `{"tabRenderer":{"title":"Lyrics","endpoint":{}}}`},
		{"empty_browse_id", `{"tabRenderer":{"title":"Lyrics","endpoint":{"browseEndpoint":{"browseId":""}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseLyricsBrowseID([]byte(nextTabsBody(tc.tab))); !errors.Is(err, ErrNoLyricsTab) {
				t.Errorf("want ErrNoLyricsTab, got %v", err)
			}
		})
	}
}

func TestParseLyricsBrowseID_UnusableBodyIsErrNotFound(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"whitespace", "   \n\t"},
		{"html_error_page", "<html><body>captive portal</body></html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseLyricsBrowseID([]byte(tc.body)); !errors.Is(err, ErrNotFound) {
				t.Errorf("want ErrNotFound, got %v", err)
			}
		})
	}
}

func TestParseLyricsBrowseID_GenuineParseErrorStaysUnclassified(t *testing.T) {
	_, err := parseLyricsBrowseID([]byte(`{"contents":{"singleColumnMusicWatchNextResultsRenderer":`))
	if err == nil {
		t.Fatal("want an error for a truncated mid-document body")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a genuine mid-document parse error must not classify as ErrNotFound")
	}
}

// TestParseSearchCandidates_WalksEveryTabAndShelf pins the two loop breadths
// the doc claims but no fixture exercised: every captured search response has
// exactly one tab and one shelf at the parsed depth, so truncating either
// loop to its first element left the suite green (854-R5F3). Both are real
// behavior -- the correspondence gate downstream needs EVERY candidate to
// choose from, so silently dropping one is a lane regression, not a
// cosmetic loss.
func TestParseSearchCandidates_WalksEveryTabAndShelf(t *testing.T) {
	shelf := func(videoID string) string {
		return `{"musicCardShelfRenderer":{"title":{"runs":[{"text":"T",` +
			`"navigationEndpoint":{"watchEndpoint":{"videoId":"` + videoID + `"}}}]}}}`
	}
	tab := func(shelves ...string) string {
		return `{"tabRenderer":{"content":{"sectionListRenderer":{"contents":[` +
			strings.Join(shelves, ",") + `]}}}}`
	}

	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "two shelves in one tab",
			body: `{"contents":{"tabbedSearchResultsRenderer":{"tabs":[` +
				tab(shelf("vidA"), shelf("vidB")) + `]}}}`,
			want: []string{"vidA", "vidB"},
		},
		{
			name: "one shelf in each of two tabs",
			body: `{"contents":{"tabbedSearchResultsRenderer":{"tabs":[` +
				tab(shelf("vidA")) + `,` + tab(shelf("vidB")) + `]}}}`,
			want: []string{"vidA", "vidB"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSearchCandidates([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseSearchCandidates: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("want %d candidates, got %d: %+v", len(tc.want), len(got), got)
			}
			for i, want := range tc.want {
				if got[i].VideoID != want {
					t.Errorf("candidate %d: want videoID %q, got %q", i, want, got[i].VideoID)
				}
			}
		})
	}
}

// TestParseSearchCandidates_TitleComesFromTheFirstRun pins which title run is
// read. Every fixture has exactly one, so reading the LAST instead survived
// mutation (854-R5F2). A multi-run title silently truncated to one run is fed
// to the correspondence gate, which then rejects a candidate that was
// actually correct -- a silent miss, not a visible error.
func TestParseSearchCandidates_TitleComesFromTheFirstRun(t *testing.T) {
	body := `{"contents":{"tabbedSearchResultsRenderer":{"tabs":[{"tabRenderer":{"content":` +
		`{"sectionListRenderer":{"contents":[{"musicCardShelfRenderer":{"title":{"runs":[` +
		`{"text":"First","navigationEndpoint":{"watchEndpoint":{"videoId":"wantedVid"}}},` +
		`{"text":"Second","navigationEndpoint":{"watchEndpoint":{"videoId":"otherVid"}}}` +
		`]}}}]}}}}]}}}`

	got, err := parseSearchCandidates([]byte(body))
	if err != nil {
		t.Fatalf("parseSearchCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly one candidate, got %d: %+v", len(got), got)
	}
	if got[0].VideoID != "wantedVid" {
		t.Errorf("want the FIRST run's videoID %q, got %q", "wantedVid", got[0].VideoID)
	}
	if got[0].Title != "First" {
		t.Errorf("want the FIRST run's text %q, got %q", "First", got[0].Title)
	}
}
