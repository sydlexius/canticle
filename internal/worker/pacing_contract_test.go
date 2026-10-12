package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/orchestrator"
)

// The provider-request and pacing cost of a row the audio detector settles
// (#568), per dispatch mode and detector ordering. This pins behavior the code
// already has; it changes none. Every artist and title is invented.
//
// The pacing observable is the pause the run loop takes after a processed item:
// the loop takes it only when the item contacted a provider (#534), so a count
// of pauses is a count of provider pacing waits owed.
// A row nobody settles keeps the loop's fail-safe default and pauses too.

type pacingRowKind int

const (
	// settledByDetector: the detector says instrumental, every provider misses.
	settledByDetector pacingRowKind = iota
	// providerHasLyric: the detector says not instrumental, a provider has words.
	providerHasLyric
	// everyoneMisses: the detector says not instrumental, every provider misses.
	everyoneMisses
)

// settledRow is the row's "status|provider_lane" once the pass is over: what
// the cost above bought.
func (k pacingRowKind) settledRow() string {
	return [...]string{"done|detector", "done|musixmatch", "deferred|null"}[k]
}

func (k pacingRowKind) String() string {
	return [...]string{"detector-settled", "provider-has-lyric", "every-provider-misses"}[k]
}

type pacingCost struct{ providerRequests, detectorCalls, pauses int }

func TestPacingContract(t *testing.T) {
	// One entry per cell and row kind. providerRequests counts every
	// FindLyrics call on both providers. See the notes on the cells that depart
	// from the plain "one detector call per row" reading.
	cases := []struct {
		mode     string
		ordering string
		kind     pacingRowKind
		want     pacingCost
	}{
		// ordered + demoted: the detector is the last lane, so a provider lyric
		// ends the dispatch before the detector is ever asked (detectorCalls 0).
		{orchestrator.ModeOrdered, "demoted", settledByDetector, pacingCost{2, 1, 1}},
		{orchestrator.ModeOrdered, "demoted", providerHasLyric, pacingCost{1, 0, 1}},
		{orchestrator.ModeOrdered, "demoted", everyoneMisses, pacingCost{2, 1, 1}},
		// ordered + front: a detector-settled row asks no provider and owes no wait.
		{orchestrator.ModeOrdered, "front", settledByDetector, pacingCost{0, 1, 0}},
		{orchestrator.ModeOrdered, "front", providerHasLyric, pacingCost{1, 1, 1}},
		{orchestrator.ModeOrdered, "front", everyoneMisses, pacingCost{2, 1, 1}},
		// parallel + demoted: every provider is raced, then the detector is staged
		// after them, and only when no provider lyric committed.
		{orchestrator.ModeParallel, "demoted", settledByDetector, pacingCost{2, 1, 1}},
		{orchestrator.ModeParallel, "demoted", providerHasLyric, pacingCost{2, 0, 1}},
		{orchestrator.ModeParallel, "demoted", everyoneMisses, pacingCost{2, 1, 1}},
		// parallel + front: the detector runs in the pre stage, before any race.
		{orchestrator.ModeParallel, "front", settledByDetector, pacingCost{0, 1, 0}},
		{orchestrator.ModeParallel, "front", providerHasLyric, pacingCost{2, 1, 1}},
		{orchestrator.ModeParallel, "front", everyoneMisses, pacingCost{2, 1, 1}},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%s/%s/%s", tc.mode, tc.ordering, tc.kind)
		t.Run(name, func(t *testing.T) {
			r := newTitleRig(t, models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Paper Kites"})
			det := &fakeDetector{instrumental: tc.kind == settledByDetector, version: "9.9.9"}
			r.w.EnableAudioDetector(det)
			r.w.SetInstrumentalDetectionDefault(true)
			r.w.SetProvidersMode(tc.mode)
			r.w.SetDetectorOrdering(tc.ordering)
			if tc.kind != providerHasLyric {
				r.primary.err = musixmatch.ErrNoLyrics
			}
			if _, err := r.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
				t.Fatal(err)
			}
			pauses := 0
			if err := r.w.run(context.Background(), func(context.Context) error { pauses++; return nil }); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got, want := r.cols(t, "status", "provider_lane"), tc.kind.settledRow(); got != want {
				t.Errorf("mode=%s ordering=%s row=%s: row = %q; want %q", tc.mode, tc.ordering, tc.kind, got, want)
			}
			got := pacingCost{r.fetches(), len(det.calls), pauses}
			if got != tc.want {
				t.Errorf("mode=%s ordering=%s row=%s: provider requests, detector calls, pacing waits = %d, %d, %d; want %d, %d, %d",
					tc.mode, tc.ordering, tc.kind, got.providerRequests, got.detectorCalls, got.pauses,
					tc.want.providerRequests, tc.want.detectorCalls, tc.want.pauses)
			}
		})
	}
}
