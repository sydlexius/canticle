package innertube

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// The fixtures carry PLACEHOLDER artist/title strings, deliberately: the
// library's track metadata is private and this is a public repo. That is not a
// weakening of the test. The guard's input is a pair of strings and its verdict
// is a Jaro-Winkler comparison, so what has to be preserved from the measured
// #848 spike is the RELATIONSHIP between the requested values and the returned
// ones -- identical on a correct resolution, unrelated on a nonsense query --
// and placeholders preserve that exactly. The measured confidence numbers are
// asserted directly in TestMeasuredSpikeConfidencesStraddleTheFloor below,
// against non-lyric synthetic strings.

// loadSearchCandidates parses the top-hit candidate out of a search fixture.
//
// This is a TEST-LOCAL parser, not a preview of the real one. #851 owns the
// search response parser and lives in another file; duplicating a minimal
// reader here is what lets this slice assert against the REAL captured payload
// shape without editing a shared file or taking a dependency on a sibling
// slice that is being built in parallel. It reads only the fields
// SearchCandidate carries.
func loadSearchCandidates(t *testing.T, name string) []SearchCandidate {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	var doc struct {
		Contents struct {
			TabbedSearchResultsRenderer struct {
				Tabs []struct {
					TabRenderer struct {
						Content struct {
							SectionListRenderer struct {
								Contents []struct {
									MusicCardShelfRenderer *struct {
										Title struct {
											Runs []struct {
												Text               string `json:"text"`
												NavigationEndpoint struct {
													WatchEndpoint struct {
														VideoID string `json:"videoId"`
													} `json:"watchEndpoint"`
												} `json:"navigationEndpoint"`
											} `json:"runs"`
										} `json:"title"`
										Subtitle struct {
											Runs []struct {
												Text               string `json:"text"`
												NavigationEndpoint struct {
													BrowseEndpoint struct {
														BrowseEndpointContextSupportedConfigs struct {
															BrowseEndpointContextMusicConfig struct {
																PageType string `json:"pageType"`
															} `json:"browseEndpointContextMusicConfig"`
														} `json:"browseEndpointContextSupportedConfigs"`
													} `json:"browseEndpoint"`
												} `json:"navigationEndpoint"`
											} `json:"runs"`
										} `json:"subtitle"`
									} `json:"musicCardShelfRenderer"`
								} `json:"contents"`
							} `json:"sectionListRenderer"`
						} `json:"content"`
					} `json:"tabRenderer"`
				} `json:"tabs"`
			} `json:"tabbedSearchResultsRenderer"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}

	var out []SearchCandidate
	for _, tab := range doc.Contents.TabbedSearchResultsRenderer.Tabs {
		for _, section := range tab.TabRenderer.Content.SectionListRenderer.Contents {
			shelf := section.MusicCardShelfRenderer
			if shelf == nil {
				continue
			}
			var c SearchCandidate
			for _, run := range shelf.Title.Runs {
				if run.NavigationEndpoint.WatchEndpoint.VideoID != "" {
					c.VideoID = run.NavigationEndpoint.WatchEndpoint.VideoID
					c.Title = run.Text
				}
			}
			for _, run := range shelf.Subtitle.Runs {
				pageType := run.NavigationEndpoint.BrowseEndpoint.
					BrowseEndpointContextSupportedConfigs.
					BrowseEndpointContextMusicConfig.PageType
				switch {
				case pageType == "MUSIC_PAGE_TYPE_ARTIST":
					c.Artist = run.Text
				case strings.Contains(run.Text, ":"):
					c.DurationSeconds = parseDuration(run.Text)
				}
			}
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatalf("fixture %s yielded no candidates -- the fixture shape changed", name)
	}
	return out
}

// parseDuration converts a colon-separated "M:SS" or "H:MM:SS" fixture label to
// seconds, returning 0 (the documented "not supplied, fails open" sentinel) if
// any part is not an integer.
//
// It does NOT validate ranges: "3:75" parses to 255 rather than being refused.
// That is deliberate for a test helper reading a fixture this repo controls --
// the parser under test is parseDurationSeconds, which DOES range-check, and
// duplicating that here would let this helper disagree with it. Do not reuse
// this on untrusted input.
func parseDuration(s string) int {
	parts := strings.Split(strings.TrimSpace(s), ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// TestSelectCandidateRejectsTheMeasuredNonsenseCase is the core regression
// test: the #848 spike's measured no-match response, which is structurally
// indistinguishable from a real hit and carries a valid videoId and a real
// lyrics tab, must be REJECTED. Accepting it is what would write another
// song's words next to the user's audio.
func TestSelectCandidateRejectsTheMeasuredNonsenseCase(t *testing.T) {
	candidates := loadSearchCandidates(t, "search_nonsense.json")

	// The requested track is what the nonsense query asked for; the fixture's
	// candidate is an unrelated top hit.
	requested := models.Track{
		ArtistName:  "Flibbertigibbet Wonkabazoo",
		TrackName:   "Nonsense Query Zzzzz9999",
		TrackLength: 126,
	}

	got, err := SelectCandidate(candidates, requested)
	if err == nil {
		t.Fatalf("nonsense search response was ACCEPTED (videoID %q) -- the correspondence guard did not fire", got.VideoID)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error must wrap ErrNotFound so callers bucket it as a clean miss, got %v", err)
	}
	if got.VideoID != "" {
		t.Errorf("a rejected selection must return a zero candidate, got videoID %q", got.VideoID)
	}
	assertErrorCarriesNoFieldValues(t, err, requested, candidates[0])
}

// TestSelectCandidateAcceptsACorrectResolution is the MANDATORY positive
// control. Without it a guard that rejects everything would pass the nonsense
// regression test perfectly and prove nothing.
func TestSelectCandidateAcceptsACorrectResolution(t *testing.T) {
	candidates := loadSearchCandidates(t, "search.json")

	requested := models.Track{
		ArtistName:  candidates[0].Artist,
		TrackName:   candidates[0].Title,
		TrackLength: candidates[0].DurationSeconds,
	}

	got, err := SelectCandidate(candidates, requested)
	if err != nil {
		t.Fatalf("a correct resolution was REJECTED: %v", err)
	}
	if got.VideoID != candidates[0].VideoID {
		t.Errorf("videoID = %q, want %q", got.VideoID, candidates[0].VideoID)
	}
	if got.DurationSeconds == 0 {
		t.Error("the fixture's duration did not survive parsing; the duration signal 853b ranks on would be untested")
	}
}

// TestMeasuredSpikeConfidencesStraddleTheFloor pins the numeric basis of the
// guard. If a future change to normalize.MatchConfidence or to the floor moved
// either measured case across 0.75, this slice's central claim would be false;
// the fixture tests alone would not necessarily notice, because they use
// identical-vs-unrelated strings rather than the measured near-miss values.
func TestMeasuredSpikeConfidencesStraddleTheFloor(t *testing.T) {
	// Correct resolution: identical values, both fields at 1.000, accepted.
	if err := checkCorresponds(
		models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title"},
		SearchCandidate{Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"},
	); err != nil {
		t.Errorf("identical fields must correspond, got %v", err)
	}

	// Nonsense: unrelated values on both fields, both far below the floor.
	if err := checkCorresponds(
		models.Track{ArtistName: "Flibbertigibbet Wonkabazoo", TrackName: "Nonsense Query Zzzzz9999"},
		SearchCandidate{Artist: "Unrelated Placeholder Artist", Title: "Unrelated Placeholder Title"},
	); err == nil {
		t.Error("unrelated fields must NOT correspond")
	}

	// The floor itself. ENDPOINTS ARE NOT ENOUGH: 1.0-vs-0.0 pins nothing
	// between them, so the floor could be moved anywhere from 0.60 to 0.99
	// with this test green (853-R5F1). These two values BRACKET 0.75 tightly
	// and are measured, not guessed -- see matchMinConfidence's doc.
	if fieldOK, _ := fieldCorresponds("abcdefgh", "abcdefgh"); !fieldOK {
		t.Error("an exact field match must clear the floor")
	}
	if fieldOK, _ := fieldCorresponds("abcdefgh", "zyxwvuts"); fieldOK {
		t.Error("a wholly different field must not clear the floor")
	}

	// 0.7597 -- the WEAKEST legitimate pair in the calibration corpus, a
	// leading article. It clears 0.75 by 0.01, so raising the floor at all
	// breaks a case the floor exists to admit.
	if fieldOK, _ := fieldCorresponds("Beatles", "The Beatles"); !fieldOK {
		t.Error("a leading-article pair scores 0.7597 and MUST clear the floor: raising it rejects legitimate matches")
	}

	// 0.6667 -- above the 0.60 a lowered floor would admit, below 0.75. It
	// must reject, which is what stops the floor being lowered silently.
	if fieldOK, _ := fieldCorresponds("Vanguard", "Songbird"); fieldOK {
		t.Error("an unrelated pair scores 0.6667 and must NOT clear the floor: lowering it admits mismatches")
	}
}

// TestEveryComparableFieldMustClearTheFloor covers the two realistic
// wrong-track classes the permissive one-field rule lets through: a cover
// (title exact, artist wrong) and a wrongly-served compilation track (artist
// exact, title wrong). Both are another song's words.
func TestEveryComparableFieldMustClearTheFloor(t *testing.T) {
	requested := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title"}

	tests := []struct {
		name       string
		candidate  SearchCandidate
		wantReject bool
	}{
		{
			name:       "both fields correspond",
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"},
			wantReject: false,
		},
		{
			name:       "cover: title exact, artist unrelated",
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Zzyzx Unrelated Performer", Title: "Placeholder Song Title"},
			wantReject: true,
		},
		{
			name:       "wrongly-served compilation: artist exact, title unrelated",
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Zzyzx Unrelated Track"},
			wantReject: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SelectCandidate([]SearchCandidate{tc.candidate}, requested)
			if tc.wantReject && err == nil {
				t.Error("candidate was accepted but must be rejected")
			}
			if !tc.wantReject && err != nil {
				t.Errorf("candidate was rejected but must be accepted: %v", err)
			}
		})
	}
}

// TestBlankFieldIsSkippedNeverCountedAsAPass is the subtle half of the gate. A
// blank field is NOT COMPARABLE, so it is skipped -- which means when only one
// field is comparable, that one field alone must still clear the floor. A
// blank that counted as a pass would let a single wrong field through.
func TestBlankFieldIsSkippedNeverCountedAsAPass(t *testing.T) {
	tests := []struct {
		name       string
		requested  models.Track
		candidate  SearchCandidate
		wantReject bool
	}{
		{
			name:       "blank requested artist, title corresponds",
			requested:  models.Track{TrackName: "Placeholder Song Title"},
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"},
			wantReject: false,
		},
		{
			name:       "blank requested artist, title does NOT correspond",
			requested:  models.Track{TrackName: "Placeholder Song Title"},
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Zzyzx Unrelated Track"},
			wantReject: true,
		},
		{
			name:       "blank candidate title, artist corresponds",
			requested:  models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title"},
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name"},
			wantReject: false,
		},
		{
			name:       "blank candidate title, artist does NOT correspond",
			requested:  models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title"},
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Zzyzx Unrelated Performer"},
			wantReject: true,
		},
		{
			name:       "whitespace-only fields are not comparable",
			requested:  models.Track{ArtistName: "   ", TrackName: "Placeholder Song Title"},
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Zzyzx Unrelated Performer", Title: "Placeholder Song Title"},
			wantReject: false,
		},
		{
			name:       "no comparable field at all is rejected, not accepted",
			requested:  models.Track{},
			candidate:  SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"},
			wantReject: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SelectCandidate([]SearchCandidate{tc.candidate}, tc.requested)
			if tc.wantReject && err == nil {
				t.Error("candidate was accepted but must be rejected")
			}
			if !tc.wantReject && err != nil {
				t.Errorf("candidate was rejected but must be accepted: %v", err)
			}
		})
	}
}

// TestSelectCandidateGatesEveryCandidateNotJustTheWinner is the "best of a bad
// set" guard. Ranking alone would return the highest-scoring candidate even
// when every candidate is wrong.
func TestSelectCandidateGatesEveryCandidateNotJustTheWinner(t *testing.T) {
	requested := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title", TrackLength: 126}

	// All three are unrelated; one is a slightly better unrelated match than
	// the others. A rank-then-return implementation returns it.
	candidates := []SearchCandidate{
		{VideoID: "a", Artist: "Zzyzx Unrelated Performer", Title: "Zzyzx Unrelated Track", DurationSeconds: 126},
		{VideoID: "b", Artist: "Qqqqq Other Performer", Title: "Qqqqq Other Track", DurationSeconds: 240},
		{VideoID: "c", Artist: "Wwwww Third Performer", Title: "Wwwww Third Track", DurationSeconds: 126},
	}

	if got, err := SelectCandidate(candidates, requested); err == nil {
		t.Fatalf("a set with no corresponding candidate was accepted (videoID %q)", got.VideoID)
	}

	// GATE-FIRST, NOT RANK-THEN-GATE. This is the case that distinguishes the
	// two arrangements, and without it both pass: removing the per-candidate
	// gate entirely still satisfies every other test here (measured -- the
	// mutation went GREEN before this case existed).
	//
	// The "outranks" candidate has an EXACT artist (1.0000) and a sub-floor
	// title (0.6773), so it scores 16.773 and FAILS the gate. The "good"
	// candidate is the weakest legitimate shape, a leading article on both
	// fields (0.7840 / 0.8434), so it scores 16.274 and PASSES the gate.
	//
	// Rank-then-gate picks "outranks", the gate then rejects it, and "good" is
	// lost -- a false reject manufactured entirely by ranking a candidate that
	// was never eligible. Gate-first cannot reach that state: an ineligible
	// candidate never enters the ranking at all.
	rankedAboveButIneligible := []SearchCandidate{
		{VideoID: "outranks", Artist: "Placeholder Artist Name", Title: "Different Song Title", DurationSeconds: 126},
		{VideoID: "good", Artist: "The Placeholder Artist Name", Title: "A Placeholder Song Title", DurationSeconds: 126},
	}
	// Assert the premise rather than assuming it: the ineligible candidate
	// really does out-rank the eligible one, so this test would be vacuous if
	// normalize.MatchConfidence ever moved.
	if scoreCandidate(rankedAboveButIneligible[0], requested) <= scoreCandidate(rankedAboveButIneligible[1], requested) {
		t.Fatal("test premise broken: the gate-failing candidate no longer out-ranks the gate-passing one, so this case no longer distinguishes gate-first from rank-then-gate")
	}
	if err := checkCorresponds(requested, rankedAboveButIneligible[0]); err == nil {
		t.Fatal("test premise broken: the higher-ranked candidate is supposed to FAIL the gate")
	}
	gotGF, err := SelectCandidate(rankedAboveButIneligible, requested)
	if err != nil {
		t.Fatalf("gate-passing candidate was lost because a gate-FAILING candidate out-ranked it: %v", err)
	}
	if gotGF.VideoID != "good" {
		t.Errorf("videoID = %q, want %q", gotGF.VideoID, "good")
	}

	// And a corresponding candidate buried at the end of a bad set must still
	// be found, not shadowed by the better-ranked wrong ones.
	withGood := append(append([]SearchCandidate{}, candidates...),
		SearchCandidate{VideoID: "good", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 400})
	got, err := SelectCandidate(withGood, requested)
	if err != nil {
		t.Fatalf("a corresponding candidate in a bad set was rejected: %v", err)
	}
	if got.VideoID != "good" {
		t.Errorf("videoID = %q, want %q -- a non-corresponding candidate displaced the corresponding one", got.VideoID, "good")
	}
}

// TestDurationRanksButNeverRejects pins the explicit duration decision: it
// orders candidates that have already cleared the gate, and it can never on
// its own cause a rejection.
func TestDurationRanksButNeverRejects(t *testing.T) {
	requested := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title", TrackLength: 200}

	t.Run("a wildly wrong duration does not reject a corresponding candidate", func(t *testing.T) {
		c := SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 4000}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("duration must not reject: %v", err)
		}
	})

	t.Run("a zero duration does not reject and does not penalize", func(t *testing.T) {
		zero := SearchCandidate{VideoID: "zero", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"}
		if _, err := SelectCandidate([]SearchCandidate{zero}, requested); err != nil {
			t.Errorf("an absent duration must fail open: %v", err)
		}
		// Against an equally-corresponding candidate WITH a matching duration,
		// the one with evidence wins; the zero one is not penalized below a
		// candidate whose duration is far off, it simply scores no bonus.
		far := SearchCandidate{VideoID: "far", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 4000}
		if scoreCandidate(zero, requested) != scoreCandidate(far, requested) {
			t.Error("an absent duration and an out-of-tolerance duration must both contribute zero")
		}
	})

	t.Run("among gate-passing candidates the closer duration wins", func(t *testing.T) {
		candidates := []SearchCandidate{
			{VideoID: "far", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 260},
			{VideoID: "near", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 200},
		}
		got, err := SelectCandidate(candidates, requested)
		if err != nil {
			t.Fatalf("unexpected rejection: %v", err)
		}
		if got.VideoID != "near" {
			t.Errorf("videoID = %q, want \"near\"", got.VideoID)
		}
	})

	t.Run("duration never outvotes a materially stronger text signal", func(t *testing.T) {
		// A weaker-but-passing text match with a PERFECT duration must NOT
		// beat a perfect text match with a wildly wrong duration.
		//
		// The weaker candidate's fields are the leading-article shape, which
		// is the weakest LEGITIMATE variance musixmatch measured (0.8051
		// there). Measured here with normalize.MatchConfidence: artist 0.7840
		// ("The " prefix), title 0.8434 ("A " prefix) -- both above the 0.75
		// floor, so both candidates genuinely pass the gate and this is a
		// RANKING question, not a gating one. Combined text 1.627 vs 2.000 is
		// a 0.373 gap, well outside the 0.1 window inside which duration is
		// allowed to decide, so text must win by construction.
		req := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title", TrackLength: 200}
		candidates := []SearchCandidate{
			{VideoID: "durationalike", Artist: "The Placeholder Artist Name", Title: "A Placeholder Song Title", DurationSeconds: 200},
			{VideoID: "textalike", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 4000},
		}
		got, err := SelectCandidate(candidates, req)
		if err != nil {
			t.Fatalf("unexpected rejection: %v", err)
		}
		if got.VideoID != "textalike" {
			t.Errorf("videoID = %q, want \"textalike\" -- duration outvoted the stronger text signal", got.VideoID)
		}
	})

	t.Run("inside the tie-break window duration does decide", func(t *testing.T) {
		// The complement of the case above, and the reason duration is scored
		// at all: when the text signals are effectively indistinguishable
		// (both ~0.99 here, a combined gap under the 0.1 window), the closer
		// duration is the only evidence left and must break the tie.
		req := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title", TrackLength: 200}
		candidates := []SearchCandidate{
			{VideoID: "near", Artist: "Placeholder Artist Names", Title: "Placeholder Song Titles", DurationSeconds: 200},
			{VideoID: "far", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 4000},
		}
		got, err := SelectCandidate(candidates, req)
		if err != nil {
			t.Fatalf("unexpected rejection: %v", err)
		}
		if got.VideoID != "near" {
			t.Errorf("videoID = %q, want \"near\" -- duration failed to break a text tie", got.VideoID)
		}
	})
}

// TestSelectCandidateEmptyAndUnusableInputs covers the degenerate inputs.
func TestSelectCandidateEmptyAndUnusableInputs(t *testing.T) {
	requested := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title"}

	t.Run("empty list", func(t *testing.T) {
		_, err := SelectCandidate(nil, requested)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("a corresponding candidate with no videoID cannot be continued", func(t *testing.T) {
		c := SearchCandidate{Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("a usable candidate is still found alongside an unusable one", func(t *testing.T) {
		candidates := []SearchCandidate{
			{Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"},
			{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"},
		}
		got, err := SelectCandidate(candidates, requested)
		if err != nil {
			t.Fatalf("unexpected rejection: %v", err)
		}
		if got.VideoID != "vid" {
			t.Errorf("videoID = %q, want \"vid\"", got.VideoID)
		}
	})
}

// assertErrorCarriesNoFieldValues enforces the privacy constraint: the
// rejection error reaches logs and the failure-analysis report, and the
// library's track metadata is private, so no artist, title or videoId value
// may appear in the message.
func assertErrorCarriesNoFieldValues(t *testing.T, err error, requested models.Track, c SearchCandidate) {
	t.Helper()
	msg := err.Error()
	for _, v := range []string{requested.ArtistName, requested.TrackName, c.Artist, c.Title, c.VideoID} {
		if v == "" {
			continue
		}
		if strings.Contains(msg, v) {
			t.Errorf("error message leaks a field value; the message must carry none")
		}
	}
}

// TestSelectCandidateWithMultipleSurvivors covers the case every other test in
// this file avoids by construction: MORE THAN ONE candidate clears the gate.
// TestSelectCandidateGatesEveryCandidateNotJustTheWinner deliberately builds a
// set with exactly one passer, so first-wins was never exercised and removing
// the loop's break survived mutation (853-R5F2).
//
// This slice's driver is a documented PLACEHOLDER -- it takes the first
// survivor, which is not a judgment about which is better. The point of
// pinning it is that 853b REPLACES this policy with a ranker, and a policy
// change should show up as a diff in behavior rather than passing silently.
func TestSelectCandidateWithMultipleSurvivors(t *testing.T) {
	requested := models.Track{ArtistName: "Placeholder Artist", TrackName: "Placeholder Title"}
	candidates := []SearchCandidate{
		{VideoID: "firstPasser", Artist: "Placeholder Artist", Title: "Placeholder Title"},
		{VideoID: "secondPasser", Artist: "Placeholder Artist", Title: "Placeholder Title"},
	}

	got, err := SelectCandidate(candidates, requested)
	if err != nil {
		t.Fatalf("with two corresponding candidates, one must be selected: %v", err)
	}
	if got.VideoID != "firstPasser" {
		t.Errorf("selected %q, want %q: this slice takes the FIRST survivor", got.VideoID, "firstPasser")
	}

	// Whichever is returned must itself have cleared the gate -- the package's
	// whole promise. This holds regardless of which policy selects it, so it
	// survives 853b's replacement of first-wins.
	if err := checkCorresponds(requested, got); err != nil {
		t.Errorf("the selected candidate must clear the gate: %v", err)
	}
}
