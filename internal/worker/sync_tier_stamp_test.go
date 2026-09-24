package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

// readSyncTier reads work_queue.sync_tier for id, "" for NULL.
func readSyncTier(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var tier sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT sync_tier FROM work_queue WHERE id = ?`, id).Scan(&tier); err != nil {
		t.Fatalf("read sync_tier: %v", err)
	}
	return tier.String
}

// TestOrdinarySyncTier pins #1075's completion stamp over the real
// SQLite/writer rig also used for the word_timing_state stamp tests
// (newStampRig): an ordinary synced completion whose words landed on disk
// (inline or companion) stamps 'word'; a plain line-synced completion (no
// markers, no companion -- including under word_sync_mode=off) stamps 'line'.
func TestOrdinarySyncTier(t *testing.T) {
	cases := []struct {
		name, mode string
		song       models.Song
		want       string
	}{
		{"inline word markers land", "inline", recheckSong("word line", true, models.WordAnswerServed), queue.SyncTierWord},
		{"companion lands", "sidecar", recheckSong("word line", true, models.WordAnswerServed), queue.SyncTierWord},
		{"word_sync_mode off: line only", "off", recheckSong("word line", true, models.WordAnswerServed), queue.SyncTierLine},
		{"no word timings at all: line only", "sidecar", recheckSong("line only", false, models.WordAnswerAbsent), queue.SyncTierLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, w := newStampRig(t, &fakeFetcher{song: tc.song}, nil, tc.mode, "")
			w.SetFallbackProviders()
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if got := readSyncTier(t, rig.db, rig.id); got != tc.want {
				t.Errorf("sync_tier = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOrdinarySyncTier_NonSyncedClearsPrior: the detector-instrumental and
// guard-reject settles write no synced sidecar, so each must drop a prior
// sync_tier a reopened row carried -- mirroring
// TestOrdinaryStamp_EarlySettlesClearAStaleVerdict for word_timing_state.
func TestOrdinarySyncTier_NonSyncedClearsPrior(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary *fakeFetcher
		setup   func(*Worker)
	}{
		{"detector instrumental", &fakeFetcher{err: musixmatch.ErrNotFound}, func(w *Worker) {
			w.SetFallbackProviders()
			w.EnableAudioDetector(&fakeDetector{instrumental: true, version: "9.9.9"})
			w.SetInstrumentalDetectionDefault(true)
		}},
		{"guard reject", &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, func(w *Worker) {
			w.EnableGuard(rejectAllGuard{reason: "script"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, w := newStampRig(t, tc.primary, nil, "sidecar", "")
			if err := rig.q.SetSyncTier(context.Background(), rig.id, queue.SyncTierWord); err != nil {
				t.Fatalf("seed sync tier: %v", err)
			}
			tc.setup(w)
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if got := readSyncTier(t, rig.db, rig.id); got != "" {
				t.Errorf("sync_tier = %q, want cleared to NULL", got)
			}
		})
	}
}

// failingSyncTierQueue fails only SetSyncTier, over the real DBQueue, to prove
// stampSyncTier's failure is non-fatal like its siblings.
type failingSyncTierQueue struct{ *queue.DBQueue }

func (failingSyncTierQueue) SetSyncTier(context.Context, int64, string) error {
	return errors.New("injected sync tier stamp failure")
}

// TestOrdinarySyncTier_StampAndClearBothFail is CodeRabbit thread 4098910896
// on #1085: when the stamp AND its clear both fail, the row's PRIOR tier
// would otherwise describe a file this completion may have just rewritten.
// The row must NOT reach Complete/done; it settles failed instead, so the
// wedge is retried rather than silently trusted.
func TestOrdinarySyncTier_StampAndClearBothFail(t *testing.T) {
	rig, w := newStampRig(t, &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, nil, "sidecar", "")
	w.SetFallbackProviders()
	w.queue = failingSyncTierQueue{rig.q}
	if err := w.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce: want an error when both the stamp and its clear fail")
	}
	row := rig.recheckRow(t)
	if row.status == queue.StatusDone {
		t.Fatalf("row status = %q, want NOT done (Complete must not run) when both writes fail", row.status)
	}
	if !strings.Contains(row.lastError, "stamp and clear sync tier") {
		t.Errorf("last_error = %q, want it to name the double stamp/clear failure", row.lastError)
	}
}

// failingSyncTierStampQueue fails SetSyncTier only when stamping a REAL tier
// (mirroring a stamp failure right after a sidecar just landed or was
// rewritten); the clear call (tier="") is passed through to the real
// DBQueue, so stampOrClearSyncTier's failure-triggered clear attempt can
// actually land -- #1085 review finding 2.
type failingSyncTierStampQueue struct{ *queue.DBQueue }

func (q failingSyncTierStampQueue) SetSyncTier(ctx context.Context, id int64, tier string) error {
	if tier == "" {
		return q.DBQueue.SetSyncTier(ctx, id, tier)
	}
	return errors.New("injected sync tier stamp failure")
}

// TestOrdinarySyncTier_StampFailureClearsPriorTier is #1085 review finding 2:
// a reopened row seeded with a PRIOR tier ('line') whose ordinary completion
// would stamp 'word' must not keep asserting the stale 'line' when the stamp
// fails -- it must fall back to NULL (unknown) via the clear attempt.
func TestOrdinarySyncTier_StampFailureClearsPriorTier(t *testing.T) {
	rig, w := newStampRig(t, &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, nil, "sidecar", "")
	w.SetFallbackProviders()
	if err := rig.q.SetSyncTier(context.Background(), rig.id, queue.SyncTierLine); err != nil {
		t.Fatalf("seed sync tier: %v", err)
	}
	w.queue = failingSyncTierStampQueue{rig.q}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := readSyncTier(t, rig.db, rig.id); got != "" {
		t.Errorf("sync_tier = %q, want cleared to NULL after a failed stamp (never the stale prior tier)", got)
	}
}

// TestWordRecheckWrite_StampFailureClearsPriorTier is the word-recheck-write
// twin of the above (#1085 review finding 2): seeded 'line' (an upgrade
// candidate) whose recheck write lands qualifying words -- the stamp would be
// 'word', but it fails, so the row must end NULL, never the stale 'line'.
func TestWordRecheckWrite_StampFailureClearsPriorTier(t *testing.T) {
	primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
	rig, w := newRecheckRig(t, primary, nil, false)
	if err := rig.q.SetSyncTier(context.Background(), rig.id, queue.SyncTierLine); err != nil {
		t.Fatalf("seed sync tier: %v", err)
	}
	w.queue = failingSyncTierStampQueue{rig.q}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := readSyncTier(t, rig.db, rig.id); got != "" {
		t.Errorf("sync_tier = %q, want cleared to NULL after a failed stamp (never the stale prior tier)", got)
	}
}

// TestWordRecheckWrite_StampAndClearBothFail is the word-recheck-write twin of
// TestOrdinarySyncTier_StampAndClearBothFail (CodeRabbit thread 4098910896):
// a double stamp/clear failure must not settle the row served (its prior
// tier would then describe a file the write may have just changed) -- it
// re-defers instead, the same path settleWordRecheck's own failure uses.
func TestWordRecheckWrite_StampAndClearBothFail(t *testing.T) {
	primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
	rig, w := newRecheckRig(t, primary, nil, false)
	if err := rig.q.SetSyncTier(context.Background(), rig.id, queue.SyncTierLine); err != nil {
		t.Fatalf("seed sync tier: %v", err)
	}
	w.queue = failingSyncTierQueue{rig.q}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	row := rig.recheckRow(t)
	if row.state != queue.WordTimingQueued {
		t.Fatalf("word_timing_state = %q, want still %q (unsettled) when both writes fail", row.state, queue.WordTimingQueued)
	}
	if row.status == queue.StatusDone {
		t.Fatalf("row status = %q, want re-deferred, not done", row.status)
	}
	if !strings.Contains(row.lastError, "stamp and clear sync tier") {
		t.Errorf("last_error = %q, want it to name the double stamp/clear failure", row.lastError)
	}
}

// TestWordRecheckWrite_StampsSyncTierWord: seeded 'line' first (the upgrade
// path). "foreign companion" pins #1075 finding 1: a pre-existing, foreign
// .elrc blocks planCompanion's write despite qualifying words, so the
// settled .lrc stays line-only and must stamp 'line', never 'word'.
func TestWordRecheckWrite_StampsSyncTierWord(t *testing.T) {
	foreignCompanion := []byte("[00:10.00]<00:10.00>someone else's\n")
	for _, tc := range []struct {
		name    string
		foreign bool
		want    string
	}{
		{name: "companion lands", want: queue.SyncTierWord},
		{name: "foreign companion blocks landing", foreign: true, want: queue.SyncTierLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
			rig, w := newRecheckRig(t, primary, nil, false)
			if err := rig.q.SetSyncTier(context.Background(), rig.id, queue.SyncTierLine); err != nil {
				t.Fatalf("seed sync tier: %v", err)
			}
			if tc.foreign {
				if err := os.WriteFile(rig.elrc(), foreignCompanion, 0o644); err != nil {
					t.Fatalf("seed foreign companion: %v", err)
				}
			}
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if got := readSyncTier(t, rig.db, rig.id); got != tc.want {
				t.Errorf("sync_tier = %q, want %q after a word-recheck write", got, tc.want)
			}
			if tc.foreign {
				if got, err := os.ReadFile(rig.elrc()); err != nil || string(got) != string(foreignCompanion) {
					t.Fatalf("foreign .elrc = %q, %v; want untouched", got, err)
				}
			}
		})
	}
}
