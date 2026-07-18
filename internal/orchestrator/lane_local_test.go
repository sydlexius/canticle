package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
)

// newLocalTestLane builds a lane that resolves without an outbound provider
// request, standing in for the detector lane. It returns a suitable synced
// result so it can win a dispatch in either mode.
func newLocalTestLane(song models.Song) *Lane {
	return &Lane{
		name:        "detector",
		breaker:     circuit.New(60*time.Second, 30*time.Minute),
		classifyErr: func(_ *Lane, err error) error { return err },
		local:       true,
		resolve: func(context.Context, models.Track, string) (models.Song, error) {
			return song, nil
		},
	}
}

func attemptFor(t *testing.T, song models.Song, lane string) models.LaneAttempt {
	t.Helper()
	for _, a := range song.LaneAttempts {
		if a.Lane == lane {
			return a
		}
	}
	t.Fatalf("lane %q not present in attribution %+v", lane, song.LaneAttempts)
	return models.LaneAttempt{}
}

// TestLaneLocalDefaultsToRemote pins the fail-safe default: a lane is remote
// unless it explicitly opts in, so a newly added lane cannot accidentally
// suppress the worker's provider pacing (#534).
func TestLaneLocalDefaultsToRemote(t *testing.T) {
	l, _ := newTestLane(&stubProvider{name: "musixmatch"})
	if l.Local() {
		t.Fatal("provider lane reports Local() = true; a lane must be remote unless it opts in")
	}
	if !newLocalTestLane(syncedSong()).Local() {
		t.Fatal("local lane reports Local() = false")
	}
}

// TestOrderedDispatchPropagatesLaneLocality verifies the ordered path carries
// each lane's locality onto the emitted attribution, which is the signal the
// worker uses to decide whether an item spent the provider pacing budget.
func TestOrderedDispatchPropagatesLaneLocality(t *testing.T) {
	local := newLocalTestLane(syncedSong())
	o, err := New(ModeOrdered, local)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	song, err := o.FindLyrics(context.Background(), models.Track{}, "/audio.flac")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.LaneAttempts) != 1 {
		t.Fatalf("attempts = %+v; want exactly the local lane", song.LaneAttempts)
	}
	if a := attemptFor(t, song, "detector"); !a.Local || !a.Hit {
		t.Fatalf("detector attempt = %+v; want Local and Hit true", a)
	}
}

// TestOrderedDispatchMarksProviderLaneRemote is the other half: a provider that
// serves the track must be attributed as remote, so the worker still paces.
func TestOrderedDispatchMarksProviderLaneRemote(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: syncedSong()}
	lane, _ := newTestLane(p)
	o, err := New(ModeOrdered, lane)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	song, err := o.FindLyrics(context.Background(), models.Track{}, "")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if a := attemptFor(t, song, "musixmatch"); a.Local {
		t.Fatalf("provider attempt = %+v; want Local false", a)
	}
}

// TestParallelDispatchPropagatesLaneLocality covers the parallel path, which
// builds its attribution from a separate laneResult channel rather than the
// ordered loop. A regression there would silently report a provider lane as
// local and skip pacing.
//
// Note the worker does not install a local lane under parallel dispatch today
// (see rebuildOrchestrator, and #528), so this exercises the orchestrator's
// contract directly rather than a currently-reachable worker configuration.
func TestParallelDispatchPropagatesLaneLocality(t *testing.T) {
	local := newLocalTestLane(syncedSong())
	o, err := New(ModeParallel, local)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	song, err := o.FindLyrics(context.Background(), models.Track{}, "/audio.flac")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if a := attemptFor(t, song, "detector"); !a.Local {
		t.Fatalf("detector attempt = %+v; want Local true under parallel dispatch", a)
	}
}

// TestParallelDispatchAttributesEveryReportingLaneRemote guards the mixed case
// CodeRabbit raised on #541: when a provider lane reports alongside a local
// lane, the provider must appear in the attribution so the worker still paces.
func TestParallelDispatchAttributesEveryReportingLaneRemote(t *testing.T) {
	p := &stubProvider{name: "musixmatch", song: syncedSong()}
	provider, _ := newTestLane(p)
	local := newLocalTestLane(syncedSong())
	o, err := New(ModeParallel, provider, local)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	song, err := o.FindLyrics(context.Background(), models.Track{}, "/audio.flac")
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	// The race is nondeterministic, so assert the invariant rather than a
	// specific winner: every lane that reported carries its own locality, and a
	// reporting provider lane is never marked local.
	for _, a := range song.LaneAttempts {
		if a.Lane == "musixmatch" && a.Local {
			t.Fatalf("provider lane attributed as local: %+v", song.LaneAttempts)
		}
		if a.Lane == "detector" && !a.Local {
			t.Fatalf("local lane attributed as remote: %+v", song.LaneAttempts)
		}
	}
}
