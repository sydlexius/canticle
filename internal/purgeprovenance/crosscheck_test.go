package purgeprovenance

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
)

// setLane stamps work_queue.provider_lane on a seeded row. An empty lane leaves
// the column NULL, which is what an uncompleted (or lane-cleared) row carries.
func setLane(t *testing.T, ctx context.Context, sqlDB *sql.DB, wqID int64, lane string) {
	t.Helper()
	var v any
	if lane != "" {
		v = lane
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET provider_lane = ? WHERE id = ?`, v, wqID); err != nil {
		t.Fatalf("set provider_lane: %v", err)
	}
}

// TestRun_RefusesWhenTagDisagreesWithProviderLane is the deletion hazard from
// issue #827. A sidecar written while the lane-misattribution bug was live
// carries a [source:] tag naming one provider while the database row credits
// another. purge-provenance selects by the TAG, so `--source musixmatch` would
// otherwise delete a file the database says petitlyrics served. Provenance that
// two authorities disagree about is uncertain, and deleting on uncertain
// provenance is exactly the failure mode to prevent.
//
// The assertion is on the ARTIFACT: the file is still on disk, and the coupled
// rows were never reset.
func TestRun_RefusesWhenTagDisagreesWithProviderLane(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	path := filepath.Join(root, "A", "track.lrc")
	writeSidecar(t, path, "musixmatch")
	srID, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "done")
	setLane(t, ctx, sqlDB, wqID, "petitlyrics")

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("sidecar was DELETED despite its [source:] tag disagreeing with work_queue.provider_lane; "+
			"that is the #827 deletion hazard: %v", serr)
	}
	if res.SkippedProvenanceMismatch != 1 {
		t.Errorf("SkippedProvenanceMismatch = %d; want 1", res.SkippedProvenanceMismatch)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted = %d; want 0", res.Deleted)
	}
	if res.WorkItemsRequeued != 0 || res.ScanResultsReset != 0 {
		t.Errorf("rows were mutated for a refused sidecar: requeued=%d reset=%d; want 0/0",
			res.WorkItemsRequeued, res.ScanResultsReset)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", srID); got != "done" {
		t.Errorf("scan_results status = %q; want %q untouched", got, "done")
	}
}

// TestRun_DryRunReportsNothingForAMismatch locks the preview against the apply
// run: a refused sidecar must not appear in the dry-run purge set either, or the
// operator is told a file will be deleted that never will be.
func TestRun_DryRunReportsNothingForAMismatch(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	path := filepath.Join(root, "A", "track.lrc")
	writeSidecar(t, path, "musixmatch")
	_, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "done")
	setLane(t, ctx, sqlDB, wqID, "petitlyrics")

	var reported []string
	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: true,
		Report: func(rec Record) error { reported = append(reported, rec.Path); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(reported) != 0 {
		t.Errorf("dry run previewed %d sidecar(s) it would refuse to delete; want 0", len(reported))
	}
	if res.SkippedProvenanceMismatch != 1 {
		t.Errorf("SkippedProvenanceMismatch = %d; want 1", res.SkippedProvenanceMismatch)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("dry run touched the filesystem: %v", serr)
	}
}

// TestRun_CrossCheckAgreementCases covers every shape the cross-check must NOT
// refuse. Each of these deletes; a regression that widened the refusal into any
// of them would silently disable purge-provenance for that whole cohort.
func TestRun_CrossCheckAgreementCases(t *testing.T) {
	tests := []struct {
		name string
		// tag is the [source:] value written into the sidecar ("" writes no tag).
		tag string
		// lane is the work_queue.provider_lane value ("" leaves it NULL).
		lane string
		// noRow seeds no scan_results/work_queue row at all.
		noRow bool
		// filter is the purge filter under test.
		filter Filter
	}{
		{
			// The ordinary case: the tag and the row name the same provider.
			name: "tag agrees with lane", tag: "musixmatch", lane: "musixmatch",
			filter: Filter{Source: "musixmatch"},
		},
		{
			// A NULL lane asserts nothing (not-yet-completed, or a cleared
			// verdict), so there is nothing for the tag to contradict.
			name: "null lane asserts nothing", tag: "musixmatch", lane: "",
			filter: Filter{Source: "musixmatch"},
		},
		{
			// No coupled row at all: no second authority exists, so the tag
			// stands alone. This is the pre-existing UnlinkedNoCacheKey cohort.
			name: "no linked row", tag: "musixmatch", noRow: true,
			filter: Filter{Source: "musixmatch"},
		},
		{
			// A detector-written marker tags [source:canticle-detector] while the
			// row's lane is "detector". That spelling difference is by design, not
			// misattribution -- refusing it would break --source canticle-detector
			// for every instrumental marker in the library.
			name: "detector tag matches detector lane", tag: lyrics.SourceDetector, lane: lyrics.DetectorLaneName,
			filter: Filter{Source: lyrics.SourceDetector},
		},
		{
			// --no-source selects the untagged cohort canticle never wrote. There
			// is no tag to contradict, so a row lane is not a disagreement.
			name: "no-source filter ignores the lane", tag: "", lane: "musixmatch",
			filter: Filter{NoSource: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			path := filepath.Join(root, "A", "track.lrc")
			writeSidecar(t, path, tt.tag)
			if !tt.noRow {
				_, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "done")
				setLane(t, ctx, sqlDB, wqID, tt.lane)
			}

			res, err := New(sqlDB).Run(ctx, Options{
				Roots: []string{root}, LibraryID: &libID,
				Filter: tt.filter, DryRun: false,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.SkippedProvenanceMismatch != 0 {
				t.Errorf("SkippedProvenanceMismatch = %d; want 0 -- this shape is not a disagreement",
					res.SkippedProvenanceMismatch)
			}
			if res.Deleted != 1 {
				t.Fatalf("Deleted = %d; want 1", res.Deleted)
			}
			if _, serr := os.Stat(path); !os.IsNotExist(serr) {
				t.Errorf("sidecar still on disk after an agreeing purge: %v", serr)
			}
		})
	}
}

// TestRun_DisputedProvenanceIsCountedEvenWhenInFlight covers the ordering
// CodeRabbit flagged on #830: a sidecar can be BOTH in-flight and disputed, and
// the in-flight return used to run first, so such a file was reported only as
// "skipped, processing". The file was safe either way, but the operator never
// saw the safety-relevant half -- in-flight is transient and resolves on the
// next run, while disputed provenance is a standing contradiction to look at.
//
// Exactly one counter must increment: the CLI preview subtracts BOTH from the
// matched total, so double-counting would understate what a purge will delete.
func TestRun_DisputedProvenanceIsCountedEvenWhenInFlight(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	path := filepath.Join(root, "track.lrc")
	writeSidecar(t, path, "musixmatch")
	_, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "processing")
	setLane(t, ctx, sqlDB, wqID, "petitlyrics")

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedProvenanceMismatch != 1 {
		t.Errorf("SkippedProvenanceMismatch = %d, want 1: a disputed sidecar must be reported as disputed even while in flight",
			res.SkippedProvenanceMismatch)
	}
	if res.SkippedProcessing != 0 {
		t.Errorf("SkippedProcessing = %d, want 0: exactly one counter may increment per sidecar (the CLI subtracts both)",
			res.SkippedProcessing)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0", res.Deleted)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("sidecar was deleted: %v", serr)
	}
}

// TestRun_InFlightAgreeingSidecarStillCountsAsProcessing is the control for the
// test above: reordering the two branches must not steal the ordinary in-flight
// case, which has nothing disputed about it.
func TestRun_InFlightAgreeingSidecarStillCountsAsProcessing(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	path := filepath.Join(root, "track.lrc")
	writeSidecar(t, path, "musixmatch")
	_, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "processing")
	setLane(t, ctx, sqlDB, wqID, "musixmatch")

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SkippedProcessing != 1 {
		t.Errorf("SkippedProcessing = %d, want 1", res.SkippedProcessing)
	}
	if res.SkippedProvenanceMismatch != 0 {
		t.Errorf("SkippedProvenanceMismatch = %d, want 0: this sidecar's provenance is not disputed", res.SkippedProvenanceMismatch)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("sidecar was deleted: %v", serr)
	}
}

// TestDisputedLanes_ReReadsThroughTheTransaction covers the stale-snapshot
// finding: the lanes checked during the walk come from buildIndex, which runs
// ONCE before the filesystem walk, so on a large library that snapshot can be
// minutes old by the time a given sidecar is reached. disputedLanes re-reads
// through the reset transaction so a lane corrected in the interim cannot let a
// now-disputed file be deleted on an agreeing value that no longer holds.
func TestDisputedLanes_ReReadsThroughTheTransaction(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	path := filepath.Join(root, "track.lrc")
	writeSidecar(t, path, "musixmatch")
	_, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "done")
	setLane(t, ctx, sqlDB, wqID, "musixmatch")

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Agreeing at read time.
	disputed, err := disputedLanes(ctx, tx, []int64{wqID}, "musixmatch")
	if err != nil {
		t.Fatalf("disputedLanes: %v", err)
	}
	if len(disputed) != 0 {
		t.Errorf("disputedLanes = %v, want empty for an agreeing lane", disputed)
	}

	// Now disagreeing -- what a lane correction landing mid-walk looks like.
	disputed, err = disputedLanes(ctx, tx, []int64{wqID}, "petitlyrics")
	if err != nil {
		t.Fatalf("disputedLanes: %v", err)
	}
	if len(disputed) != 1 || disputed[0] != wqID {
		t.Errorf("disputedLanes = %v, want [%d]: a lane that no longer agrees must be reported", disputed, wqID)
	}

	// A vanished row cannot contradict anything.
	disputed, err = disputedLanes(ctx, tx, []int64{wqID + 9999}, "musixmatch")
	if err != nil {
		t.Fatalf("disputedLanes: %v", err)
	}
	if len(disputed) != 0 {
		t.Errorf("disputedLanes = %v, want empty for a missing row", disputed)
	}
}

// TestResetRows_RefusesWhenTheLaneChangedUnderfoot exercises the guard where it
// actually lives, in resetRows' transaction -- not the disputedLanes helper in
// isolation. Testing the helper alone leaves the WIRING unverified: neutering
// resetRows' use of it left the helper's own test green.
//
// The scenario is the stale-snapshot race: buildIndex read an agreeing lane
// before the walk, the lane was corrected while the walk was still running, and
// this sidecar is only now reached. The reset must abort rather than authorize a
// delete on a value that no longer holds, and it must leave the rows untouched.
func TestResetRows_RefusesWhenTheLaneChangedUnderfoot(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	path := filepath.Join(root, "track.lrc")
	writeSidecar(t, path, "musixmatch")
	srID, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "done")
	// The lane as it stands NOW: corrected since the pre-walk snapshot.
	setLane(t, ctx, sqlDB, wqID, "petitlyrics")

	statusBefore := rowStatus(t, ctx, sqlDB, "work_queue", wqID)

	// The tag the walk matched on still says musixmatch, which is what the stale
	// index agreed with.
	_, _, _, err := New(sqlDB).resetRows(ctx,
		[]int64{srID}, []int64{wqID},
		[]trackIdentity{{artist: "Artist", title: "Title"}},
		"musixmatch")
	if err == nil {
		t.Fatal("resetRows accepted a lane that changed under it; the delete would have been authorized on a stale value")
	}
	if !errors.Is(err, errProvenanceChangedUnderfoot) {
		t.Errorf("error = %v, want errProvenanceChangedUnderfoot", err)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wqID); got != statusBefore {
		t.Errorf("work_queue status = %q, want %q unchanged: the aborted transaction must not have reset the row", got, statusBefore)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("sidecar was deleted: %v", serr)
	}
}

// The control: an agreeing lane must still reset normally, so the guard cannot
// pass by refusing everything.
func TestResetRows_ProceedsWhenTheLaneStillAgrees(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	path := filepath.Join(root, "track.lrc")
	writeSidecar(t, path, "musixmatch")
	srID, wqID := seedTrack(t, ctx, sqlDB, libID, filepath.Dir(path), "track.lrc", "done")
	setLane(t, ctx, sqlDB, wqID, "musixmatch")

	_, wqReset, _, err := New(sqlDB).resetRows(ctx,
		[]int64{srID}, []int64{wqID},
		[]trackIdentity{{artist: "Artist", title: "Title"}},
		"musixmatch")
	if err != nil {
		t.Fatalf("resetRows refused an agreeing lane: %v", err)
	}
	if wqReset != 1 {
		t.Errorf("work items requeued = %d, want 1", wqReset)
	}
}
