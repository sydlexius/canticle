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
	"github.com/sydlexius/canticle/internal/normalize"
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
		//
		// RE-FIXTURED BY THE F1 TOKEN RULE, deliberately. This case originally
		// built its near-tie from PLURALIZED fields ("...Titles"), which the
		// title token rule now rejects as a sibling title -- correctly, that
		// being the measured silent-corruption class. A gate-failing candidate
		// cannot exercise a RANKING question, so the tie is rebuilt from a
		// variant decoration instead: "(Live)" scores 0.9517 on the title and
		// passes the gate, leaving a combined text gap of 0.0483 -- still
		// inside the 0.1 window where duration is allowed to decide, which is
		// the property this case exists to pin.
		req := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title", TrackLength: 200}
		candidates := []SearchCandidate{
			{VideoID: "near", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title (Live)", DurationSeconds: 200},
			{VideoID: "far", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title", DurationSeconds: 4000},
		}
		// Assert the premise: both candidates must genuinely clear the gate,
		// or this is a gating test wearing a ranking test's name.
		for _, c := range candidates {
			if err := checkCorresponds(req, c); err != nil {
				t.Fatalf("test premise broken: candidate %q must pass the gate for this to be a ranking question: %v", c.VideoID, err)
			}
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
	// The SLICE-FIRST candidate is lexically LARGER, so first-wins and the
	// tie-break disagree here. That is deliberate: 853a took the first
	// survivor and this slice replaced that with the ranker, so a test whose
	// two orderings agree would pass under either policy and pin neither
	// (877-R1F2). Under the ranker these tie on score and "aaa" wins.
	candidates := []SearchCandidate{
		{VideoID: "zzz-slice-first", Artist: "Placeholder Artist", Title: "Placeholder Title"},
		{VideoID: "aaa-lexically-first", Artist: "Placeholder Artist", Title: "Placeholder Title"},
	}

	got, err := SelectCandidate(candidates, requested)
	if err != nil {
		t.Fatalf("with two corresponding candidates, one must be selected: %v", err)
	}
	if got.VideoID != "aaa-lexically-first" {
		t.Errorf("selected %q, want %q: equal scores break on the smallest videoID, not on slice position", got.VideoID, "aaa-lexically-first")
	}

	// Whichever is returned must itself have cleared the gate -- the package's
	// whole promise. This holds regardless of which policy selects it, so it
	// survives 853b's replacement of first-wins.
	if err := checkCorresponds(requested, got); err != nil {
		t.Errorf("the selected candidate must clear the gate: %v", err)
	}
}

// TestSelectCandidateWinnerIsIndependentOfInputOrder pins the tie-break
// (853-R5F1). Ties are the COMMON shape, not a corner case: duplicate uploads
// of one track carry identical artist, title and duration and therefore score
// identically. Before this, the winner was whichever tied candidate innertube
// happened to return first, so the same result set could select a different
// lyric on two identical searches. Shuffling must not move the winner.
func TestSelectCandidateWinnerIsIndependentOfInputOrder(t *testing.T) {
	requested := models.Track{ArtistName: "Placeholder Artist", TrackName: "Placeholder Title", TrackLength: 200}
	tied := []SearchCandidate{
		{VideoID: "vidC", Artist: "Placeholder Artist", Title: "Placeholder Title", DurationSeconds: 200},
		{VideoID: "vidA", Artist: "Placeholder Artist", Title: "Placeholder Title", DurationSeconds: 200},
		{VideoID: "vidB", Artist: "Placeholder Artist", Title: "Placeholder Title", DurationSeconds: 200},
	}

	first, err := SelectCandidate(tied, requested)
	if err != nil {
		t.Fatalf("SelectCandidate: %v", err)
	}
	// Assert WHICH candidate wins, not merely that the answer is stable
	// (877-R1F1). Comparing every permutation against an unasserted baseline
	// pins consistency only, so a reverse-lexical policy would pass this test
	// unchanged. "vidA" is the lexicographically smallest videoID in the set.
	if first.VideoID != "vidA" {
		t.Fatalf("tie-break selected %q, want %q: ties break on the smallest videoID", first.VideoID, "vidA")
	}

	// Every permutation of the same set must select the same candidate.
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, perm := range perms {
		shuffled := make([]SearchCandidate, 0, len(tied))
		for _, i := range perm {
			shuffled = append(shuffled, tied[i])
		}
		got, err := SelectCandidate(shuffled, requested)
		if err != nil {
			t.Fatalf("permutation %v: %v", perm, err)
		}
		if got.VideoID != first.VideoID {
			t.Errorf("permutation %v selected %q, want %q: the winner must not depend on input order", perm, got.VideoID, first.VideoID)
		}
	}
}

// TestTitleContributesToTheRanking pins the title half of "text dominates".
// Deleting the title term from scoreCandidate left the whole suite green,
// because every other ranking case is already ordered correctly by its artist
// term alone (853-R5F2). These candidates share an artist and an exact
// duration, so the title is the ONLY signal that can separate them.
func TestTitleContributesToTheRanking(t *testing.T) {
	requested := models.Track{ArtistName: "Placeholder Artist", TrackName: "Placeholder Title", TrackLength: 200}
	// TWO constraints on the weaker candidate, and this slice broke the second
	// one (879-R1F1). It must (a) sort BEFORE the exact match so the tie-break
	// favors the WRONG one -- otherwise deleting the title term makes them tie
	// and the tie-break picks correctly by accident -- and (b) still PASS the
	// gate, so it reaches the ranker at all.
	//
	// A pluralized title satisfied (a) but stopped satisfying (b) once this
	// slice's token rule landed: "titles" is an unmatched content token, so
	// the candidate is now rejected as a sibling and filtered before scoring,
	// leaving one survivor and no ranking decision to make. A decorated
	// variant is the right shape: "(Live)" is packaging, so it passes the
	// token rule, while scoring lower than the exact match.
	candidates := []SearchCandidate{
		{VideoID: "aaa-wrongish", Artist: "Placeholder Artist", Title: "Placeholder Title (Live)", DurationSeconds: 200},
		{VideoID: "zzz-exact", Artist: "Placeholder Artist", Title: "Placeholder Title", DurationSeconds: 200},
	}

	got, err := SelectCandidate(candidates, requested)
	if err != nil {
		t.Fatalf("SelectCandidate: %v", err)
	}
	if got.VideoID != "zzz-exact" {
		t.Errorf("selected %q, want %q: with artist and duration identical, the title must decide", got.VideoID, "zzz-exact")
	}
}

// TestSiblingTitleIsRejected is the F1 regression test: a candidate whose
// title differs from the requested one by a CONTENT token is a DIFFERENT SONG
// and must be rejected, even though it scores far above the floor.
//
// Both cases below were measured ACCEPTED before the token-multiset rule was
// added, with the artist held identical so the title is the only variable. No
// floor value separates them -- the weakest LEGITIMATE shape (a leading
// article) scores 0.8426, below both -- which is why the fix is structural
// rather than a tuning change.
//
// THE REASON THIS CLASS IS CRITICAL rather than a scoring nit: the safety net
// below this gate is internal/timing.Evaluate, which catches a grossly wrong
// RUNTIME. A sibling track from the same release runs about as long, so its
// cues never overrun, timing returns Ok, and another song's words are promoted
// to a .lrc beside the user's audio.
func TestSiblingTitleIsRejected(t *testing.T) {
	const artist = "Placeholder Artist Name"
	requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title"}

	tests := []struct {
		name  string
		title string
	}{
		{name: "pluralized sibling", title: "Placeholder Song Titles"},
		{name: "part-number sibling", title: "Placeholder Song Title Pt.2"},
		{name: "spelled-out part number", title: "Placeholder Song Title Part 2"},
		{name: "roman-numeral sequel", title: "Placeholder Song Title II"},
		{name: "extra content word", title: "Placeholder Song Title Reprise"},
		{name: "dropped content word", title: "Placeholder Song"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Assert the PREMISE: this case is only meaningful while the
			// candidate clears the similarity floor. If it ever dropped below,
			// the floor would be rejecting it and this test would no longer
			// prove the token rule does anything.
			if conf := normalize.MatchConfidence(requested.TrackName, tc.title); conf < matchMinConfidence {
				t.Fatalf("test premise broken: title confidence %.4f is already below the %.2f floor, so this case no longer exercises the token rule", conf, matchMinConfidence)
			}

			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: tc.title}
			got, err := SelectCandidate([]SearchCandidate{c}, requested)
			if err == nil {
				t.Fatalf("a sibling title was ACCEPTED (videoID %q) -- this writes another song's words next to the user's audio", got.VideoID)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("error must wrap ErrNotFound, got %v", err)
			}
		})
	}
}

// TestMeasuredSiblingPairsFromIssue883 pins the FOUR EXACT PAIRS issue #883
// measured, as that issue's first acceptance criterion requires.
//
// These four were ALREADY closed when this test was written, by #879's token
// rule rather than by anything here. #883 was filed against #853's gate,
// before that rule existed, and predicted it would catch "several" of these
// but not `Intro` vs `Interlude` -- reasoning the pair "shares no unmatched
// content token to reject on". That misreads the rule: titleTokensCorrespond
// rejects when EITHER side carries an unmatched non-packaging token. A shared
// token is needed to ACCEPT, not to reject, so `intro` and `interlude` each
// reject on their own.
//
// THE CLASS DID NOT CLOSE WITH THEM, which is why yearsDiffer exists in
// selection.go. An adversarial review found that #879 also CREATED a sibling
// class while fixing this one: every four-digit year was packaging, so
// "Nocturne 1984" vs "Nocturne 2019" accepted at 0.9385 and
// "Placeholder 1990" vs "Placeholder 1991" at 0.9750 -- the same confidence as
// row 2 below. Checking only the four pairs an issue happens to list, and
// generalizing from them, is how that was missed.
//
// A THRESHOLD CANNOT DO THIS JOB, and that part of the original reasoning
// stands. These pairs score 0.8237 to 0.9818, ABOVE the weakest legitimate
// field in the calibration corpus (0.7597, a leading article), so any floor
// high enough to reject them rejects that too. Both guards are structural
// instead: the token rule separates on unmatched content, yearsDiffer on
// whether a year is carrying identity or a pressing.
//
// Each row asserts its premise before its verdict, so a case that stops
// exercising the rule fails loudly rather than passing vacuously.
func TestMeasuredSiblingPairsFromIssue883(t *testing.T) {
	const artist = "Placeholder Artist Name"

	// The confidences #883 measured, reproduced here so a drift in
	// normalize.MatchConfidence is visible as a failing premise rather than as
	// a silently different test.
	tests := []struct {
		name         string
		requested    string
		candidate    string
		measuredConf float64
	}{
		{"roman numeral movement", "Movement I", "Movement II", 0.9818},
		{"numbered untitled track", "Untitled Track 1", "Untitled Track 2", 0.9750},
		{"spelled-out suite part", "Suite Part One", "Suite Part Two", 0.9429},
		// The pair #883 predicted would survive. It is the shape unlike every
		// other case in this file: no shared base, no appended or dropped
		// token -- two entirely different short words that Jaro-Winkler's
		// prefix scaling pulls above the floor.
		{"short unrelated neighbors", "Intro", "Interlude", 0.8237},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// PREMISE 1: the pair still clears the floor. If it did not, the
			// floor would be doing the rejecting and this case would prove
			// nothing about the token rule.
			conf := normalize.MatchConfidence(tc.requested, tc.candidate)
			if conf < matchMinConfidence {
				t.Fatalf("test premise broken: confidence %.4f is below the %.2f floor, "+
					"so this case no longer exercises the token rule", conf, matchMinConfidence)
			}
			// PREMISE 2: the confidence is still what #883 measured. A drift
			// here does not necessarily break the guard, but it means the
			// issue's table no longer describes this code.
			if diff := conf - tc.measuredConf; diff > 0.0001 || diff < -0.0001 {
				t.Errorf("confidence %.4f differs from the %.4f measured in #883; "+
					"the issue's analysis no longer describes this code", conf, tc.measuredConf)
			}

			requested := models.Track{ArtistName: artist, TrackName: tc.requested}
			// Artist held IDENTICAL, as #883 measured it: the artist field
			// contributes nothing to separating siblings, so the title is the
			// only evidence available.
			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: tc.candidate}

			got, err := SelectCandidate([]SearchCandidate{c}, requested)
			if err == nil {
				t.Fatalf("a sibling track was ACCEPTED (videoID %q) at confidence %.4f -- "+
					"this writes another song's words to a .lrc next to the user's audio, "+
					"and internal/timing cannot catch it because a sibling from the same "+
					"release runs about as long", got.VideoID, conf)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("error must wrap ErrNotFound so the lane classifies it as a "+
					"benign miss rather than a transport failure, got %v", err)
			}
		})
	}
}

// TestTokenRuleClausesAreIndividuallyPinned covers the specific clauses of
// titleTokensCorrespond that the rows above do NOT discriminate.
//
// Measured: the four #883 rows survive two separate mutations that each break
// the rule. Deleting the shared-content-token requirement leaves them green,
// because every one of them also has an unmatched content token. Adding
// `part`/`pt` to the variant vocabulary -- the exact widening the exclusion at
// titleVariantTokens calls load bearing -- ALSO leaves them green, including
// the `Suite Part One`/`Suite Part Two` row, because `one` and `two` carry
// that rejection by themselves. That row does not test what its name implies.
//
// A regression net that only re-covers what other tests already catch is
// documentation, not a guard. These rows are chosen so each fails against one
// mutation and nothing else does.
func TestTokenRuleClausesAreIndividuallyPinned(t *testing.T) {
	const artist = "Placeholder Artist Name"

	// EVERY REJECT ROW ASSERTS ITS FLOOR PREMISE FIRST, and the asymmetry is the
	// reason. SelectCandidate can reject at the similarity floor BEFORE
	// titleTokensCorrespond runs, so a REJECT row would pass without ever
	// reaching the clause it names if normalization ever moved that pair below
	// the floor -- silently, for the wrong reason. An ACCEPT row needs no such
	// premise: a floor rejection there fails the test loudly, which is the
	// correct direction to be wrong in.
	//
	// ISOLATION IS THE WHOLE POINT OF THESE ROWS, and it is easy to get wrong.
	// A pair whose unmatched tokens are CONTENT rejects at the unmatched-token
	// clause and never reaches the clause under test, so it pins nothing.
	// Measured: "Alpha Live" vs "Beta Live" leaves `alpha`/`beta` unmatched and
	// rejects there, and "Song Part 1" vs "Song Part 2" leaves `1`/`2`
	// unmatched and rejects there -- neither notices its clause being deleted.
	// Each row below leaves ONLY the token under test unmatched.

	t.Run("a differing part marker rejects on `part` alone", func(t *testing.T) {
		// `part` is the ONLY unmatched token: no ordinal, no digit to carry the
		// rejection. That makes this the one shape that fails when `part` is
		// added to the variant vocabulary, which the exclusion at
		// titleVariantTokens calls load bearing.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Title Part"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor, "+
				"so the floor rejects this pair and the row no longer exercises the "+
				"variant vocabulary", conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("a title differing only by `part` was ACCEPTED -- a part marker " +
				"names a different song and must never become vocabulary")
		}
	})

	t.Run("differing years are rejected", func(t *testing.T) {
		// The C1 class, isolated the same way: both sides reduce to
		// [shared content, year], so the year is the only unmatched token and
		// yearsDiffer is the only thing that can reject.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Nocturne 1984"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Nocturne 2019"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor, "+
				"so the floor rejects this pair and the row no longer exercises "+
				"yearsDiffer", conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("two differently-dated works were ACCEPTED -- when both titles " +
				"carry a year and the years disagree, the year is the identity, not " +
				"the pressing")
		}
	})

	t.Run("differing performance tokens are rejected", func(t *testing.T) {
		// #890 row 3, and the shape the class is named for: a live take and a
		// demo are different RECORDINGS, not two pressings of one. Both sides
		// carry a performance token and they disagree, so the performance is
		// the identity.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Live"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Demo"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor, "+
				"so the floor rejects this pair and the row no longer exercises the "+
				"performance discriminator", conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("a live take was ACCEPTED for a demo -- these name different " +
				"performances, which can carry different words")
		}
	})

	t.Run("a radio cut and a live cut still ACCEPT, deliberately", func(t *testing.T) {
		// #890 listed this pair as a defect. DECIDED THE OTHER WAY, which its
		// AC 1 explicitly permits ("or whether the current accept is correct
		// and the cases above are an accepted cost"), and the row is kept as an
		// ACCEPT rather than deleted so the decision is pinned instead of
		// merely absent.
		//
		// A radio edit is a PRESSING of the studio take -- shortened, cleaned --
		// not a different performance, so `radio` stays plain packaging. When
		// this pair rejected, it rejected on the token `radio` alone, which is
		// the wrong REASON even where the verdict looks arguable: it also
		// rejected "(Radio Edit)" against a live take while accepting a bare
		// title against either.
		//
		// The words are the same either way, and timing.Evaluate is a real net
		// beneath this gate for a runtime mismatch. A false ACCEPT here is the
		// documented, reasoned kind.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song (Radio Edit)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (Live)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a radio edit was REJECTED against a live take -- `radio` names a "+
				"pressing, not a performance, and must not discriminate: %v", err)
		}
	})

	t.Run("a session and a demo are rejected", func(t *testing.T) {
		// #890 row 2.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Session"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Demo"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("a session take was ACCEPTED for a demo")
		}
	})

	t.Run("a live take and a live INSTRUMENTAL are rejected", func(t *testing.T) {
		// 891-R1C2b: the worst multi-token case. An earlier revision returned
		// as soon as ANY one performance token matched, so a live take and a
		// live instrumental of the same song accepted on the shared `live` --
		// one of them has no sung words at all. The rule compares the FAMILY
		// SETS, so naming an extra family is a difference.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song (Live)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (Live Instrumental)"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("a live take was ACCEPTED for a live INSTRUMENTAL -- sharing the " +
				"`live` token must not admit a wordless cut")
		}
	})

	t.Run("a live acoustic set and a live demo are rejected", func(t *testing.T) {
		// The same set-comparison property on two multi-token performances that
		// share a family but not the whole set.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song (Live Acoustic)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (Live Demo)"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("a live acoustic set was ACCEPTED for a live demo")
		}
	})

	t.Run("live, unplugged and acoustic are ONE family and accept each other", func(t *testing.T) {
		// 891-R1R1/R2, and the correction of a real false REJECT this branch
		// introduced. An unplugged set IS a live acoustic performance; these
		// are frequently the same recording marketed two ways, and the block
		// above titleVariantTokens already DECIDED that "(Live)" vs
		// "(Acoustic)" corresponds. Grouping them keeps that decision true
		// while still separating a live take from a demo.
		for _, pair := range [][2]string{
			{"Placeholder Song (Live)", "Placeholder Song (Unplugged)"},
			{"Placeholder Song (Live)", "Placeholder Song (Acoustic)"},
			{"Placeholder Song (Unplugged)", "Placeholder Song (Acoustic)"},
		} {
			requested := models.Track{ArtistName: artist, TrackName: pair[0]}
			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: pair[1]}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
				t.Errorf("%q vs %q was REJECTED -- these name ONE live performance and "+
					"carry the same words: %v", pair[0], pair[1], err)
			}
		}
	})

	t.Run("instrumental and karaoke are ONE family", func(t *testing.T) {
		// 891-R2V1: splitting `instrumental` into its own family left the suite
		// GREEN, so the grouping was unpinned. It is a deliberate LOOSENING --
		// both cuts are wordless, so neither can carry a sung lyric the other
		// lacks, and rejecting between them would buy nothing.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song (Instrumental)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (Karaoke)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("an instrumental was REJECTED against a karaoke cut -- both are "+
				"wordless, so they share a family deliberately: %v", err)
		}
	})

	t.Run("demo, session and take are SEPARATE families", func(t *testing.T) {
		// 891-R2V1: each was pinned only against `demo`, so merging session
		// into take (or either into the other) left the suite green. These are
		// distinct recording EVENTS, not three words for one, and each pair
		// must reject.
		for _, pair := range [][2]string{
			{"Placeholder Song (Session)", "Placeholder Song (Take)"},
			{"Placeholder Song (Session)", "Placeholder Song (Demo)"},
			{"Placeholder Song (Take)", "Placeholder Song (Demo)"},
		} {
			requested := models.Track{ArtistName: artist, TrackName: pair[0]}
			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: pair[1]}
			if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
				t.Fatalf("test premise broken: confidence %.4f is below the %.2f floor for %q vs %q",
					conf, matchMinConfidence, pair[0], pair[1])
			}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
				t.Errorf("%q vs %q was ACCEPTED -- these name different recording "+
					"events and must not share a family", pair[0], pair[1])
			}
		}
	})

	t.Run("radio is NOT a performance and joins no family", func(t *testing.T) {
		// 891-R2V1: mapping `radio` into the live family left the suite green,
		// so only a radio-in-a-NEW-family mutation was caught. A radio edit is
		// a PRESSING; if it joined the live family it would start rejecting
		// against a demo, which it must not.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song (Radio Edit)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (Demo)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a radio edit was REJECTED against a demo -- `radio` names a "+
				"pressing and must belong to no performance family: %v", err)
		}
	})

	t.Run("the credit tail is read from the FIRST marker", func(t *testing.T) {
		// 891-R2V1: matching the LAST marker instead of the first left the
		// suite green. With two markers, everything after the FIRST is credit,
		// so a differing first credit must reject even when the trailing text
		// matches.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song feat Alpha ft Gamma"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song feat Beta ft Gamma"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("credits differing at the FIRST marker were ACCEPTED -- the tail " +
				"must start at the first marker, not the last")
		}
	})

	t.Run("each performance family is pinned by a rejecting pair", func(t *testing.T) {
		// 891-R1V4: deleting `unplugged`, `acoustic`, `sessions` or `take` from
		// the vocabulary left the suite GREEN, so four members were unpinned and
		// a wrong classification of any of them was invisible. Each row below
		// dies if its token stops naming its family.
		//
		// Every pair contrasts against `demo`, whose own membership is pinned
		// separately above, so a row can only pass by the token under test
		// being recognized.
		for _, tc := range []struct{ name, title string }{
			{"unplugged", "Placeholder Song (Unplugged)"},
			{"acoustic", "Placeholder Song (Acoustic)"},
			{"sessions", "Placeholder Song (Sessions)"},
			{"take", "Placeholder Song (Take)"},
			{"live", "Placeholder Song (Live)"},
			{"karaoke", "Placeholder Song (Karaoke)"},
			{"instrumental", "Placeholder Song (Instrumental)"},
		} {
			requested := models.Track{ArtistName: artist, TrackName: tc.title}
			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (Demo)"}
			if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
				t.Fatalf("%s: test premise broken: confidence %.4f is below the %.2f floor",
					tc.name, conf, matchMinConfidence)
			}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
				t.Errorf("%q was ACCEPTED against a demo -- its performance family is "+
					"not being recognized", tc.name)
			}
		}
	})

	t.Run("a PACKAGING token still accepts against another packaging token", func(t *testing.T) {
		// The partition's whole point: packaging variants are the SAME take
		// issued differently, so they must keep accepting where performances do
		// not. This is what separates the #890 fix from simply making the
		// vocabulary stricter.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title (Remastered)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Title (Deluxe Edition)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("two PACKAGING variants were REJECTED -- a remaster and a deluxe "+
				"edition are the same take issued differently and carry the same "+
				"words: %v", err)
		}
	})

	t.Run("an instrumental and a live take are rejected", func(t *testing.T) {
		// #890's AC 3, the MEMBERSHIP half: `instrumental` is in
		// performanceTokens, so it discriminates against ANOTHER performance.
		// Without this row, deleting `instrumental` and `karaoke` from the set
		// leaves the suite green -- measured, and the reason this row exists.
		// The one-sided case is decided the other way, in the row below.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Instrumental"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Live"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("an instrumental was ACCEPTED for a live take -- one has no sung " +
				"words at all, so these cannot share a lyric")
		}
	})

	t.Run("a karaoke cut and a demo are rejected", func(t *testing.T) {
		// The membership pin for `karaoke`, the other token P4 showed unpinned.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Karaoke"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Demo"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("a karaoke cut was ACCEPTED for a demo")
		}
	})

	t.Run("a one-sided instrumental is still packaging", func(t *testing.T) {
		// #890 row 4, and #890's AC 3 decided EXPLICITLY: a one-sided
		// instrumental keeps ACCEPTING. TestLegitimateVariantsStayAccepted
		// already pins this shape, and it is repeated here so the decision sits
		// beside the rejects it is contrasted with rather than only in the
		// controls. Whether the FILE has words is the instrumental-detection
		// path's question, not a title comparison's.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Words"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Words Instrumental"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a one-sided instrumental was REJECTED -- the performance "+
				"discriminator must fire only when BOTH sides name a performance: %v", err)
		}
	})

	t.Run("differing guest credits are rejected", func(t *testing.T) {
		// #891, structurally the SAME shape as the year class above: a token
		// that is packaging when ONE-SIDED becomes identity when both sides
		// carry it and they disagree. tokenizeTitle truncates from the feat
		// marker onward, so both sides reduce to the identical base multiset
		// and the sameMultiset early-accept fires with no further check.
		//
		// A guest verse is lyrics, so this is another recording's words.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title feat Alpha"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Title feat Beta"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor, "+
				"so the floor rejects this pair and the row no longer exercises the "+
				"credit discriminator", conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("two differently-credited recordings were ACCEPTED -- when BOTH " +
				"titles carry a guest credit and the credits disagree, the credit is " +
				"identity, not packaging")
		}
	})

	t.Run("differing credits are rejected with the parenthesized marker", func(t *testing.T) {
		// The same class reached through "featuring" rather than "feat", so the
		// fix cannot pin one spelling of the marker and miss the others.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title (featuring Alpha)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Title (featuring Beta)"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("differently-credited recordings were ACCEPTED via `featuring`")
		}
	})

	t.Run("a title that is ENTIRELY a differing credit is rejected", func(t *testing.T) {
		// The worst shape in #891 and a DIFFERENT code path from the two above:
		// when the whole title is a credit, both sides tokenize to NOTHING and
		// titleTokensCorrespond fails OPEN by design, so no token evidence
		// exists at all. A fix wired after the sameMultiset early-accept would
		// miss this row entirely.
		requested := models.Track{ArtistName: artist, TrackName: "feat Alpha"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "feat Beta"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("two titles that are entirely DIFFERENT credits were ACCEPTED -- " +
				"the empty-token fail-open must not admit a pair whose only content " +
				"is a disagreeing credit")
		}
	})

	t.Run("a shared suffix after the credit does not dilute it", func(t *testing.T) {
		// 891-R1C1, the CRITICAL finding: creditTail returned everything after
		// the marker, so a version suffix landed in BOTH tails and the
		// comparison found a shared token. That reopened this defect for the
		// more common real title shape. Every earlier credit row had a
		// single-token tail, which is why nothing saw it.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song (feat. Alpha) [Remix]"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (feat. Beta) [Remix]"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("differing credits carrying the SAME trailing packaging token were " +
				"ACCEPTED -- the shared suffix must not count as shared credit")
		}
	})

	t.Run("a shared honorific does not dilute the credit", func(t *testing.T) {
		// The same hole reached without any suffix at all: "DJ"/"Lil"/"MC" and
		// band nouns are the dominant guest naming convention in exactly the
		// genres that carry a feat credit, and under an OVERLAP comparison the
		// shared honorific alone defeated the rule.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song feat DJ Alpha"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song feat DJ Beta"}
		if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
			t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor",
				conf, matchMinConfidence)
		}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
			t.Error("two different guests sharing an honorific were ACCEPTED")
		}
	})

	t.Run("an extra guest on one side still accepts", func(t *testing.T) {
		// Pins the SUBSET rule's permissive half. This is the case the
		// comparison is deliberately loose for, and nothing pinned it before:
		// swapping subset for strict equality left the suite green (891-R1V1),
		// so the reasoning paragraph was unenforced.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song feat Alpha"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song feat Alpha and Beta"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a credit listing an EXTRA guest was REJECTED -- one credit being a "+
				"superset of the other is the same collaboration: %v", err)
		}
	})

	t.Run("a guest whose NAME is a vocabulary word is still compared", func(t *testing.T) {
		// 891-R2C1, a regression the FIRST fix for this class introduced. The
		// packaging filter was applied unconditionally, so an act actually
		// named "Mono", "Radio" or "Clean" -- all real act names, all in
		// titleVariantTokens -- had its tail emptied, and an empty tail hits
		// creditsDiffer's fail-open, which ACCEPTS any differing credit. Four
		// measured rejects became accepts on the corruption axis.
		//
		// The filter now falls back to the unfiltered tail rather than
		// returning nothing, so a credit made entirely of vocabulary words is
		// compared as written.
		for _, pair := range [][2]string{
			{"Placeholder Song (feat. Mono)", "Placeholder Song (feat. Alpha)"},
			{"Placeholder Song (feat. Mono)", "Placeholder Song (feat. Stereo)"},
			{"Placeholder Song (feat. Radio)", "Placeholder Song (feat. Beta)"},
			{"Placeholder Song (feat. Clean)", "Placeholder Song (feat. Alpha)"},
		} {
			requested := models.Track{ArtistName: artist, TrackName: pair[0]}
			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: pair[1]}
			if conf := normalize.MatchConfidence(requested.TrackName, c.Title); conf < matchMinConfidence {
				t.Fatalf("test premise broken: confidence %.4f is below the %.2f floor for %q vs %q",
					conf, matchMinConfidence, pair[0], pair[1])
			}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
				t.Errorf("%q vs %q was ACCEPTED -- a guest named with a vocabulary word "+
					"must not empty the tail into the fail-open", pair[0], pair[1])
			}
		}
	})

	t.Run("identical vocabulary-word credits still accept", func(t *testing.T) {
		// The false-reject guard on the fallback: comparing the unfiltered tail
		// must not reject a credit that genuinely matches.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song (feat. Mono)"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song (feat. Mono)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("identical credits naming a vocabulary-word act were REJECTED: %v", err)
		}
	})

	t.Run("differing suffixes after matching credits still accept", func(t *testing.T) {
		// Pins the packaging filter in creditTail, which is a FALSE-REJECT
		// guard rather than a false-accept one (891-R1V2 showed it unpinned).
		// Tails here reduce to [alpha beta] and [alpha] -- a superset, so the
		// same collaboration accepts. Keeping the packaging tokens would make
		// them [alpha beta remix] and [alpha live], which share no subset
		// relation and would reject a legitimate pair on its DECORATION.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song feat Alpha and Beta [Remix]"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song feat Alpha [Live]"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a credit superset carrying a DIFFERENT trailing packaging token was "+
				"REJECTED -- the suffix must not participate in the credit comparison: %v", err)
		}
	})

	t.Run("a reordered credit still accepts", func(t *testing.T) {
		// The other half of the subset rule, also previously unpinned.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song feat Alpha Beta"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song feat Beta Alpha"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a reordered credit was REJECTED: %v", err)
		}
	})

	t.Run("every featuring marker spelling is honored", func(t *testing.T) {
		// 891-R1V3: the existing `featuring` row passed even with `featuring`
		// REMOVED from featMarkers, because the pair then rejected on the
		// ordinary unmatched-content rule instead -- a different mechanism, so
		// the row pinned nothing. These use a BARE-vs-credited shape, where an
		// unrecognized marker ACCEPTS (the content rule cannot reject it), so
		// only real marker recognition produces the expected verdict.
		for _, marker := range []string{"feat", "feats", "ft", "featuring"} {
			requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title"}
			c := SearchCandidate{
				VideoID: "vid", Artist: artist,
				Title: "Placeholder Song Title " + marker + " Placeholder Guest",
			}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
				t.Errorf("marker %q was not recognized: a one-sided credit using it was "+
					"REJECTED, so the truncation never fired: %v", marker, err)
			}
		}
	})

	t.Run("a one-sided credit is still packaging", func(t *testing.T) {
		// The false-reject guard, and the case the truncation was WRITTEN for:
		// same song, credit added on one side. This must survive the fix or it
		// trades a corruption bug for a provider that returns nothing.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Title (feat Guest)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a one-sided guest credit was REJECTED -- the credit discriminator "+
				"must fire only when BOTH sides are credited and disagree: %v", err)
		}
	})

	t.Run("identical credits on both sides still accept", func(t *testing.T) {
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title feat Alpha"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Title (feat Alpha)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("matching guest credits were REJECTED: %v", err)
		}
	})

	t.Run("a one-sided year is still packaging", func(t *testing.T) {
		// The false-reject guard on the C1 fix: the remaster shape the year
		// rule was written for must survive it.
		requested := models.Track{ArtistName: artist, TrackName: "Placeholder Song Title"}
		c := SearchCandidate{VideoID: "vid", Artist: artist, Title: "Placeholder Song Title (2011 Remaster)"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a remaster was REJECTED -- the year discriminator must fire only "+
				"when BOTH sides are dated and disagree, or it trades a corruption bug "+
				"for a provider that returns nothing: %v", err)
		}
	})
}

// TestLegitimateVariantsStayAccepted is the MANDATORY regression guard on the
// F1 fix, and it is the larger half of the requirement. Most of what this gate
// accepts is CORRECT: a live version, a remaster, a radio edit, an acoustic
// take, a deluxe reissue, a karaoke or instrumental cut all carry the SAME
// WORDS with the same timing, so the lyric is right and must still be returned.
//
// A fix that rejected these would trade a silent-corruption bug for a feature
// that never returns a lyric -- which is why every class the review measured as
// correctly accepted is pinned here explicitly rather than assumed.
func TestLegitimateVariantsStayAccepted(t *testing.T) {
	const artist = "Placeholder Artist Name"
	const title = "Placeholder Song Title"
	requested := models.Track{ArtistName: artist, TrackName: title}

	// reqTi overrides the requested title for cases where the VARIANCE UNDER
	// TEST is on the requested side (an apostrophe has to differ in RENDERING
	// between the two sides to be tested at all; comparing a string to itself
	// would assert nothing).
	tests := []struct {
		name                  string
		artist, reqTi, candTi string
	}{
		{name: "live version", artist: artist, candTi: title + " (Live)"},
		{name: "remaster", artist: artist, candTi: title + " - Remastered"},
		{name: "remaster with year", artist: artist, candTi: title + " (2011 Remaster)"},
		{name: "radio edit", artist: artist, candTi: title + " (Radio Edit)"},
		{name: "acoustic", artist: artist, candTi: title + " (Acoustic Version)"},
		{name: "deluxe edition", artist: artist, candTi: title + " (Deluxe Edition)"},
		{name: "instrumental", artist: artist, candTi: title + " (Instrumental)"},
		{name: "karaoke", artist: artist, candTi: title + " (Karaoke Version)"},
		{name: "mono", artist: artist, candTi: title + " (Mono)"},
		{name: "sped up", artist: artist, candTi: title + " (Sped Up)"},
		{name: "slowed down", artist: artist, candTi: title + " (Slowed Down)"},
		{name: "extended mix", artist: artist, candTi: title + " (Extended Mix)"},
		{name: "feat suffix on the title", artist: artist, candTi: title + " (feat. Placeholder Guest)"},
		{name: "leading article on the title", artist: artist, candTi: "The " + title},
		{name: "ampersand vs and", artist: "Placeholder & Artist Name", candTi: title},
		{name: "and vs ampersand", artist: "Placeholder and Artist Name", candTi: title},
		{name: "diacritics", artist: "Plácehölder Artist Name", candTi: "Plácehölder Söng Title"},
		{name: "typographic vs straight apostrophe", artist: artist, reqTi: "Placeholder's Song Title", candTi: "Placeholder\u2019s Song Title"},
		{name: "case and whitespace", artist: "  placeholder   ARTIST name ", candTi: "  PLACEHOLDER song   TITLE  "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := requested
			if tc.reqTi != "" {
				req.TrackName = tc.reqTi
			}
			c := SearchCandidate{VideoID: "vid", Artist: tc.artist, Title: tc.candTi}
			if _, err := SelectCandidate([]SearchCandidate{c}, req); err != nil {
				t.Errorf("a LEGITIMATE variant was rejected -- this is the failure mode that makes the provider return nothing: %v", err)
			}
		})
	}
}

// TestVariantVocabularyEdgeCases pins the deliberate boundaries of the variant
// vocabulary, so a later widening of it has to break a test rather than a
// library.
func TestVariantVocabularyEdgeCases(t *testing.T) {
	t.Run("a variant token in the REQUESTED title is tolerated symmetrically", func(t *testing.T) {
		// Asking for the remaster and being handed the plain cut is the same
		// words, so direction must not matter.
		requested := models.Track{ArtistName: "Placeholder Artist Name", TrackName: "Placeholder Song Title (Remastered)"}
		c := SearchCandidate{VideoID: "vid", Artist: "Placeholder Artist Name", Title: "Placeholder Song Title"}
		if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
			t.Errorf("a variant token on the REQUESTED side must be tolerated the same way: %v", err)
		}
	})

	t.Run("two titles made entirely of vocabulary words do not correspond", func(t *testing.T) {
		// Without the shared-content-token requirement these differ only by
		// variant tokens and would be accepted.
		if titleTokensCorrespond("Live", "Karaoke") {
			t.Error("two titles built only from variant vocabulary must not correspond")
		}
	})

	t.Run("an identical all-vocabulary title still corresponds to itself", func(t *testing.T) {
		// The multiset-identity branch has to run BEFORE the shared-content
		// requirement, or a song genuinely titled with a vocabulary word could
		// never match itself.
		if !titleTokensCorrespond("Live", "Live") {
			t.Error("an identical title must correspond to itself regardless of vocabulary membership")
		}
	})

	t.Run("a part number is content, a remaster year is packaging", func(t *testing.T) {
		// yearsAreContent=false is the ordinary case: at most one side carries
		// a year, so a year is the remaster/reissue label the rule exists for.
		if isVariantToken("2", false) {
			t.Error("a bare number must be CONTENT -- treating it as packaging is the measured sibling defect")
		}
		if !isVariantToken("2011", false) {
			t.Error("a four-digit year must be packaging when only one side is dated")
		}
		if isVariantToken("1234", false) {
			t.Error("a four-digit number outside the year range must be CONTENT")
		}
		if isVariantToken("part", false) || isVariantToken("pt", false) {
			t.Error("part markers must never be vocabulary -- a part number names a different song")
		}
	})

	t.Run("a year is CONTENT when both sides are dated differently", func(t *testing.T) {
		// The C1 discriminator. The same token flips meaning with context:
		// "Song (2011 Remaster)" against "Song" is packaging, but
		// "Nocturne 1984" against "Nocturne 2019" is two different works.
		if isVariantToken("2011", true) {
			t.Error("a year must be CONTENT when both titles carry differing years -- " +
				"treating it as packaging accepts a different song by the same artist")
		}
		// Vocabulary membership is unaffected by the flag: only the year branch
		// is context-dependent.
		if !isVariantToken("live", true) || !isVariantToken("remaster", true) {
			t.Error("the variant vocabulary must not be disturbed by the year context")
		}
	})

	t.Run("yearsDiffer fires only when both sides are dated and disagree", func(t *testing.T) {
		cases := []struct {
			name string
			req  map[string]int
			cand map[string]int
			want bool
		}{
			{"neither dated", map[string]int{"song": 1}, map[string]int{"song": 1}, false},
			{"only candidate dated (the remaster shape)", map[string]int{"song": 1}, map[string]int{"song": 1, "2011": 1}, false},
			{"only requested dated", map[string]int{"song": 1, "1999": 1}, map[string]int{"song": 1}, false},
			{"same year both sides", map[string]int{"song": 1, "2011": 1}, map[string]int{"song": 1, "2011": 1}, false},
			{"differing years", map[string]int{"song": 1, "1984": 1}, map[string]int{"song": 1, "2019": 1}, true},
			{"one shared year among several", map[string]int{"song": 1, "1984": 1}, map[string]int{"song": 1, "1984": 1, "2019": 1}, false},
			{"non-year numbers are not years", map[string]int{"song": 1, "2": 1}, map[string]int{"song": 1, "3": 1}, false},
		}
		for _, tc := range cases {
			if got := yearsDiffer(tc.req, tc.cand); got != tc.want {
				t.Errorf("%s: yearsDiffer = %v, want %v", tc.name, got, tc.want)
			}
		}
	})

	t.Run("a title that tokenizes to nothing fails open rather than guessing", func(t *testing.T) {
		// Punctuation-only and credit-only titles leave no token evidence. The
		// floor has already been cleared by the caller at that point, so
		// inventing a rejection here would be a guess.
		if !titleTokensCorrespond("!!!", "Placeholder Song Title") {
			t.Error("no token evidence on one side must fail open")
		}
	})
}

// TestApostropheIsErasedNotSplit is the R2F4 regression test, and it guards a
// FALSE-REJECT class the token rule itself introduced.
//
// A dropped apostrophe is one of the most common differences there is between a
// local tag and an upload title, and the similarity floor handles it correctly
// on its own -- every pair below clears the floor comfortably, which the premise
// assertion pins. What broke them was tokenization: splitting on the apostrophe
// turned a contraction into fragments, so a three-token side compared against a
// one-token side, no fragment was in the vocabulary, and the multiset rule
// rejected in both directions. The token rule may only ever make the gate
// STRICTER THAN THE FLOOR FOR A REASON, and "the apostrophe was typed" is not
// one.
//
// The premise assertion is what makes this test honest: if the floor ever
// stopped admitting these, an accept here would prove nothing about
// tokenization.
func TestApostropheIsErasedNotSplit(t *testing.T) {
	const artist = "Placeholder Artist Name"

	tests := []struct {
		name, reqTi, candTi string
	}{
		{name: "contraction, apostrophe dropped by the candidate", reqTi: "Don't Quartermain the Meridian", candTi: "Dont Quartermain the Meridian"},
		{name: "contraction, apostrophe dropped by the request", reqTi: "Its All Wobblesworth", candTi: "It's All Wobblesworth"},
		{name: "possessive", reqTi: "Zenith's End", candTi: "Zeniths End"},
		{name: "leading contraction", reqTi: "I'm Not Meridian", candTi: "Im Not Meridian"},
		{name: "medial contraction", reqTi: "Can't Stop Kettledrum", candTi: "Cant Stop Kettledrum"},
		{name: "typographic apostrophe against no apostrophe", reqTi: "Zenith’s End", candTi: "Zeniths End"},
		{name: "prime against no apostrophe (853-R3F1)", reqTi: "Zenith′s End", candTi: "Zeniths End"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// PREMISE: the floor already accepts this pair, so a rejection can
			// only come from tokenization -- which is what this test guards.
			if conf := normalize.MatchConfidence(tc.reqTi, tc.candTi); conf < matchMinConfidence {
				t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor, so this pair is no longer isolating the token rule", conf, matchMinConfidence)
			}

			requested := models.Track{ArtistName: artist, TrackName: tc.reqTi}
			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: tc.candTi}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
				t.Errorf("a dropped apostrophe was treated as a different song -- the floor accepts this pair and tokenization must not undo that: %v", err)
			}
		})
	}
}

// TestApostropheErasureClosesNothing is the MANDATORY safe-direction guard on
// the R2F4 fix, and it is the half that matters: erasing a character from
// tokenization can only ever make two titles look MORE alike, so every class
// the sibling rule closes has to be re-proved closed with the erasure in place.
//
// The sharpest case is the last one. A possessive and its apostrophe-less
// spelling now tokenize IDENTICALLY -- that is the fix -- so the ONLY thing
// still separating a possessive title from its sibling WITH a part number is
// the part number itself being CONTENT. If that ever stopped holding, the fix
// would have reopened the exact silent-corruption class the slice exists to
// close, and this is where that shows up.
func TestApostropheErasureClosesNothing(t *testing.T) {
	const artist = "Placeholder Artist Name"

	tests := []struct {
		name, reqTi, candTi string
	}{
		{name: "pluralized sibling", reqTi: "Placeholder Song Title", candTi: "Placeholder Song Titles"},
		{name: "part-number sibling", reqTi: "Placeholder Song Title", candTi: "Placeholder Song Title Pt.2"},
		{name: "roman-numeral sequel", reqTi: "Placeholder Song Title", candTi: "Placeholder Song Title II"},
		{name: "one token is a prefix of the other", reqTi: "Zenith Wobble", candTi: "Zenith Wobblecraft"},
		{name: "one token is a compound of the other", reqTi: "Zenith Kettle", candTi: "Zenith Kettledrum"},
		{name: "shared prefix, different suffix", reqTi: "Zenith Meridianset", candTi: "Zenith Meridianrise"},
		{name: "possessive plus a part number", reqTi: "Wobblesworth's Lament", candTi: "Wobblesworths Lament Pt.2"},
		{name: "possessive plus a bare part number", reqTi: "Wobblesworth's Lament", candTi: "Wobblesworths Lament 2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// PREMISE: above the floor, so the token rule is the only thing
			// that can reject -- and therefore the only thing the apostrophe
			// erasure could have broken.
			if conf := normalize.MatchConfidence(tc.reqTi, tc.candTi); conf < matchMinConfidence {
				t.Fatalf("test premise broken: title confidence %.4f is already below the %.2f floor, so this case no longer exercises the token rule", conf, matchMinConfidence)
			}

			requested := models.Track{ArtistName: artist, TrackName: tc.reqTi}
			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: tc.candTi}
			got, err := SelectCandidate([]SearchCandidate{c}, requested)
			if err == nil {
				t.Fatalf("a sibling title was ACCEPTED (videoID %q) -- this writes another song's words next to the user's audio", got.VideoID)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("error must wrap ErrNotFound, got %v", err)
			}
		})
	}
}

// TestUploadDecorationStaysRejected PINS AN ABSENCE, which is why it exists at
// all: nothing else in the package fails if someone adds "video" to the
// vocabulary.
//
// Every title below rejects because its decoration token is NOT in
// titleVariantTokens. That exclusion is LOAD BEARING, not an unfinished list --
// no rule available here separates a decoration "video" from a content "video",
// and admitting the token makes a title correspond to that title with the word
// prepended, which is the measured sibling defect. The whole upload-decoration
// family is therefore excluded together; a PARTIAL list is the trap, because
// the next reader infers the missing members were an oversight and "completes"
// it, silently flipping every row here to accept.
//
// If you are here because you added one of these tokens and this test went red:
// that is this test working. Read the WHAT THIS DELIBERATELY DOES NOT HANDLE
// block in selection.go before changing it.
func TestUploadDecorationStaysRejected(t *testing.T) {
	const artist = "Placeholder Artist Name"
	const title = "Placeholder Song Title"
	requested := models.Track{ArtistName: artist, TrackName: title}

	decorated := []string{
		title + " (Official Video)",
		title + " (Official Audio)",
		title + " [HD]",
		title + " (Visualizer)",
		title + " (Official)",
		title + " (Lyrics)",
		title + " (Official Lyric Video)",
	}

	for _, candTi := range decorated {
		t.Run(candTi, func(t *testing.T) {
			// PREMISE: the floor admits these, so the token rule is what
			// rejects them and the absence is what the token rule turns on.
			if conf := normalize.MatchConfidence(title, candTi); conf < matchMinConfidence {
				t.Fatalf("test premise broken: title confidence %.4f is below the %.2f floor, so the floor rejects this and the vocabulary absence is not what is being pinned", conf, matchMinConfidence)
			}

			c := SearchCandidate{VideoID: "vid", Artist: artist, Title: candTi}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err == nil {
				t.Errorf("upload decoration was ACCEPTED -- a decoration token was added to titleVariantTokens; that exclusion is load-bearing, not an omission")
			}
		})
	}
}

// TestTokenRuleOnlySubtractsAccepts pins the STRUCTURAL BOUND this slice rests
// on: titleFieldCorresponds returns early unless the similarity floor has
// ALREADY passed, so the token rule can only ever SUBTRACT accepts, never add
// one. Removing that early return left the whole suite green (853-R5F1), which
// means the bound every downstream argument leans on was unguarded.
//
// The bound is what makes the token rule safe to reason about: no input can
// reach it that the floor did not already admit, so the rule cannot invent an
// accept out of a below-floor pair.
func TestTokenRuleOnlySubtractsAccepts(t *testing.T) {
	// Pairs BELOW the floor whose tokens would nonetheless correspond. With
	// the early return, the floor rejects them and the token rule never runs.
	// Without it, the token rule reaches them and flips them to accept.
	for _, tc := range []struct{ name, requested, got string }{
		// A reordering scores ~0.53, far below 0.75, but its token multiset is
		// IDENTICAL -- so this is the sharpest case the bound protects.
		{"reordered tokens score below the floor", "Vanguard Kettledrum", "Kettledrum Vanguard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// PREMISE: below the floor, so only the early return can be what
			// rejects this. If it ever rose above, the case stops testing the
			// bound.
			conf := normalize.MatchConfidence(tc.requested, tc.got)
			if conf >= matchMinConfidence {
				t.Fatalf("test premise broken: confidence %.4f is at or above the %.2f floor, so this pair no longer isolates the early return", conf, matchMinConfidence)
			}
			// And the tokens DO correspond, so the token rule would accept.
			if !titleTokensCorrespond(tc.requested, tc.got) {
				t.Fatalf("test premise broken: the tokens must correspond, or removing the early return would change nothing here")
			}

			ok, comparable := titleFieldCorresponds(tc.requested, tc.got)
			if !comparable {
				t.Fatal("both sides carry tokens, so the field must be comparable")
			}
			if ok {
				t.Error("a below-floor pair was ACCEPTED: the token rule may only SUBTRACT accepts, never add one")
			}
		})
	}
}

// TestTitleTokensAreAMultisetNotASet pins duplicate COUNTING. Replacing the
// count comparisons with presence-only checks left the suite green
// (853-R5F2), so the word "multiset" was load-bearing in the design comments
// and untested in the code. Repeated-word titles are a real shape, and a set
// makes a doubled title correspond to its single-word sibling.
func TestTitleTokensAreAMultisetNotASet(t *testing.T) {
	for _, tc := range []struct{ name, requested, got string }{
		{"doubled word against single", "Wobble Wobble Kettledrum", "Wobble Kettledrum"},
		{"single against doubled word", "Wobble Kettledrum", "Wobble Wobble Kettledrum"},
		{"tripled against doubled", "Wobble Wobble Wobble Kettle", "Wobble Wobble Kettle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if titleTokensCorrespond(tc.requested, tc.got) {
				t.Errorf("%q and %q repeat a token a different number of times and are different titles; a SET comparison would wrongly accept them", tc.requested, tc.got)
			}
		})
	}

	t.Run("identical multiplicities still correspond", func(t *testing.T) {
		// The multiset must not over-reject: the same counts are the same title.
		if !titleTokensCorrespond("Wobble Wobble Kettle", "Wobble Wobble Kettle") {
			t.Error("identical multiplicities name the same title")
		}
	})
}

// TestArtistReorderIsAccepted is the F2 regression test. Jaro-Winkler is an
// ordered-string measure, so a legitimate REORDERING of the same artist tokens
// reads as a large edit: a featured-credit swap measured 0.6736 and a trailing
// article 0.7076, BOTH below the 0.75 floor, while a genuine wrong-track artist
// reached 0.8788. That inversion is the defect -- the floor was rejecting
// correct artists more confidently than it rejected wrong ones.
func TestArtistReorderIsAccepted(t *testing.T) {
	const title = "Placeholder Song Title"

	tests := []struct {
		name        string
		reqA, candA string
	}{
		{
			name:  "featured-credit ordering swap",
			reqA:  "Placeholder Artist feat. Zenith Guest Performer",
			candA: "Zenith Guest Performer feat. Placeholder Artist",
		},
		{
			// 0.7052, just under the floor -- the same shape and nearly the
			// same value as the 0.7076 the review measured. A longer name in
			// this form drifts back ABOVE the floor (Jaro-Winkler's prefix
			// bonus and the shared middle carry it), which is exactly why the
			// pair is pinned by its measured premise below rather than assumed.
			name:  "trailing-article form",
			reqA:  "The Wobblesworth",
			candA: "Wobblesworth, The",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Assert the PREMISE: these are only meaningful while the pair
			// scores BELOW the floor. If the floor started admitting them the
			// token-multiset rule would no longer be what rescues them.
			if conf := normalize.MatchConfidence(tc.reqA, tc.candA); conf >= matchMinConfidence {
				t.Fatalf("test premise broken: artist confidence %.4f already clears the %.2f floor, so this case no longer exercises the token-multiset rule", conf, matchMinConfidence)
			}

			requested := models.Track{ArtistName: tc.reqA, TrackName: title}
			c := SearchCandidate{VideoID: "vid", Artist: tc.candA, Title: title}
			if _, err := SelectCandidate([]SearchCandidate{c}, requested); err != nil {
				t.Errorf("a legitimate artist reordering was REJECTED: %v", err)
			}
		})
	}
}

// TestDifferentArtistsSharingATokenAreRejected is the safe-direction guard on
// the F2 widening. A token-SET comparison must require FULL equality: accepting
// on mere OVERLAP would make any two acts sharing one common word correspond,
// a far larger false-accept class than the false rejects F2 removes.
func TestDifferentArtistsSharingATokenAreRejected(t *testing.T) {
	const title = "Placeholder Song Title"

	// END-TO-END: pairs that sit BELOW the floor, so the token-multiset rule is the
	// only thing that could admit them. These are the cases the widening
	// actually governs, and each must still be rejected.
	tests := []struct {
		name        string
		reqA, candA string
	}{
		{name: "shared token, candidate names more acts", reqA: "Zenith Wobblesworth", candA: "Zenith Kettledrum Quartermain Ensemble"},
		{name: "shared token in a different position", reqA: "Quartermain Vanguard", candA: "Vanguard Wobblesworth Kettledrum Zenith"},
		{name: "wholly unrelated", reqA: "Zenith Quartermain", candA: "Wobblesworth Kettledrum"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Assert the premise: below the floor, so an accept could only
			// come from the token-multiset rule this test is guarding.
			if conf := normalize.MatchConfidence(tc.reqA, tc.candA); conf >= matchMinConfidence {
				t.Fatalf("test premise broken: artist confidence %.4f clears the %.2f floor, so this case no longer isolates the token-multiset rule", conf, matchMinConfidence)
			}

			requested := models.Track{ArtistName: tc.reqA, TrackName: title}
			c := SearchCandidate{VideoID: "vid", Artist: tc.candA, Title: title}
			got, err := SelectCandidate([]SearchCandidate{c}, requested)
			if err == nil {
				t.Fatalf("two different artists were treated as corresponding (videoID %q)", got.VideoID)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("error must wrap ErrNotFound, got %v", err)
			}
		})
	}

	// UNIT-LEVEL: pairs the FLOOR ALREADY ADMITS on its own (all score above
	// 0.75, a strict superset reaching 0.9286). Those accepts are PRE-EXISTING
	// behavior of the similarity floor, not a consequence of the token-multiset
	// widening, and this fix does not claim to close them -- doing so would
	// mean tightening the artist gate, which is the opposite of what F2 asks
	// for and would re-break the reorderings above.
	//
	// What IS asserted here is that the widening contributes NOTHING to them:
	// the token-multiset rule reports false for every one, so the OR it forms with
	// the floor adds no accept that the floor was not already making. That is
	// the precise claim this fix can honestly support.
	//
	// THAT IS NOT THE SAME AS "NO NEW ACCEPTS AT ALL", and the difference
	// belongs here so nobody reads the stronger claim out of the weaker one.
	// The widening introduces exactly one new accept class: two acts that are
	// TOKEN PERMUTATIONS of each other, which pass at floor=false, set=true.
	// That is the intended mechanism -- it is precisely what rescues the
	// credited-order swap in TestArtistReorderIsAccepted -- and it is rare,
	// because two DIFFERENT acts named by the same multiset of words in a
	// different order is a coincidence, where a reordering of the SAME act is
	// routine. The overlap cases below are the ones that stay closed; a
	// permutation is the one that opens.
	t.Run("the token-multiset rule itself never accepts on mere overlap", func(t *testing.T) {
		overlapping := []struct{ name, a, b string }{
			{"shared surname only", "Zenith Quartermain", "Wendell Quartermain"},
			{"shared leading word", "Placeholder Vanguard", "Placeholder Meridian"},
			{"candidate is a strict superset", "Zenith Quartermain", "Zenith Quartermain Orchestra"},
			{"candidate is a strict subset", "Zenith Quartermain Orchestra", "Zenith Quartermain"},
			{"same token count, one token differs", "Quartermain Zenith Ensemble", "Quartermain Wobblesworth Ensemble"},
		}
		for _, tc := range overlapping {
			if artistTokensEqual(tc.a, tc.b) {
				t.Errorf("%s: token multisets must NOT be equal -- accepting on overlap would make any two acts sharing a word correspond", tc.name)
			}
		}
	})

	t.Run("a reordering of the SAME tokens is equal", func(t *testing.T) {
		if !artistTokensEqual("Wobblesworth & Kettledrum", "Kettledrum & Wobblesworth") {
			t.Error("a pure reordering names the same act and must compare equal")
		}
	})

	// MULTIPLICITY IS LOAD-BEARING (853-R5F1). These rows are the ones a SET
	// comparison gets wrong: it collapses duplicates, so a doubled-word act
	// and its single-word namesake compare equal and a lyric from the wrong
	// act can be written. A set passes every other test in this file, so
	// without these rows the set-vs-multiset choice is unpinned and the
	// regression is silent.
	t.Run("a repeated token is not the same act as a single one", func(t *testing.T) {
		differing := []struct{ name, a, b string }{
			{"doubled name vs its namesake", "Wobble Wobble Kettle", "Kettle Wobble"},
			{"doubled name in a collaboration", "Wobble Wobble & Kettledrum", "Kettledrum & Wobble"},
			{"repeat on the candidate side", "Kettle Wobble", "Wobble Wobble Kettle"},
			{"three tokens, one repeated", "Zenith Zenith Quartermain Vanguard", "Vanguard Quartermain Zenith"},
		}
		for _, tc := range differing {
			if artistTokensEqual(tc.a, tc.b) {
				t.Errorf("%s: %q and %q repeat a token differently and name different acts; a SET comparison would wrongly accept them", tc.name, tc.a, tc.b)
			}
		}
	})

	t.Run("a repeated token still matches its own reordering", func(t *testing.T) {
		// The multiset must not over-reject: the same multiplicities in a
		// different order are still the same act.
		if !artistTokensEqual("Wobble Wobble Kettle", "Kettle Wobble Wobble") {
			t.Error("identical multiplicities in a different order name the same act")
		}
	})

	// The featMarkers and ignorableTokens skips inside the tokenizer had no
	// test: every existing case carried the token on BOTH sides, so it
	// canceled and deleting the skip changed nothing (853-R5F3). These put
	// the token on ONE side only, which is the shape the skip exists for.
	t.Run("a marker present on only one side is skipped", func(t *testing.T) {
		oneSided := []struct{ name, a, b string }{
			{"feat marker only on the candidate", "Zenith Quartermain", "Zenith feat. Quartermain"},
			{"ignorable article only on the request", "The Kettledrum Vanguard", "Kettledrum Vanguard"},
		}
		for _, tc := range oneSided {
			if !artistTokensEqual(tc.a, tc.b) {
				t.Errorf("%s: %q and %q name the same act; the dropped token must not break equality", tc.name, tc.a, tc.b)
			}
		}
	})

	// The empty-multiset guard is reachable: a value can survive NormalizeKey
	// and still tokenize to nothing. Without the guard two such fields compare
	// equal and accept at 0.0 confidence.
	t.Run("a field that tokenizes to nothing never compares equal", func(t *testing.T) {
		for _, tc := range [][2]string{{"&", "!!!"}, {"...", "&&&"}, {"&", "&"}} {
			if artistTokensEqual(tc[0], tc[1]) {
				t.Errorf("%q vs %q: neither carries a token, so there is no evidence to accept on", tc[0], tc[1])
			}
		}
	})
}
