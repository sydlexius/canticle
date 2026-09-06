package innertube

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- search response parsing ---

type searchResponse struct {
	Contents struct {
		TabbedSearchResultsRenderer struct {
			Tabs []struct {
				TabRenderer struct {
					Content struct {
						SectionListRenderer struct {
							Contents []searchSectionContent `json:"contents"`
						} `json:"sectionListRenderer"`
					} `json:"content"`
				} `json:"tabRenderer"`
			} `json:"tabs"`
		} `json:"tabbedSearchResultsRenderer"`
	} `json:"contents"`
}

type searchSectionContent struct {
	MusicCardShelfRenderer *musicCardShelfRenderer `json:"musicCardShelfRenderer"`

	// MusicShelfRenderer is the shape the LIVE gateway returns for a
	// songs-filtered search (#894). It carries the whole result LIST, one
	// musicResponsiveListItemRenderer per song, where a card shelf carries a
	// single promoted result. Both arms are kept: a response may contain
	// either, and a section carrying neither is skipped.
	MusicShelfRenderer *musicShelfRenderer `json:"musicShelfRenderer"`
}

// musicShelfRenderer is a list shelf: its contents are the search results.
type musicShelfRenderer struct {
	Contents []musicShelfItem `json:"contents"`
}

type musicShelfItem struct {
	MusicResponsiveListItemRenderer *musicResponsiveListItemRenderer `json:"musicResponsiveListItemRenderer"`
}

// musicResponsiveListItemRenderer is one row of a music shelf. The videoId is
// read from playlistItemData rather than from the title run's watchEndpoint:
// both carry it in every captured response, and playlistItemData is the
// simpler path with no navigation indirection.
type musicResponsiveListItemRenderer struct {
	FlexColumns      []flexColumn `json:"flexColumns"`
	PlaylistItemData struct {
		VideoID string `json:"videoId"`
	} `json:"playlistItemData"`
}

type flexColumn struct {
	Renderer struct {
		Text runsContainer `json:"text"`
	} `json:"musicResponsiveListItemFlexColumnRenderer"`
}

type musicCardShelfRenderer struct {
	Title    runsContainer `json:"title"`
	Subtitle runsContainer `json:"subtitle"`
}

type runsContainer struct {
	Runs []textRun `json:"runs"`
}

type textRun struct {
	Text               string              `json:"text"`
	NavigationEndpoint *navigationEndpoint `json:"navigationEndpoint"`
}

type navigationEndpoint struct {
	WatchEndpoint  *watchEndpoint  `json:"watchEndpoint"`
	BrowseEndpoint *browseEndpoint `json:"browseEndpoint"`
}

type watchEndpoint struct {
	VideoID string `json:"videoId"`
}

type browseEndpoint struct {
	BrowseID string `json:"browseId"`

	// Configs is populated on a next-response tab endpoint and carries the
	// pageType discriminator; a search-response browse endpoint omits it,
	// where encoding/json simply leaves it zero.
	Configs browseEndpointConfigs `json:"browseEndpointContextSupportedConfigs"`
}

// parseSearchCandidates walks every tab and section of a search response and
// extracts one SearchCandidate per musicCardShelfRenderer found.
func parseSearchCandidates(raw []byte) ([]SearchCandidate, error) {
	var resp searchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		if isUnusableBody(raw) {
			return nil, fmt.Errorf("innertube: search response unusable: %w", ErrNotFound)
		}
		return nil, fmt.Errorf("innertube: decode search response: %w", err)
	}

	var out []SearchCandidate
	for _, tab := range resp.Contents.TabbedSearchResultsRenderer.Tabs {
		for _, section := range tab.TabRenderer.Content.SectionListRenderer.Contents {
			if section.MusicCardShelfRenderer != nil {
				if cand, ok := candidateFromShelf(*section.MusicCardShelfRenderer); ok {
					out = append(out, cand)
				}
			}
			if section.MusicShelfRenderer != nil {
				for _, item := range section.MusicShelfRenderer.Contents {
					if item.MusicResponsiveListItemRenderer == nil {
						continue
					}
					if cand, ok := candidateFromListItem(*item.MusicResponsiveListItemRenderer); ok {
						out = append(out, cand)
					}
				}
			}
		}
	}
	return out, nil
}

// candidateFromShelf extracts a SearchCandidate from one musicCardShelfRenderer,
// or reports false if the shelf carries no usable videoId.
func candidateFromShelf(shelf musicCardShelfRenderer) (SearchCandidate, bool) {
	if len(shelf.Title.Runs) == 0 {
		return SearchCandidate{}, false
	}
	titleRun := shelf.Title.Runs[0]
	if titleRun.NavigationEndpoint == nil || titleRun.NavigationEndpoint.WatchEndpoint == nil {
		return SearchCandidate{}, false
	}
	videoID := titleRun.NavigationEndpoint.WatchEndpoint.VideoID
	if videoID == "" {
		return SearchCandidate{}, false
	}

	cand := SearchCandidate{VideoID: videoID, Title: titleRun.Text}
	artistSet := false
	for _, run := range shelf.Subtitle.Runs {
		if run.NavigationEndpoint != nil && run.NavigationEndpoint.BrowseEndpoint != nil {
			// A real subtitle reads "Song • Artist • Album • Duration", and
			// BOTH the artist run and the album run carry a browseEndpoint
			// (854-F2) -- the shipped fixture has only one such run, which
			// is why no prior test caught the last-wins overwrite. Take the
			// FIRST browse-bearing run rather than discriminating on the
			// browseId prefix ("UC..." for an artist channel, "MPRE..." for
			// an album): observed subtitle order always places the artist
			// run before the album run, and relying on run ORDER rather
			// than a prefix avoids trusting an undocumented, unversioned
			// string format as a second attack surface. If a future subtitle
			// shape ever puts a non-artist browse-bearing run first, this
			// degrades to that run's text in Artist -- still bounded by the
			// downstream correspondence guard (doc.go) that must
			// independently verify every candidate regardless.
			if !artistSet {
				cand.Artist = run.Text
				artistSet = true
			}
			continue
		}
		if d := parseDurationSeconds(run.Text); d > 0 {
			cand.DurationSeconds = d
		}
	}
	return cand, true
}

// candidateFromListItem extracts a SearchCandidate from one music-shelf row,
// or reports false if the row carries no usable videoId.
//
// The column layout is positional in every captured response: flexColumn 0 is
// the title (a single run bearing the watchEndpoint) and flexColumn 1 is the
// subtitle, reading "Artist - Album - Duration" as alternating browse-bearing
// and plain runs. Rather than trust that layout, this walks EVERY column's runs
// and classifies each run by what it carries, so a column reorder degrades to a
// miss on one field instead of reading the wrong field confidently.
//
// Artist takes the FIRST browse-bearing run, matching candidateFromShelf and
// for the same reason (854-F2): the album run carries a browseEndpoint too, so
// a last-wins loop would report the album as the artist. Both arms therefore
// share one rule, and the pageType discriminator stays unused -- see the
// reasoning in candidateFromShelf about not trusting an undocumented browseId
// format as a second attack surface.
//
// As with the card-shelf arm, nothing here establishes that the row
// CORRESPONDS to the requested track; search has no empty state (doc.go), so
// that remains SelectCandidate's job.
func candidateFromListItem(item musicResponsiveListItemRenderer) (SearchCandidate, bool) {
	videoID := item.PlaylistItemData.VideoID
	if videoID == "" {
		return SearchCandidate{}, false
	}

	cand := SearchCandidate{VideoID: videoID}
	artistSet := false
	for _, col := range item.FlexColumns {
		for _, run := range col.Renderer.Text.Runs {
			if run.NavigationEndpoint != nil && run.NavigationEndpoint.WatchEndpoint != nil {
				if cand.Title == "" {
					cand.Title = run.Text
				}
				continue
			}
			if run.NavigationEndpoint != nil && run.NavigationEndpoint.BrowseEndpoint != nil {
				if !artistSet {
					cand.Artist = run.Text
					artistSet = true
				}
				continue
			}
			if d := parseDurationSeconds(run.Text); d > 0 {
				cand.DurationSeconds = d
			}
		}
	}
	return cand, true
}

// --- next response parsing ---

// nextResponse models only the path from the top of a next response down to
// the tab list this client scans for a Lyrics tab.
type nextResponse struct {
	Contents struct {
		SingleColumnMusicWatchNextResultsRenderer struct {
			TabbedRenderer struct {
				WatchNextTabbedResultsRenderer struct {
					Tabs []struct {
						TabRenderer struct {
							Title    string       `json:"title"`
							Endpoint *tabEndpoint `json:"endpoint"`
						} `json:"tabRenderer"`
					} `json:"tabs"`
				} `json:"watchNextTabbedResultsRenderer"`
			} `json:"tabbedRenderer"`
		} `json:"singleColumnMusicWatchNextResultsRenderer"`
	} `json:"contents"`
}

type tabEndpoint struct {
	BrowseEndpoint *browseEndpoint `json:"browseEndpoint"`
}

// browseEndpointConfigs carries the machine-readable page type innertube
// stamps on a browse endpoint. This is the stable key the lyrics tab is
// identified by -- see lyricsPageType.
type browseEndpointConfigs struct {
	MusicConfig struct {
		PageType string `json:"pageType"`
	} `json:"browseEndpointContextMusicConfig"`
}

// lyricsBrowseIDPrefix is the browseId prefix doc.go documents for the
// Lyrics tab. Validated in parseLyricsBrowseID (854-F7): the prefix was
// previously only ASSERTED by a test against a well-formed fixture, not
// enforced by any code, which let the doc comment claim an invariant the
// parser did not actually hold.
const lyricsBrowseIDPrefix = "MPLY"

// lyricsPageType is the machine-readable page type innertube stamps on the
// lyrics tab's browse endpoint. Unlike the tab TITLE it is not localized and
// not user-facing, so it is the same token whatever language the gateway
// decides to render for us -- which is what makes it the right key to match
// on (854-R4F1).
const lyricsPageType = "MUSIC_PAGE_TYPE_TRACK_LYRICS"

// lyricsTabTitleEn is the English display title of the lyrics tab, used ONLY
// as a fallback for a response that carries no pageType at all.
//
// The fallback is deterministic only when the request pins a locale. That
// pinning lives in the CALLS slice (see requestHl / requestGl there), not in
// this one and not in the transport slice, so on this branch alone the title
// a server returns is whatever it picks from IP or account. That is a
// FORWARD dependency, deliberately recorded rather than assumed: the
// pageType match below is what makes tab selection correct here, and the
// title fallback only becomes locale-deterministic once the calls slice
// lands.
const lyricsTabTitleEn = "Lyrics"

// parseLyricsBrowseID scans a next response's tabs for the lyrics tab and
// returns its browseId, or ErrNoLyricsTab if no such tab exists, it carries
// no browseId, or the browseId does not carry the documented MPLY prefix.
//
// The tab is identified by its pageType (MUSIC_PAGE_TYPE_TRACK_LYRICS), a
// stable machine token, NOT by its display title (854-R4F1). Matching on the
// title alone was a silent total-lane outage waiting to happen: the title is
// a LOCALIZED string chosen by the gateway from the caller's IP or account,
// so a non-English response would match no tab at all and return
// ErrNoLyricsTab -- indistinguishable from a genuine catalog miss, for every
// track, forever. The title comparison survives only as a fallback for a tab
// that carries no pageType. The calls slice pins hl/gl on every request,
// which is what makes that fallback deterministic rather than
// locale-dependent -- see the note on lyricsTabTitleEn above.
//
// A non-MPLY browseId under a "Lyrics"-titled tab has never been observed,
// but if the API ever renders one, ErrNoLyricsTab (rather than returning the
// unrecognized ID) is the right sentinel: it wraps ErrNotFound, and a
// browseId this client does not recognize as a lyrics rendition is
// indistinguishable in consequence from there being no lyrics tab at all --
// both are benign misses, not failures. Silently passing an unvalidated ID
// through to Browse risks feeding an unrelated (non-lyrics) browse target to
// a caller that assumes every browseId it receives is a lyrics tab.
func parseLyricsBrowseID(raw []byte) (string, error) {
	var resp nextResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		if isUnusableBody(raw) {
			return "", fmt.Errorf("innertube: next response unusable: %w", ErrNotFound)
		}
		return "", fmt.Errorf("innertube: decode next response: %w", err)
	}

	for _, tab := range resp.Contents.SingleColumnMusicWatchNextResultsRenderer.TabbedRenderer.WatchNextTabbedResultsRenderer.Tabs {
		tr := tab.TabRenderer
		if tr.Endpoint == nil || tr.Endpoint.BrowseEndpoint == nil || tr.Endpoint.BrowseEndpoint.BrowseID == "" {
			continue
		}
		be := tr.Endpoint.BrowseEndpoint
		pageType := be.Configs.MusicConfig.PageType
		switch {
		case pageType == lyricsPageType:
			// Primary, locale-independent match.
		case pageType != "":
			// A stamped page type that is not the lyrics one is a
			// different tab; the title must not be allowed to override it.
			continue
		case tr.Title != lyricsTabTitleEn:
			// No page type at all: fall back to the pinned-English title.
			continue
		}
		browseID := be.BrowseID
		if !strings.HasPrefix(browseID, lyricsBrowseIDPrefix) {
			continue
		}
		return browseID, nil
	}
	return "", fmt.Errorf("innertube: no lyrics tab found: %w", ErrNoLyricsTab)
}
