package reports_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/tablesort"
)

func bucketCount(t *testing.T, repo *reports.Repo, b reports.Bucket, f reports.BucketFilter) int64 {
	t.Helper()
	rows, err := repo.ListBucketFiltered(context.Background(), b, f, reports.BucketSpec(b).Default, tablesort.Cursor{}, reports.MaxBucketLimit)
	if err != nil {
		t.Fatalf("ListBucketFiltered(%s): %v", b, err)
	}
	return int64(len(rows))
}

// TestFinishedFollowsTopRung pins #1275: with word sync off a settled
// line-synced row is Finished, with it on nothing changes, and in both rungs
// QueueSummary, the Finished/Settled listings, the Line-synced chip and
// ResultsBreakdown describe one population. Each surface is checked against
// fixed counts AND against the others, so changing one shared predicate alone
// fails here.
func TestFinishedFollowsTopRung(t *testing.T) {
	d := openTestDB(t)
	for _, w := range []workItem{
		{artist: "A", title: "word", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "line", status: "done", outcomeType: "synced", syncTier: "line"},
		{artist: "A", title: "line2", status: "done", outcomeType: "synced", syncTier: "line"},
		// The shared tier predicates' exclusions: never a current line tier.
		{artist: "A", title: "line-missync", status: "done", outcomeType: "synced", syncTier: "line", timingOutcome: "mis_synced"},
		{artist: "A", title: "line-recheck", status: "done", outcomeType: "synced", syncTier: "line", wordTimingState: "queued"},
		{artist: "A", title: "line-retired", status: "done", outcomeType: "synced", syncTier: "line", lastError: queue.UnresolvableGoneError},
		{artist: "A", title: "line-unsynced-outcome", status: "done", outcomeType: "unsynced", syncTier: "line"},
		{artist: "A", title: "untiered", status: "done", outcomeType: "synced"},
		{artist: "A", title: "instr", status: "done", outcomeType: "instrumental"},
		{artist: "A", title: "line-pending", status: "pending", outcomeType: "synced", syncTier: "line"},
	} {
		insertWorkItem(t, d, w)
	}
	const done, line = 9, 2
	for _, tc := range []struct {
		repo     *reports.Repo
		top      reports.TopRung
		finished int64
		chipOn   reports.Bucket // the bucket offering the Line-synced chip
	}{
		{reports.New(d), reports.TopRungWord, 1, reports.BucketSettled},
		{reports.New(d, reports.WithLineTopRung(true)), reports.TopRungLine, 3, reports.BucketFinished},
	} {
		ctx := context.Background()
		qs, err := tc.repo.QueueSummary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rb, err := tc.repo.ResultsBreakdown(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if tc.repo.TopRung() != tc.top || qs.TopRung != tc.top || rb.TopRung != tc.top {
			t.Errorf("rung %d: repo/summary/results rungs = %d/%d/%d", tc.top, tc.repo.TopRung(), qs.TopRung, rb.TopRung)
		}
		if qs.Done != done || qs.Finished != tc.finished || qs.SettledUpgradable != done-tc.finished {
			t.Errorf("rung %d: done/finished/settled = %d/%d/%d, want %d/%d/%d",
				tc.top, qs.Done, qs.Finished, qs.SettledUpgradable, done, tc.finished, done-tc.finished)
		}
		fin := bucketCount(t, tc.repo, reports.BucketFinished, reports.BucketFilter{})
		set := bucketCount(t, tc.repo, reports.BucketSettled, reports.BucketFilter{})
		if fin != qs.Finished || set != qs.SettledUpgradable {
			t.Errorf("rung %d: listings finished/settled = %d/%d, summary %d/%d", tc.top, fin, set, qs.Finished, qs.SettledUpgradable)
		}
		top := rb.WordSynced
		if tc.top == reports.TopRungLine {
			top += rb.LineSynced
		}
		if qs.Finished != top || rb.Total() != qs.Done {
			t.Errorf("rung %d: Finished %d vs results top rungs %d; results total %d vs Done %d", tc.top, qs.Finished, top, rb.Total(), qs.Done)
		}
		if !reports.HasChip(tc.chipOn, reports.ChipLineSynced, tc.top) {
			t.Fatalf("rung %d: Line-synced chip not offered on %s", tc.top, tc.chipOn)
		}
		if got := bucketCount(t, tc.repo, tc.chipOn, reports.BucketFilter{Tier: reports.TierLine}); got != line || got != rb.LineSynced {
			t.Errorf("rung %d: line chip on %s lists %d, want %d (dashboard %d)", tc.top, tc.chipOn, got, line, rb.LineSynced)
		}
	}
}
