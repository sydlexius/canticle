package reports_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// seedFinishedSplit seeds one row per shape that decides "finished" (#553):
// only a settled, word-tier, synced row that is neither timing-remediated nor
// mid-recheck is finished; every other 'done' row is settled but upgradable.
// Returns the expected (done, finished) counts.
func seedFinishedSplit(t *testing.T) (repo *reports.Repo, wantDone, wantFinished int64) {
	t.Helper()
	sqlDB := openTestDB(t)
	rows := []workItem{
		// Finished: the one terminal rung.
		{artist: "A", title: "word", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "word2", status: "done", outcomeType: "synced", syncTier: "word"},
		// Settled, upgradable: every lower rung, whatever wrote it.
		{artist: "A", title: "line", status: "done", outcomeType: "synced", syncTier: "line"},
		{artist: "A", title: "untiered", status: "done", outcomeType: "synced"},
		{artist: "A", title: "unsynced", status: "done", outcomeType: "unsynced"},
		{artist: "A", title: "instr", status: "done", outcomeType: "instrumental"},
		{artist: "A", title: "legacy", status: "done"},
		// A word tier the timing guard later remediated is stale, not finished.
		{artist: "A", title: "quarantined", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "categorical"},
		{artist: "A", title: "demoted", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "mis_synced"},
		// #1082: a degenerate verdict demotes too, so its word tier is stale.
		{artist: "A", title: "degenerate", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "degenerate"},
		// prune's retired done+queued shape: mid-recheck, tier not re-litigated.
		{artist: "A", title: "retired-queued", status: "done", outcomeType: "synced", syncTier: "word", wordTimingState: "queued"},
		// A non-done word row is not settled at all, so it is neither counter.
		{artist: "A", title: "rechecking", status: "deferred", outcomeType: "synced", syncTier: "word", wordTimingState: "queued"},
		// A non-synced row carrying a stray word tier is never finished.
		{artist: "A", title: "stray", status: "done", outcomeType: "unsynced", syncTier: "word"},
	}
	for _, w := range rows {
		insertWorkItem(t, sqlDB, w)
	}
	return reports.New(sqlDB), 12, 2
}

// TestQueueSummaryFinishedSplit pins the 2026-09-24 decision on #553: Done
// splits into Finished (word-synced) and SettledUpgradable (everything else
// settled), and the two always sum to Done.
func TestQueueSummaryFinishedSplit(t *testing.T) {
	repo, wantDone, wantFinished := seedFinishedSplit(t)
	got, err := repo.QueueSummary(context.Background())
	if err != nil {
		t.Fatalf("QueueSummary: %v", err)
	}
	if got.Done != wantDone {
		t.Errorf("Done = %d, want %d", got.Done, wantDone)
	}
	if got.Finished != wantFinished {
		t.Errorf("Finished = %d, want %d", got.Finished, wantFinished)
	}
	if got.SettledUpgradable != wantDone-wantFinished {
		t.Errorf("SettledUpgradable = %d, want %d", got.SettledUpgradable, wantDone-wantFinished)
	}
	if got.Finished+got.SettledUpgradable != got.Done {
		t.Errorf("Finished(%d)+SettledUpgradable(%d) != Done(%d)", got.Finished, got.SettledUpgradable, got.Done)
	}
}
