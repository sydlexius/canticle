package instrumentalrecalib

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
)

// An UNKNOWN current version must never trigger the destructive branch.
//
// CurrentVersion is resolved from the sidecar's reported model version (#684),
// so it is empty whenever that cannot be determined: an old sidecar, a sidecar
// that is down, or ffmpeg missing so the detector cannot even be constructed.
// Comparing a real stored version against "" makes EVERY row look mismatched,
// and the mismatch branch calls ResetInstrumentalToUnclassified -- discarding
// stored telemetry library-wide and forcing a full re-inference. That is the
// exact I/O storm #684 exists to eliminate, triggered by a transient probe
// failure.
//
// Unknown must mean "I cannot tell, so change nothing", never "reset everything".
func TestRun_UnknownCurrentVersionNeverResets(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/cello.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "1.17.0",
	})

	w := &fakeWriter{}
	res, err := New(q, w).Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20,
		CurrentVersion: "", // unknown: sidecar unreachable or too old to report one
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.ResetStale != 0 {
		t.Fatalf("ResetStale = %d; want 0. An unknown current version must not reset any row -- "+
			"that discards telemetry library-wide and forces the full re-inference #684 exists to prevent", res.ResetStale)
	}

	// It SETTLES instead: the row already passed the current thresholds above, so
	// the marker the gate says is correct gets written. That is the safe
	// direction -- the destructive branch discards telemetry, this one does not.
	if res.Settled != 1 {
		t.Fatalf("Settled = %d; want 1. With the version unknown the row should settle from its "+
			"cached scores, not be discarded", res.Settled)
	}
	if w.calls != 1 {
		t.Fatalf("writer calls = %d; want 1 (the settle writes the instrumental marker)", w.calls)
	}
}

// The destructive branch must still fire on a REAL mismatch: the guard above
// exempts only an UNKNOWN version, and must not have disabled reset entirely.
// Without this, "never reset" would pass trivially by never resetting anything.
func TestRun_KnownMismatchStillResets(t *testing.T) {
	ctx := context.Background()
	q := openTestQueue(t)

	seedRejection(t, q, "/music/cello.flac", queue.InstrumentalTelemetry{
		MusicSum: 0.97, VocalPeak: 0.04, SpeechMean: 0.001, VocalClass: "Singing", DetectorVersion: "old-model-sha",
	})

	res, err := New(q, &fakeWriter{}).Run(ctx, Options{
		DryRun: false, MinConfidence: 0.90, VocalMax: 0.30, SpeechMax: 0.20,
		CurrentVersion: "new-model-sha", // known, and genuinely different
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ResetStale != 1 {
		t.Fatalf("ResetStale = %d; want 1. A row whose model really did change must still reset", res.ResetStale)
	}
}
