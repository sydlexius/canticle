package orchestrator

import (
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// wordSyncedSong is syncedSong plus per-word timings -- the only difference the
// orchestrator can see between the two tiers, since the line cues are identical.
func wordSyncedSong() models.Song {
	s := syncedSong()
	s.WordTimings = []models.WordTiming{
		{Line: 0, Text: "la", StartMS: 1000, EndMS: 1200},
		{Line: 0, Text: "la", StartMS: 1200, EndMS: 1400},
	}
	return s
}

func TestQualityOf_WordSyncedOutranksSynced(t *testing.T) {
	ws, ls := QualityOf(wordSyncedSong()), QualityOf(syncedSong())
	if ws != QualityWordSynced {
		t.Errorf("word-synced song classified as %d, want %d", ws, QualityWordSynced)
	}
	if ls != QualitySynced {
		t.Errorf("line-synced song classified as %d, want %d", ls, QualitySynced)
	}
	if ws <= ls {
		t.Errorf("word-sync must outrank line-sync: %d vs %d", ws, ls)
	}
}

// TestQualityOf_WordTimingsWithoutCuesIsNotWordSynced: timings index into
// Subtitles.Lines, so they are meaningless without cues and must not promote a
// song on their own.
func TestQualityOf_WordTimingsWithoutCuesIsNotWordSynced(t *testing.T) {
	orphan := unsyncedSong()
	orphan.WordTimings = []models.WordTiming{{Line: 0, Text: "la", StartMS: 0, EndMS: 100}}
	if got := QualityOf(orphan); got != QualityUnsynced {
		t.Errorf("orphan word timings should not promote past unsynced, got %d", got)
	}
}

// TestQualityOf_PartialWordCoverageStillRanksAbove pins the deliberate ranking
// rule at its BOUNDARY: ANY word timings promote the result, even when most
// lines carry none.
//
// The fixture must have a genuinely untimed line for this to test anything. An
// earlier version reused syncedSong() -- which has exactly ONE cue -- plus one
// timing, i.e. 100% coverage, so it was indistinguishable from the full-coverage
// case and would have passed under any "promote if len(WordTimings) > 0"
// implementation. Here 1 of 4 cues is timed: unambiguously partial.
//
// Rationale for promoting anyway: a partially timed result still carries every
// line cue a line-synced one would, so preferring it can never produce worse
// output. Coverage is uneven in the wild -- across 54 word-synced tracks
// measured 2026-07-21, the median had 100% of words distinctly timed and the
// worst 51%.
//
// This is a RANKING rule only. The higher bar for terminal-ness -- where marking
// a half-timed result "done" would permanently exclude it from upgrade -- is
// #553's call, not QualityOf's.
func TestQualityOf_PartialWordCoverageStillRanksAbove(t *testing.T) {
	partial := models.Song{Subtitles: models.Synced{Lines: []models.Lines{
		{Text: "one", Time: models.Time{Total: 1}},
		{Text: "two", Time: models.Time{Total: 2}},
		{Text: "three", Time: models.Time{Total: 3}},
		{Text: "four", Time: models.Time{Total: 4}},
	}}}
	// Only the first of four cues carries word timings.
	partial.WordTimings = []models.WordTiming{{Line: 0, Text: "one", StartMS: 1000, EndMS: 1200}}

	timedLines := map[int]bool{}
	for _, w := range partial.WordTimings {
		timedLines[w.Line] = true
	}
	if len(timedLines) >= len(partial.Subtitles.Lines) {
		t.Fatalf("fixture is not partial: %d of %d cues timed", len(timedLines), len(partial.Subtitles.Lines))
	}

	if got := QualityOf(partial); got != QualityWordSynced {
		t.Errorf("partial word coverage should still rank as word-synced, got %d", got)
	}
}

// TestIsSuitable_UnchangedByWordSync guards the scope boundary of #603: this
// change reorders results, it does not change when a lane short-circuits.
func TestIsSuitable_UnchangedByWordSync(t *testing.T) {
	cases := []struct {
		name string
		song models.Song
		want bool
	}{
		{"word-synced is suitable", wordSyncedSong(), true},
		{"line-synced is suitable", syncedSong(), true},
		{"unsynced is suitable", unsyncedSong(), true},
		{"provider instrumental is not suitable alone", instrumentalSong(), false},
		{"empty is not suitable", models.Song{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSuitable(tc.song, nil); got != tc.want {
				t.Errorf("IsSuitable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRetain_PrefersWordSynced exercises the actual consumer: the
// best-available fallback path must prefer a word-synced result over a
// line-synced one regardless of which lane reported first.
func TestRetain_PrefersWordSynced(t *testing.T) {
	t.Run("word-sync arrives second", func(t *testing.T) {
		var r dispatchResult
		r.retain(syncedSong(), "line-lane")
		r.retain(wordSyncedSong(), "word-lane")
		if r.bestLane != "word-lane" {
			t.Errorf("best lane = %q, want word-lane", r.bestLane)
		}
	})
	t.Run("word-sync arrives first and is not displaced", func(t *testing.T) {
		var r dispatchResult
		r.retain(wordSyncedSong(), "word-lane")
		r.retain(syncedSong(), "line-lane")
		if r.bestLane != "word-lane" {
			t.Errorf("best lane = %q, want word-lane", r.bestLane)
		}
	})
}
