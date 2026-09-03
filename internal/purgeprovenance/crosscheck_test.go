package purgeprovenance

import (
	"context"
	"database/sql"
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
