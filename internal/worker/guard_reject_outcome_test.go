package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// rejectAllGuard refuses every song, standing in for a configured language
// allowlist that the fetched lyric's script does not satisfy.
type rejectAllGuard struct{ reason string }

func (g rejectAllGuard) Accept(models.Song) (bool, string) { return false, g.reason }
func (g rejectAllGuard) Enabled() bool                     { return true }

// acceptAllGuard is the CONTROL. Without it, "the rejected row carries
// outcome_type=rejected" would be consistent with the worker stamping that value
// on every row regardless of the guard verdict.
type acceptAllGuard struct{}

func (acceptAllGuard) Accept(models.Song) (bool, string) { return true, "" }
func (acceptAllGuard) Enabled() bool                     { return true }

// TestGuardRejectedRowRecordsItsOutcome is the reproducing test for #655.
//
// The defect: the language-guard rejection path completed the row without
// stamping anything, so it landed `done` with a NULL outcome_type. Reports
// render NULL as "unknown", which is also how a legacy row predating the column
// renders -- so one value meant two unrelated things and an operator could not
// tell a deliberate policy rejection from missing history. 3,182 rows sat that
// way on production.
//
// It shipped because nothing tested this path at all: no test in this package
// referenced guardReject or a script guard before this file.
//
// Verified to fail against unfixed code, not merely written alongside the fix:
// with the SetOutcomeType call removed from the guard branch, the first subtest
// reds with "settled with NO outcome_type recorded".
func TestGuardRejectedRowRecordsItsOutcome(t *testing.T) {
	newRejectedRow := func(t *testing.T, guard ScriptGuard) *fakeQueue {
		t.Helper()
		q := &fakeQueue{items: []queue.WorkItem{{
			ID:     1,
			Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "One"}, Outdir: "o", Filename: "a.lrc"},
		}}}
		fetcher := &seqFetcher{results: []seqResult{{
			song: models.Song{
				Track:     models.Track{ArtistName: "A", TrackName: "One"},
				Subtitles: models.Synced{Lines: []models.Lines{{Text: "x"}}},
			},
		}}}
		w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
		w.EnableGuard(guard)
		if err := w.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return q
	}

	t.Run("rejected row is completed AND labeled", func(t *testing.T) {
		q := newRejectedRow(t, rejectAllGuard{reason: "script not in allowlist"})

		// The row must still settle terminally: a guard rejection is policy, not a
		// retriable failure, so re-fetching would return the same wrong-script
		// lyric forever.
		if len(q.completed) != 1 || q.completed[0] != 1 {
			t.Fatalf("completed = %v; want the guard-rejected row settled exactly once", q.completed)
		}
		if len(q.failed) != 0 || len(q.deferred) != 0 {
			t.Errorf("guard rejection must not fail or defer the row; failed=%v deferred=%v", q.failed, q.deferred)
		}

		// THE REGRESSION. Before the fix this map was empty for the row.
		got, ok := q.outcomeTypes[1]
		if !ok {
			t.Fatal("guard-rejected row settled with NO outcome_type recorded; " +
				"reports cannot distinguish it from a legacy row (#655)")
		}
		if got != outcomeTypeRejected {
			t.Errorf("outcome_type = %q; want %q", got, outcomeTypeRejected)
		}
	})

	t.Run("control: an accepted row is not labeled rejected", func(t *testing.T) {
		q := newRejectedRow(t, acceptAllGuard{})

		got, ok := q.outcomeTypes[1]
		if !ok {
			t.Fatal("accepted row recorded no outcome_type")
		}
		if got == outcomeTypeRejected {
			t.Error("an ACCEPTED row was labeled rejected; the stamp is not keyed on the guard verdict")
		}
		// It wrote a synced .lrc, so that is what it must claim -- this also pins
		// the constant swap in outcomeTypeFromSong.
		if got != outcomeTypeSynced {
			t.Errorf("outcome_type = %q; want %q", got, outcomeTypeSynced)
		}
	})

	t.Run("control: no guard configured leaves the normal path intact", func(t *testing.T) {
		q := &fakeQueue{items: []queue.WorkItem{{
			ID:     1,
			Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "One"}, Outdir: "o", Filename: "a.lrc"},
		}}}
		fetcher := &seqFetcher{results: []seqResult{{
			song: models.Song{
				Track:     models.Track{ArtistName: "A", TrackName: "One"},
				Subtitles: models.Synced{Lines: []models.Lines{{Text: "x"}}},
			},
		}}}
		w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
		if err := w.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := q.outcomeTypes[1]; got != outcomeTypeSynced {
			t.Errorf("outcome_type = %q; want %q with no guard wired", got, outcomeTypeSynced)
		}
	})
}

// TestGuardRejectFailedSettleDoesNotComplete is the regression test for the
// defect BOTH reviewers caught on PR #770.
//
// The first cut of the #655 fix stamped the outcome with a best-effort
// SetOutcomeType and then called Complete unconditionally. A dropped stamp plus
// a successful Complete leaves the row `done` with a NULL outcome_type -- which
// is exactly the bug #655 exists to fix. A label that can be silently lost is
// not a fix for a missing label.
//
// The settle is now one transaction, so this asserts the property that makes it
// correct: when persistence fails, the row must NOT be completed. It is a
// genuine regression test -- under the old stamp-then-Complete code the row was
// completed regardless, so this reds.
func TestGuardRejectFailedSettleDoesNotComplete(t *testing.T) {
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID:     1,
			Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "One"}, Outdir: "o", Filename: "a.lrc"},
		}},
		guardRejectErr: errors.New("database is locked"),
	}
	fetcher := &seqFetcher{results: []seqResult{{
		song: models.Song{
			Track:     models.Track{ArtistName: "A", TrackName: "One"},
			Subtitles: models.Synced{Lines: []models.Lines{{Text: "x"}}},
		},
	}}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.EnableGuard(rejectAllGuard{reason: "script not in allowlist"})
	// Stub the backoff. w.fail puts the loop into a real 1-minute sleep, which
	// this test would otherwise serve in full -- 60s in the gate for a property
	// that is decided the moment the settle returns.
	w.sleep = func(context.Context, time.Duration) {}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// THE REGRESSION. The row must not settle when its label could not be stored.
	if len(q.completed) != 0 {
		t.Errorf("row was completed despite a failed settle; it can now be `done` with a NULL outcome_type (#655/#770)")
	}
	if _, ok := q.outcomeTypes[1]; ok {
		t.Error("an outcome was recorded despite the settle failing; the fake must model the transaction as all-or-nothing")
	}
	// It must instead take the retriable failure path, so the row is re-attempted
	// rather than stranded in 'processing' forever.
	if len(q.failed) != 1 {
		t.Errorf("failed = %v; want the row failed for retry after an unsettleable rejection", q.failed)
	}
}

// TestGuardRejectUnsettledRowIsReleased covers the non-error, non-Settled branch:
// the settle succeeded as a call but matched no row (pruned mid-flight, or a
// concurrent writer moved it).
//
// Releasing rather than failing is the point. Failing would re-queue a row that
// may no longer exist; releasing returns it to a dequeueable state without
// asserting an outcome that was never written. The prior code had no such branch
// at all -- Complete's error was the only signal, and a no-op UPDATE is not an
// error.
func TestGuardRejectUnsettledRowIsReleased(t *testing.T) {
	claimed := queue.SettleClaimed
	q := &fakeQueue{
		items: []queue.WorkItem{{
			ID:     1,
			Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "One"}, Outdir: "o", Filename: "a.lrc"},
		}},
		guardRejectOutcome: &claimed,
	}
	fetcher := &seqFetcher{results: []seqResult{{
		song: models.Song{
			Track:     models.Track{ArtistName: "A", TrackName: "One"},
			Subtitles: models.Synced{Lines: []models.Lines{{Text: "x"}}},
		},
	}}}
	w := New(q, &fakeCache{}, fetcher, &fakeWriter{})
	w.EnableGuard(rejectAllGuard{reason: "script not in allowlist"})
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(q.completed) != 0 {
		t.Errorf("completed = %v; a row that did not settle must not be recorded as done", q.completed)
	}
	if len(q.released) != 1 || q.released[0] != 1 {
		t.Errorf("released = %v; want the unsettled row released for re-dequeue", q.released)
	}
	if len(q.failed) != 0 {
		t.Errorf("failed = %v; an unsettled row must be released, not failed (it may no longer exist)", q.failed)
	}
}
