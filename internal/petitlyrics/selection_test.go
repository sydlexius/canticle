package petitlyrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// liBlock builds one <li> block for a multi-candidate search HTML fixture.
func liBlock(id, album string, synced bool) string {
	var sb strings.Builder
	sb.WriteString(`<li class="lyrics-list-item">`)
	sb.WriteString(`<a href="/lyrics/` + id + `">Song Title</a>`)
	if album != "" {
		sb.WriteString(`<p class="lyrics-list-album">` + album + `</p>`)
	}
	if synced {
		sb.WriteString(`<span class="text_sync"></span>`)
	}
	sb.WriteString(`</li>`)
	return sb.String()
}

// searchHTMLMulti wraps a set of liBlock strings in a minimal search result page.
func searchHTMLMulti(blocks ...string) string {
	return `<html><body><ul class="lyrics-list">` +
		strings.Join(blocks, "\n") +
		`</ul></body></html>`
}

func TestParseSearchCandidates(t *testing.T) {
	html := searchHTMLMulti(
		liBlock("100", "Album One", true),
		liBlock("200", "Album Two", false),
		liBlock("300", "", false),
		// spurious <li> with no lyrics link must be skipped
		`<li class="nav-item"><a href="/about">About</a></li>`,
	)
	got := parseSearchCandidates([]byte(html))
	if len(got) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(got))
	}
	if got[0].id != "100" || got[0].album != "Album One" || !got[0].synced {
		t.Errorf("candidate 0 = %+v; want {id:100 album:Album One synced:true}", got[0])
	}
	if got[1].id != "200" || got[1].album != "Album Two" || got[1].synced {
		t.Errorf("candidate 1 = %+v; want {id:200 album:Album Two synced:false}", got[1])
	}
	if got[2].id != "300" || got[2].album != "" || got[2].synced {
		t.Errorf("candidate 2 = %+v; want {id:300 album: synced:false}", got[2])
	}
}

func TestParseSearchCandidates_Empty(t *testing.T) {
	if got := parseSearchCandidates([]byte(`<html><body>no results</body></html>`)); len(got) != 0 {
		t.Fatalf("want 0 candidates for no-results page, got %d", len(got))
	}
}

// TestParseSearchCandidates_MultiClassAlbum verifies that the album class token
// is matched even when additional classes appear on the same element.
func TestParseSearchCandidates_MultiClassAlbum(t *testing.T) {
	html := `<ul><li class="lyrics-list-item">` +
		`<a href="/lyrics/42">Song</a>` +
		`<p class="lyrics-list-album active">Multi Class Album</p>` +
		`</li></ul>`
	got := parseSearchCandidates([]byte(html))
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	if got[0].album != "Multi Class Album" {
		t.Errorf("album = %q; want %q", got[0].album, "Multi Class Album")
	}
}

func TestNormalizeAlbum(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Greatest Hits", "greatest hits"},
		{"  Greatest  Hits  ", "greatest hits"},
		{"Greatest Hits (Deluxe Edition)", "greatest hits (deluxe edition)"},
	}
	for _, tc := range cases {
		if got := normalizeAlbum(tc.in); got != tc.want {
			t.Errorf("normalizeAlbum(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestAlbumMatches(t *testing.T) {
	cases := []struct {
		candidate, query string
		want             bool
	}{
		// exact match
		{"Greatest Hits", "Greatest Hits", true},
		// edition suffix on candidate, base in query
		{"Greatest Hits (Deluxe Edition)", "Greatest Hits", true},
		// edition suffix on query, base on candidate
		{"Greatest Hits", "Greatest Hits (Deluxe Edition)", true},
		// remaster variant
		{"Greatest Hits (Remastered 2011)", "Greatest Hits", true},
		// "Hits" must NOT match "Greatest Hits" (no prefix in either direction)
		{"Greatest Hits", "Hits", false},
		{"Hits", "Greatest Hits", false},
		// empty candidate album must not match any non-empty query
		{"", "Greatest Hits", false},
		// empty query must not match any candidate
		{"Greatest Hits", "", false},
		// completely unrelated
		{"Album One", "Album Two", false},
	}
	for _, tc := range cases {
		got := albumMatches(tc.candidate, tc.query)
		if got != tc.want {
			t.Errorf("albumMatches(%q, %q) = %v; want %v", tc.candidate, tc.query, got, tc.want)
		}
	}
}

func TestSelectCandidate(t *testing.T) {
	cases := []struct {
		name       string
		candidates []searchCandidate
		album      string
		wantID     string
		wantErr    error
	}{
		{
			name:    "zero candidates",
			wantErr: ErrNotFound,
		},
		{
			name:       "single candidate, no album arg",
			candidates: []searchCandidate{{id: "1", synced: false}},
			wantID:     "1",
		},
		{
			name: "multi, no album arg, no synced - return first",
			candidates: []searchCandidate{
				{id: "1", synced: false},
				{id: "2", synced: false},
			},
			wantID: "1",
		},
		{
			name: "multi, no album arg, synced available - prefer synced",
			candidates: []searchCandidate{
				{id: "1", synced: false},
				{id: "2", synced: true},
			},
			wantID: "2",
		},
		{
			name: "album match with no synced in matched set - return album match",
			candidates: []searchCandidate{
				{id: "1", album: "A", synced: false},
				{id: "2", album: "B", synced: true},
			},
			album:  "A",
			wantID: "1",
		},
		{
			name: "album match with synced in matched set - prefer synced album match",
			candidates: []searchCandidate{
				{id: "1", album: "A", synced: false},
				{id: "2", album: "A", synced: true},
				{id: "3", album: "B", synced: true},
			},
			album:  "A",
			wantID: "2",
		},
		{
			name: "no album match - fall back to all, prefer synced",
			candidates: []searchCandidate{
				{id: "1", album: "A", synced: false},
				{id: "2", album: "B", synced: true},
			},
			album:  "Z",
			wantID: "2",
		},
		{
			name: "no album match - fall back to all, no synced - return first",
			candidates: []searchCandidate{
				{id: "1", album: "A", synced: false},
				{id: "2", album: "B", synced: false},
			},
			album:  "Z",
			wantID: "1",
		},
		{
			name: "edition suffix in candidate album - matches base query",
			candidates: []searchCandidate{
				{id: "1", album: "Greatest Hits (Deluxe Edition)", synced: false},
				{id: "2", album: "Other Album", synced: true},
			},
			album:  "Greatest Hits",
			wantID: "1",
		},
		{
			name: "substring non-match: Hits should not match Greatest Hits",
			candidates: []searchCandidate{
				{id: "1", album: "Greatest Hits", synced: false},
				{id: "2", album: "Hits", synced: true},
			},
			album: "Hits",
			// only id=2 matches "Hits" exactly; id=1 "Greatest Hits" does not
			wantID: "2",
		},
		{
			name: "empty album arg - ignore album, prefer synced",
			candidates: []searchCandidate{
				{id: "1", album: "A", synced: false},
				{id: "2", album: "B", synced: true},
			},
			album:  "",
			wantID: "2",
		},
		{
			// Regression: a blank-album candidate must NOT enter the matched set
			// when a non-empty query album is given. HasPrefix(nq, "") is always
			// true without the empty-string guard, so id:1 would wrongly win.
			name: "blank-album candidate must not match non-empty query (empty-string guard)",
			candidates: []searchCandidate{
				{id: "1", album: "", synced: false},
				{id: "2", album: "Target Album", synced: false},
			},
			album:  "Target Album",
			wantID: "2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := selectCandidate(tc.candidates, tc.album)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v; want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != tc.wantID {
				t.Fatalf("id = %q; want %q", id, tc.wantID)
			}
		})
	}
}

// TestFindLyrics_MultiCandidate_AlbumSelection verifies end-to-end that
// FindLyrics picks the album-matched candidate and uses its id for the AJAX
// lyrics fetch.
func TestFindLyrics_MultiCandidate_AlbumSelection(t *testing.T) {
	lrc := "[00:01.00]picked\n"
	// Two candidates: id=100 (Album A, not synced) and id=200 (Album B, synced).
	// With album="Album B" the selector should pick id=200 (album match + synced).
	f := &fixtureServer{
		searchBody: searchHTMLMulti(
			liBlock("100", "Album A", false),
			liBlock("200", "Album B", true),
		),
		jsBody:   validJS,
		ajaxBody: ajaxJSON(b64(lrc), 2),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	song, err := c.FindLyrics(context.Background(), models.Track{
		TrackName:  "x",
		ArtistName: "y",
		AlbumName:  "Album B",
	})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 1 || song.Subtitles.Lines[0].Text != "picked" {
		t.Fatalf("song = %+v; want one synced line 'picked'", song.Subtitles)
	}
}

// TestFindLyrics_MultiCandidate_NoAlbumMatch_FallsBack verifies that when no
// candidate matches the requested album, FindLyrics does not return an error
// and instead falls back to the global synced-preference logic.
func TestFindLyrics_MultiCandidate_NoAlbumMatch_FallsBack(t *testing.T) {
	lrc := "[00:01.00]fallback\n"
	f := &fixtureServer{
		searchBody: searchHTMLMulti(
			liBlock("100", "Album A", false),
			liBlock("200", "Album B", true),
		),
		jsBody:   validJS,
		ajaxBody: ajaxJSON(b64(lrc), 2),
	}
	srv := newFixtureServer(t, f)
	c := newTestClient(srv)

	// AlbumName="Z" matches nothing; should fall back and pick id=200 (synced).
	song, err := c.FindLyrics(context.Background(), models.Track{
		TrackName:  "x",
		ArtistName: "y",
		AlbumName:  "Z",
	})
	if err != nil {
		t.Fatalf("FindLyrics with no album match should not error; got: %v", err)
	}
	if len(song.Subtitles.Lines) == 0 {
		t.Fatal("expected lyrics from fallback candidate, got none")
	}
}
