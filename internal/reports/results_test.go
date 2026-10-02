package reports_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// TestResultsBreakdownSumsToDone pins the #599 Results-row rule: every
// completed (status='done') row lands in exactly one Results bucket, so the
// buckets sum to QueueSummary.Done. The fixture covers every shape that
// decides a bucket, including the ones that fit no named tile (rejected,
// legacy NULL) and the non-done rows that must stay out entirely.
func TestResultsBreakdownSumsToDone(t *testing.T) {
	sqlDB := openTestDB(t)
	rows := []workItem{
		{artist: "A", title: "word", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "word2", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "line", status: "done", outcomeType: "synced", syncTier: "line"},
		// Tier unknown: no tier, an 'unsynced' tier on a .lrc, remediated
		// word/line tiers, and prune's retired done+queued shape.
		{artist: "A", title: "untiered", status: "done", outcomeType: "synced"},
		{artist: "A", title: "tier-unsynced", status: "done", outcomeType: "synced", syncTier: "unsynced"},
		{artist: "A", title: "quarantined", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "categorical"},
		{artist: "A", title: "demoted-line", status: "done", outcomeType: "synced", syncTier: "line", timingOutcome: "mis_synced"},
		{artist: "A", title: "retired-queued", status: "done", outcomeType: "synced", syncTier: "word", wordTimingState: "queued"},
		// prune's retired row keeps its stale outcome/tier but is not a result.
		{artist: "A", title: "retired-stale", status: "done", outcomeType: "synced", syncTier: "word", lastError: queue.UnresolvableGoneError},
		{artist: "A", title: "retired-line", status: "done", outcomeType: "synced", syncTier: "line", lastError: queue.UnresolvableGoneError},
		// No tier: only the explicit retirement arm keeps this out of tier-unknown.
		{artist: "A", title: "retired-untiered", status: "done", outcomeType: "synced", lastError: queue.UnresolvableGoneError},
		{artist: "A", title: "txt", status: "done", outcomeType: "unsynced"},
		// A stray word tier on a non-synced row does not make it word-synced.
		{artist: "A", title: "stray", status: "done", outcomeType: "unsynced", syncTier: "word"},
		{artist: "A", title: "instr", status: "done", outcomeType: "instrumental"},
		{artist: "A", title: "instr2", status: "done", outcomeType: "instrumental"},
		{artist: "A", title: "rejected", status: "done", outcomeType: "rejected"},
		{artist: "A", title: "legacy", status: "done"},
		// Not completed: never counted, even with a result-shaped outcome.
		{artist: "A", title: "rechecking", status: "deferred", outcomeType: "synced", syncTier: "word", wordTimingState: "queued"},
		{artist: "A", title: "retired", status: "unavailable", outcomeType: "synced", syncTier: "line"},
		{artist: "A", title: "instr-pending", status: "pending", outcomeType: "instrumental"},
	}
	for _, w := range rows {
		insertWorkItem(t, sqlDB, w)
	}
	repo := reports.New(sqlDB)
	ctx := context.Background()

	got, err := repo.ResultsBreakdown(ctx)
	if err != nil {
		t.Fatalf("ResultsBreakdown: %v", err)
	}
	want := reports.ResultsBreakdown{
		WordSynced: 2, LineSynced: 1, SyncedTierUnknown: 5,
		Unsynced: 2, Instrumental: 2, Other: 5,
	}
	if got != want {
		t.Errorf("ResultsBreakdown = %+v, want %+v", got, want)
	}

	qs, err := repo.QueueSummary(ctx)
	if err != nil {
		t.Fatalf("QueueSummary: %v", err)
	}
	if got.Total() != qs.Done {
		t.Errorf("Results total %d != QueueSummary.Done %d (%+v)", got.Total(), qs.Done, got)
	}
	// Word-synced and Finished are one predicate, not two that can drift.
	if got.WordSynced != qs.Finished {
		t.Errorf("WordSynced %d != QueueSummary.Finished %d", got.WordSynced, qs.Finished)
	}
}

// TestResultsBreakdownRemediatedRowsNeverTiered pins (ported from the retired
// SyncTierCounts suite, #1200) that a row the timing guard remediated
// (categorical / mis_synced / degenerate) never counts as word- or
// line-synced, whatever stale tier it kept: it routes to tier-unknown. Both
// tiers are exercised for every verdict, so neither tier predicate can drop
// the exclusion unnoticed.
func TestResultsBreakdownRemediatedRowsNeverTiered(t *testing.T) {
	sqlDB := openTestDB(t)
	rows := []workItem{
		{artist: "A", title: "ok", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "cat-word", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "categorical"},
		{artist: "A", title: "cat-line", status: "done", outcomeType: "synced", syncTier: "line", timingOutcome: "categorical"},
		{artist: "A", title: "missync-word", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "mis_synced"},
		{artist: "A", title: "missync-line", status: "done", outcomeType: "synced", syncTier: "line", timingOutcome: "mis_synced"},
		{artist: "A", title: "deg-word", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "degenerate"},
		{artist: "A", title: "deg-line", status: "done", outcomeType: "synced", syncTier: "line", timingOutcome: "degenerate"},
	}
	for _, w := range rows {
		insertWorkItem(t, sqlDB, w)
	}
	got, err := reports.New(sqlDB).ResultsBreakdown(context.Background())
	if err != nil {
		t.Fatalf("ResultsBreakdown: %v", err)
	}
	want := reports.ResultsBreakdown{WordSynced: 1, SyncedTierUnknown: 6}
	if got != want {
		t.Errorf("ResultsBreakdown = %+v, want %+v (remediated rows route to tier unknown)", got, want)
	}
}

// TestResultsBreakdownQueuedRowKeepsStaleTierAsUnknown pins (ported from the
// retired SyncTierCounts suite, #1085 finding 1 / #1200) that a done row left
// mid word-recheck (word_timing_state='queued', prune's retired shape) never
// counts as tiered even when it carries a stale word or line tier.
func TestResultsBreakdownQueuedRowKeepsStaleTierAsUnknown(t *testing.T) {
	sqlDB := openTestDB(t)
	rows := []workItem{
		{artist: "A", title: "queued-word", status: "done", outcomeType: "synced", syncTier: "word", wordTimingState: "queued"},
		{artist: "A", title: "queued-line", status: "done", outcomeType: "synced", syncTier: "line", wordTimingState: "queued"},
		{artist: "A", title: "queued-untiered", status: "done", outcomeType: "synced", wordTimingState: "queued"},
	}
	for _, w := range rows {
		insertWorkItem(t, sqlDB, w)
	}
	got, err := reports.New(sqlDB).ResultsBreakdown(context.Background())
	if err != nil {
		t.Fatalf("ResultsBreakdown: %v", err)
	}
	want := reports.ResultsBreakdown{SyncedTierUnknown: 3}
	if got != want {
		t.Errorf("ResultsBreakdown = %+v, want %+v (queued row's stale tier must not count)", got, want)
	}
}

// TestResultsBreakdownWordSyncedMatchesFinished pins (ported from the retired
// SyncTierCounts agreement check, #1200) that the Results row's word-synced
// bucket and QueueSummary.Finished are one predicate, over the full
// finished-split fixture (every remediated, queued, stray and non-done shape).
func TestResultsBreakdownWordSyncedMatchesFinished(t *testing.T) {
	repo, _, wantFinished := seedFinishedSplit(t)
	ctx := context.Background()
	qs, err := repo.QueueSummary(ctx)
	if err != nil {
		t.Fatalf("QueueSummary: %v", err)
	}
	rb, err := repo.ResultsBreakdown(ctx)
	if err != nil {
		t.Fatalf("ResultsBreakdown: %v", err)
	}
	if rb.WordSynced != qs.Finished {
		t.Errorf("ResultsBreakdown.WordSynced = %d, QueueSummary.Finished = %d; want equal", rb.WordSynced, qs.Finished)
	}
	if rb.WordSynced != wantFinished {
		t.Errorf("ResultsBreakdown.WordSynced = %d, want %d", rb.WordSynced, wantFinished)
	}
}

// TestResultsBreakdownEmptyAndClosed covers a fresh install (every bucket
// zero, no NULL-scan failure) and the query-error branch.
func TestResultsBreakdownEmptyAndClosed(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	got, err := repo.ResultsBreakdown(context.Background())
	if err != nil {
		t.Fatalf("ResultsBreakdown on empty DB: %v", err)
	}
	if got != (reports.ResultsBreakdown{}) {
		t.Errorf("empty DB: got %+v, want all zero", got)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := repo.ResultsBreakdown(context.Background()); err == nil {
		t.Error("ResultsBreakdown on closed DB: want error")
	}
}

// TestRetiredSentinelHasNoQuote guards the inlined retiredPredicate literal:
// a quote in the sentinel would break (or inject into) every query using it.
func TestRetiredSentinelHasNoQuote(t *testing.T) {
	if strings.ContainsAny(queue.UnresolvableGoneError, "'\"") {
		t.Fatalf("queue.UnresolvableGoneError contains a quote; retiredPredicate inlines it")
	}
}
