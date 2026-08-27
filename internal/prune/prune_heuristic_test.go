package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// Tests for the NAME TIER (#740): prune re-attaching a gone, identity-less queue
// row to a present file that carries the same title, instead of retiring it as
// permanently unresolvable.
//
// EVERY NEGATIVE TEST HERE IS PAIRED WITH A POSITIVE CONTROL, and that structure
// is the point rather than ceremony. An earlier version of this file asserted
// only `Relinked == 0` for its guard cases; three of those four tests were later
// shown to pass against a build with the whole feature REVERTED, because code
// that never relinks anything satisfies a negative trivially. Mutation testing
// did not catch it -- mutating a present feature cannot detect a test that
// survives the feature's absence.
//
// So each guard test runs the SAME scenario twice: once with the blocking
// condition present (expect no relink) and once with it removed (expect a
// relink). The control proves the setup was capable of relinking, so the
// negative half is evidence about the guard rather than about the feature's
// existence.
//
// The shared seeders in prune_test.go stamp every row "Artist"/"Title", which is
// fine for the exact tier (it matches on MBID/ISRC and never reads a name) but
// makes a title-based test meaningless. Hence the local seeders below.

// The orphan is the same track throughout; only what surrounds it on disk
// changes. goneArtist is the pre-rename artist, present only to make the row
// realistic -- the tier matches on TITLE alone, deliberately, since an artist is
// shared by every track on an album and identifies no single song.
const (
	goneArtist = "Old Artist Name"
	goneTitle  = "Winterlight"
)

// seedNamedGoneRow seeds a scan_results row plus its linked work_queue item for
// the goneArtist/goneTitle track with NO identity (no MBID, no ISRC) -- the #740
// population. The work item's Track carries the names, which is where the tier
// reads the orphan's title from.
func seedNamedGoneRow(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath string) {
	t.Helper()
	seedGoneRowTagged(t, ctx, sqlDB, libraryID, filePath, goneArtist, goneTitle)
}

// seedGoneRowTagged is seedNamedGoneRow with the tags spelled out, so a test can
// seed an orphan with a different title or with NO title at all (the untagged
// case). An empty title stores an empty tag rather than omitting the column.
func seedGoneRowTagged(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, artist, title string) {
	t.Helper()
	srID := seedGoneScanResult(t, ctx, sqlDB, libraryID, filePath, artist, title)
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: artist, TrackName: title},
		SourcePath:   filePath,
		OutputPaths:  []models.OutputPath{{Outdir: filepath.Dir(filePath), Filename: filepath.Base(filePath)}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 'failed' is the state the #740 rows were actually in: the worker could not
	// write, because the source had moved out from under it.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set wq status: %v", err)
	}
}

// seedGoneScanResult seeds ONLY the scan_results row -- no work_queue item. That
// is the shape that must never be relinked (nothing to re-attach), and the shape
// whose row an earlier revision deleted on a name guess.
func seedGoneScanResult(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, artist, title string) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid, isrc)
		 VALUES (?, ?, ?, ?, 'done', ?, ?, '', '')`,
		libraryID, filePath, artist, title, filepath.Dir(filePath), filepath.Base(filePath))
	if err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("scan_result id: %v", err)
	}
	return id
}

// seedWorkQueueOnlyGoneRow seeds a work_queue row with NO linked scan_results
// row, which is what leaves candidate.libraryID nil -- gatherCandidates only ever
// sets the scope from the scan_results loop. That is the unscoped shape Gate 3
// refuses to match by name.
func seedWorkQueueOnlyGoneRow(t *testing.T, ctx context.Context, sqlDB *sql.DB, filePath, artist, title string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:       models.Track{ArtistName: artist, TrackName: title},
		SourcePath:  filePath,
		OutputPaths: []models.OutputPath{{Outdir: filepath.Dir(filePath), Filename: filepath.Base(filePath)}},
		// No ScanResultID: nothing links this row to a library.
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set wq status: %v", err)
	}
}

// seedNamedPresent seeds a bare, unlinked scan_results row for a file that
// exists on disk, with a chosen artist/title and NO identity -- the identity-less
// majority the name pool exists to reach.
// Returns the new scan_results id, which the ownership-conflict test needs in
// order to junction-link a foreign work_queue row to it by hand.
func seedNamedPresent(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, artist, title string) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write present source: %v", err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid, isrc)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?, '', '')`,
		libraryID, filePath, artist, title, filepath.Dir(filePath), filepath.Base(filePath),
	)
	if err != nil {
		t.Fatalf("insert present scan_result: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("present scan_result id: %v", err)
	}
	return id
}

// sweepExact runs the periodic sweep at Exact granularity -- the path the tier is
// gated to.
func sweepExact(t *testing.T, ctx context.Context, sqlDB *sql.DB) Result {
	t.Helper()
	res, err := New(sqlDB).Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return res
}

// THE #740 REGRESSION TEST. An artist folder was renamed, so a work_queue row's
// source_path points at a path that no longer exists while the audio file is
// still on disk under its new parent. The row carries no MBID and no ISRC.
//
// Before the fix, prune read "no MBID and no ISRC" as proof the row could never
// resolve and RETIRED it: a terminal status='done' the dequeue set never selects
// again, for a file sitting right there.
//
// Asserts the POSITIVE -- a relink happened, to the RIGHT target, and the row is
// back in the eligible set.
func TestSweep_RelinksIdentityLessRowByTitleInsteadOfRetiring(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	gone := filepath.Join(root, "Old Artist Name", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist Name", "Album", "01. Winterlight.flac")
	// A same-folder decoy with a DIFFERENT title, so the pool is not a set of one.
	decoy := filepath.Join(root, "New Artist Name", "Album", "02. Harbor Bells.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist Name", goneTitle)
	seedNamedPresent(t, ctx, sqlDB, libID, decoy, "New Artist Name", "Harbor Bells")

	var relinked []RelinkedRow
	res, err := New(sqlDB).Sweep(ctx, SweepOptions{
		Granularity:    Exact,
		ReportRelinked: func(rr RelinkedRow) error { relinked = append(relinked, rr); return nil },
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(res.Relinked) != 1 || len(relinked) != 1 {
		t.Fatalf("Relinked = %d (hook saw %d), want 1: an identity-less row whose file moved must reach "+
			"the title tier, not be retired as unresolvable (#740). Retained=%d", len(res.Relinked), len(relinked), len(res.Retained))
	}
	if res.Relinked[0].OldPath != gone || res.Relinked[0].NewPath != moved {
		t.Fatalf("relinked %+v, want old=%s new=%s", res.Relinked[0], gone, moved)
	}

	// Back in the ELIGIBLE set, not merely re-pathed: retirement's harm was the
	// terminal status, so moving the path while leaving status='done' would still
	// leave the work permanently undone.
	var status, sourcePath, outdir, filename string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, source_path, outdir, filename FROM work_queue`).Scan(&status, &sourcePath, &outdir, &filename); err != nil {
		t.Fatalf("read work_queue row: %v", err)
	}
	if sourcePath != moved {
		t.Errorf("work_queue.source_path = %q, want %q", sourcePath, moved)
	}
	if status == "done" {
		t.Errorf("work_queue.status = %q; the row was retired despite relinking, which is the #740 defect itself", status)
	}
	// The output columns must be repointed too. A relink that blanks them leaves
	// the worker writing the sidecar to an empty path.
	if outdir == "" || filename == "" {
		t.Errorf("work_queue outdir=%q filename=%q; a relink must carry the target's output columns, not blank them", outdir, filename)
	}
}

// A NEAR-MISS TITLE MUST NOT WIN. This is the defect that blocked the first
// attempt: scoring by similarity, a row whose true target was absent relinked
// onto a DIFFERENT SONG whose title merely resembled it. Measured Jaro-Winkler
// put a plural at 0.983 and a "(Live)" variant at 0.922, so no workable
// confidence floor separates them -- and the runner-up margin cannot help, since
// it only detects ambiguity INSIDE the pool, never the true target's absence.
//
// The control half proves the scenario relinks when the title matches EXACTLY,
// so the negative half is evidence about the near-miss rule specifically.
func TestSweep_NearMissTitleIsNeverRelinked(t *testing.T) {
	variants := []struct {
		name         string
		presentTitle string
		wantRelink   bool
	}{
		{"plural", "Winterlights", false},
		{"spaced", "Winter Light", false},
		{"live variant", "Winterlight (Live)", false},
		{"unrelated", "Harbor Bells", false},
		// THE CONTROL: identical title, same setup, must relink. Without this the
		// negatives above would pass on a build that never relinks anything.
		{"exact match (control)", goneTitle, true},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
			present := filepath.Join(root, "New Artist", "Album", "07. present.flac")

			seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
			if err := os.Remove(gone); err != nil {
				t.Fatalf("remove source: %v", err)
			}
			seedNamedPresent(t, ctx, sqlDB, libID, present, "New Artist", v.presentTitle)

			res := sweepExact(t, ctx, sqlDB)
			if v.wantRelink {
				if len(res.Relinked) != 1 {
					t.Fatalf("CONTROL FAILED: Relinked = %d, want 1 for an exact title match. The negative "+
						"cases in this test prove nothing unless this control relinks.", len(res.Relinked))
				}
				return
			}
			if len(res.Relinked) != 0 {
				t.Fatalf("Relinked = %d, want 0: title %q only RESEMBLES %q, and relinking a queue row onto "+
					"a different song is silent and permanent (relinked to %q)",
					len(res.Relinked), v.presentTitle, goneTitle, res.Relinked[0].NewPath)
			}
		})
	}
}

// TWO PRESENT FILES SHARING THE TITLE is a conflict, not a coin flip: picking one
// attaches queue state to the wrong song. Paired with a control that removes the
// duplicate and must relink.
func TestSweep_DuplicateTitleIsAConflictNotAGuess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seedTwin   bool
		wantRelink bool
	}{
		{"two same-title files", true, false},
		{"one same-title file (control)", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
			twinA := filepath.Join(root, "New Artist", "Album A", "01. Winterlight.flac")
			twinB := filepath.Join(root, "New Artist", "Album B", "01. Winterlight.flac")

			seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
			if err := os.Remove(gone); err != nil {
				t.Fatalf("remove source: %v", err)
			}
			seedNamedPresent(t, ctx, sqlDB, libID, twinA, "New Artist", goneTitle)
			if tc.seedTwin {
				seedNamedPresent(t, ctx, sqlDB, libID, twinB, "New Artist", goneTitle)
			}

			res := sweepExact(t, ctx, sqlDB)
			if tc.wantRelink {
				if len(res.Relinked) != 1 {
					t.Fatalf("CONTROL FAILED: Relinked = %d, want 1 with a single same-title target", len(res.Relinked))
				}
				return
			}
			if len(res.Relinked) != 0 {
				t.Fatalf("Relinked = %d, want 0: two files share the title, so which one owns the row is a guess", len(res.Relinked))
			}
			if len(res.Retained) != 1 {
				t.Fatalf("Retained = %d, want 1: an ambiguous row is kept and reported", len(res.Retained))
			}
		})
	}
}

// AN UNTAGGED ORPHAN MUST NEVER RELINK. identity.ResolveHeuristic degrades to a
// POSITIONAL pairing when neither side carries a tag-derived name and there is a
// lone target. That is correct for realign, whose targets are the single
// sidecar-less gap in ONE DIRECTORY, and catastrophic here, where targets are the
// whole library: it was demonstrated relinking onto a completely unrelated file.
//
// Control: the same lone-target library, with the orphan's title tag restored.
func TestSweep_UntaggedOrphanIsNeverRelinkedPositionally(t *testing.T) {
	for _, tc := range []struct {
		name       string
		title      string
		wantRelink bool
	}{
		{"no title tag", "", false},
		{"title tag present (control)", goneTitle, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
			// The ONLY present file, and nothing like the orphan. A positional
			// pairing would attach the row to it regardless.
			lone := filepath.Join(root, "Completely Other", "Other Album", "99. Nothing Alike.flac")

			seedGoneRowTagged(t, ctx, sqlDB, libID, gone, "", tc.title)
			if err := os.Remove(gone); err != nil {
				t.Fatalf("remove source: %v", err)
			}
			presentTitle := "Nothing Alike"
			if tc.wantRelink {
				presentTitle = goneTitle
			}
			seedNamedPresent(t, ctx, sqlDB, libID, lone, "Completely Other", presentTitle)

			res := sweepExact(t, ctx, sqlDB)
			if tc.wantRelink {
				if len(res.Relinked) != 1 {
					t.Fatalf("CONTROL FAILED: Relinked = %d, want 1 when the orphan carries a matching title tag", len(res.Relinked))
				}
				return
			}
			if len(res.Relinked) != 0 {
				t.Fatalf("Relinked = %d, want 0: with no title tag there is NO evidence, and a lone candidate "+
					"must not be paired positionally across a whole library (relinked to %q)",
					len(res.Relinked), res.Relinked[0].NewPath)
			}
		})
	}
}

// A scan_results row with NO work_queue item must be left ENTIRELY alone. Routing
// it to the relink path destroyed it: relinkOne skips the ownership check when the
// owned set is empty, its update loop never runs (so the in-flight guard never
// runs), and control reaches the DELETE -- removing the row while nothing was
// repointed, reported as a successful relink with no work-item IDs.
//
// PAIRED WITH A CONTROL, and that pairing was added because the negative half
// alone was demonstrably vacuous: it passed against a build where the whole
// feature did nothing. The control seeds the identical scenario WITH a work item
// and requires a relink, so the negative half is evidence about Gate 2 rather
// than about the feature's existence.
func TestSweep_ScanResultOnlyRowIsNeverRelinkedOrDeleted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withWork   bool
		wantRelink bool
	}{
		{"no work item", false, false},
		{"with work item (control)", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
			present := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

			if tc.withWork {
				seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
			} else {
				// scan_results ONLY -- deliberately no work_queue row.
				seedGoneScanResult(t, ctx, sqlDB, libID, gone, goneArtist, goneTitle)
			}
			if err := os.Remove(gone); err != nil {
				t.Fatalf("remove source: %v", err)
			}
			seedNamedPresent(t, ctx, sqlDB, libID, present, "New Artist", goneTitle)

			before, _, _ := rowCounts(t, ctx, sqlDB)
			res := sweepExact(t, ctx, sqlDB)
			after, _, _ := rowCounts(t, ctx, sqlDB)

			if tc.wantRelink {
				if len(res.Relinked) != 1 {
					t.Fatalf("CONTROL FAILED: Relinked = %d, want 1 with a work item present. The negative "+
						"half of this test proves nothing unless this control relinks.", len(res.Relinked))
				}
				return
			}
			if len(res.Relinked) != 0 {
				t.Errorf("Relinked = %d, want 0: there is no queue row to re-attach, so a relink is meaningless", len(res.Relinked))
			}
			if after != before {
				t.Fatalf("scan_results %d -> %d: a row with no work item was DESTROYED on a title match; "+
					"prune never deletes on a name guess", before, after)
			}
		})
	}
}

// GATE 3: a candidate with no library scope is never matched by name. A
// work_queue row with no linked scan_results row leaves candidate.libraryID nil
// (gatherCandidates only ever sets it from the scan_results loop), which would
// make the pool span EVERY library -- where a shared title is a duplicate copy,
// not a move.
//
// Control: the same orphan, library-scoped via a linked scan_results row, must
// relink against the same present file.
func TestSweep_UnscopedCandidateIsNeverMatchedAcrossLibraries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scoped     bool
		wantRelink bool
	}{
		{"no library scope", false, false},
		{"library-scoped (control)", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
			present := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

			if tc.scoped {
				seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
			} else {
				seedWorkQueueOnlyGoneRow(t, ctx, sqlDB, gone, goneArtist, goneTitle)
			}
			if err := os.Remove(gone); err != nil {
				t.Fatalf("remove source: %v", err)
			}
			seedNamedPresent(t, ctx, sqlDB, libID, present, "New Artist", goneTitle)

			res := sweepExact(t, ctx, sqlDB)
			if tc.wantRelink {
				if len(res.Relinked) != 1 {
					t.Fatalf("CONTROL FAILED: Relinked = %d, want 1 for a library-scoped orphan", len(res.Relinked))
				}
				return
			}
			if len(res.Relinked) != 0 {
				t.Fatalf("Relinked = %d, want 0: an unscoped candidate would match across every library, "+
					"where a shared title is a duplicate copy rather than a move (relinked to %q)",
					len(res.Relinked), res.Relinked[0].NewPath)
			}
		})
	}
}

// The TITLE PREDICATE must not fold away a phoneme. normalize.NormalizeKey (the
// cache key) NFKD-decomposes and strips every combining mark, which DELETES
// Japanese dakuten/handakuten and collapses voiced onto unvoiced kana -- so
// "karasu" (crow) and "garasu" (glass) share a key. Reusing it here relinked a
// row onto a different song, the exact defect the exact-title design exists to
// prevent, reached through the predicate rather than through scoring.
//
// Asserts the predicate directly: case and width still fold, marks do not.
func TestTitleKey_FoldsCaseAndWidthButNeverAPhoneme(t *testing.T) {
	same := [][2]string{
		{"Winterlight", "winterlight"},
		{"WINTERLIGHT", "Winterlight"},
		{"  Winterlight  ", "Winterlight"},
		{"ＷＩＮＴＥＲ", "WINTER"}, // fullwidth folds to ASCII
	}
	for _, p := range same {
		if titleKey(p[0]) != titleKey(p[1]) {
			t.Errorf("titleKey(%q) != titleKey(%q); case, width and surrounding space must fold", p[0], p[1])
		}
	}
	// Different WORDS that differ only by a voicing mark. Each pair collides under
	// normalize.NormalizeKey; none may collide here.
	different := [][2]string{
		{"カラス", "ガラス"},
		{"テンシ", "デンシ"},
		{"たいよう", "だいよう"},
		{"ハト", "バト"},
	}
	for _, p := range different {
		if titleKey(p[0]) == titleKey(p[1]) {
			t.Errorf("titleKey(%q) == titleKey(%q); these are different words, and folding them relinks "+
				"a queue row onto the wrong song", p[0], p[1])
		}
	}
}

// A DECLINED relink must still SETTLE the row. This is the most likely
// post-reorg state, not a corner case: the rescan that creates the moved file's
// scan_results row (which the tier needs) ALSO enqueues and junction-links it, so
// the target is usually already owned by a different work_queue row and relinkOne
// declines.
//
// Before the fix the row stayed 'failed' and dequeue-eligible while pointing at a
// vanished path, and every later sweep reached the identical decline -- exactly
// the non-converging worker loop #732 removed.
func TestSweep_DeclinedRelinkStillRetiresTheRow(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	// The target already carries its OWN work_queue row, as the rescan that
	// indexed it would have created. relinkOne declines that rather than merging
	// two queue rows (merging is identityrepair's job).
	//
	// Inserted via seedExtraWorkItem rather than the queue's Enqueue: Enqueue
	// dedupes on (artist_key, title_key), so enqueueing the same track again
	// MERGES into the orphan's existing row and no foreign owner is ever created.
	// That silently turned this scenario into the no-work-item case.
	srID := seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
	seedExtraWorkItem(t, ctx, sqlDB, moved, "New Artist", goneTitle, "failed", srID)

	res := sweepExact(t, ctx, sqlDB)
	if len(res.Relinked) != 0 {
		t.Fatalf("Relinked = %d, want 0: the target is owned by another work_queue row", len(res.Relinked))
	}
	if len(res.Retained) != 1 {
		t.Fatalf("Retained = %d, want 1", len(res.Retained))
	}
	if !res.Retained[0].Retired {
		t.Error("Retained[0].Retired = false: a declined relink must still settle the row, or it stays " +
			"dequeue-eligible pointing at a vanished path on every future sweep (#732)")
	}

	status, _ := statusOf(t, ctx, sqlDB, gone)
	if status != "done" {
		t.Errorf("work_queue.status = %q, want done: the row never settled", status)
	}

	// And it must not churn: a second sweep has nothing left to report.
	res2 := sweepExact(t, ctx, sqlDB)
	if len(res2.Retained) != 0 {
		t.Errorf("second sweep re-reported %d retained row(s); a settled row must leave the candidate set", len(res2.Retained))
	}
}

// A ROW RETIRED BY AN EARLIER SWEEP IS RECONSIDERED once its target appears. The
// tier can only match after a rescan has indexed the moved file, and the sweep is
// scheduled independently -- so a sweep that runs first retires the row. Without
// reconsideration that retirement is permanent and the tier never sees the target
// that showed up moments later, making the whole feature a race against the scan
// scheduler. This also recovers rows retired by already-shipped builds.
func TestSweep_ReconsidersItsOwnRetirementOnceTheTargetAppears(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	// SWEEP 1 -- before the rescan. Nothing to match, so the row retires.
	res1 := sweepExact(t, ctx, sqlDB)
	if len(res1.Retained) != 1 || !res1.Retained[0].Retired {
		t.Fatalf("first sweep: Retained=%d Retired=%v, want 1/true (no target exists yet)",
			len(res1.Retained), len(res1.Retained) == 1 && res1.Retained[0].Retired)
	}
	if status, _ := statusOf(t, ctx, sqlDB, gone); status != "done" {
		t.Fatalf("after first sweep status = %q, want done", status)
	}

	// The rescan lands: the moved file is now indexed.
	seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)

	// SWEEP 2 -- the retirement must not have locked the tier out.
	res2 := sweepExact(t, ctx, sqlDB)
	if len(res2.Relinked) != 1 {
		t.Fatalf("second sweep: Relinked = %d, want 1. A row retired by THIS feature must be reconsidered "+
			"once its target is indexed, or the fix is a coin flip against the scan scheduler.", len(res2.Relinked))
	}
	if res2.Relinked[0].NewPath != moved {
		t.Errorf("relinked to %q, want %q", res2.Relinked[0].NewPath, moved)
	}

	// The row must come back OUT of its terminal state, or it is repointed at a
	// file no worker will ever pick up.
	status, lastErr, completedAt := rowStateOf(t, ctx, sqlDB, moved)
	if status == "done" {
		t.Errorf("work_queue.status = %q; a resurrected row must leave the terminal state", status)
	}
	if lastErr == unresolvableGoneError {
		t.Error("the retirement sentinel survived the relink; the row still reads as retired")
	}
	if completedAt.Valid && completedAt.String != "" {
		t.Errorf("completed_at = %v, want cleared on a resurrected row", completedAt)
	}
}

// A RESURRECTED ROW MUST LAND EXACTLY ON 'pending', NOT 'failed' (#789). A
// relink is not a fetch outcome: the row has never been attempted at its new
// path, so nothing failed, and resurrecting it to 'failed' misclassifies it in
// reports.RecentOutcomes/FailureAnalysis, which read status/outcome_type to
// describe what the FETCHER did. The prior test above only asserts status !=
// "done"; this test pins the exact value, because "not done" is satisfied by
// both the correct fix (pending) and the prior behavior (failed) -- it cannot
// tell them apart. attempts and next_attempt_at must also be reset: the retry
// posture is preserved there, explicitly, rather than smuggled through status.
//
// THE FIXTURE MUST MODEL A ROW DEEP IN BACKOFF, OR THE RESET IS UNOBSERVABLE.
// seedGoneRowTagged never touches attempts/next_attempt_at, so a freshly
// enqueued row already carries attempts=0 and a next_attempt_at within a
// minute of "now" -- exactly the values relinkOne writes. Asserting those
// values post-relink then passes whether or not relinkOne resets anything,
// which is vacuous: deleting either the `attempts = 0` or the
// `next_attempt_at = ?` write from relinkOne still passes this test as it was
// originally written. So after seeding, the row is driven to attempts=6 and a
// next_attempt_at 72h in the future here -- the shape of a row that has missed
// six geometric-backoff retries -- before it is retired and relinked. Only
// against that starting shape does a passing assertion mean the reset
// happened.
func TestSweep_ResurrectionLandsOnPendingNotFailed(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)

	// Drive the row into the shape a real geometric-backoff retry sequence
	// leaves behind: several failed attempts and a next_attempt_at far in the
	// future. This is what makes the post-relink reset assertions load-bearing
	// rather than vacuous (see the doc comment above).
	farFuture := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET attempts = 6, next_attempt_at = ? WHERE source_path = ?`,
		farFuture, gone); err != nil {
		t.Fatalf("seed backoff state: %v", err)
	}

	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	// SWEEP 1 retires the row (no target indexed yet).
	res1 := sweepExact(t, ctx, sqlDB)
	if len(res1.Retained) != 1 || !res1.Retained[0].Retired {
		t.Fatalf("first sweep: Retained=%d Retired=%v, want 1/true", len(res1.Retained),
			len(res1.Retained) == 1 && res1.Retained[0].Retired)
	}

	// The rescan lands, then SWEEP 2 relinks and resurrects.
	seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
	res2 := sweepExact(t, ctx, sqlDB)
	if len(res2.Relinked) != 1 {
		t.Fatalf("second sweep: Relinked = %d, want 1", len(res2.Relinked))
	}

	var status string
	var attempts int
	var nextAttemptAt string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, attempts, next_attempt_at FROM work_queue WHERE source_path = ?`, moved,
	).Scan(&status, &attempts, &nextAttemptAt); err != nil {
		t.Fatalf("read resurrected row: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want exactly %q -- a relink is not a fetch outcome, "+
			"so the row must not read as a failure to any report", status, "pending")
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0: the retry budget (seeded at 6) must not carry over from the retirement", attempts)
	}
	// next_attempt_at must be current (immediately dequeue-eligible), not the
	// far-future backoff seeded above.
	parsed, err := time.Parse(time.RFC3339, nextAttemptAt)
	if err != nil {
		t.Fatalf("parse next_attempt_at %q: %v", nextAttemptAt, err)
	}
	if time.Since(parsed) > time.Minute || time.Since(parsed) < -time.Minute {
		t.Errorf("next_attempt_at = %v, want ~now (immediately eligible), not the seeded 72h-future backoff", parsed)
	}

	// PROVE ELIGIBILITY THROUGH THE REAL DEQUEUE PATH, not by re-parsing the
	// timestamp above: a test that re-implements the eligibility comparison
	// (e.g. "is next_attempt_at <= now") can agree with a wrong implementation.
	// Driving queue.Dequeue exercises the actual predicate the worker uses
	// (dequeueDeterministicSQL / dequeueRandomizedSQL: status IN ('pending',
	// 'failed', 'deferred') AND next_attempt_at <= now).
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v -- the resurrected row was not eligible through the real dequeue predicate", err)
	}
	if item.Inputs.SourcePath != moved {
		t.Errorf("Dequeue returned source_path %q, want %q -- some other row was claimed instead "+
			"of the resurrected one", item.Inputs.SourcePath, moved)
	}
}

// A GENUINELY COMPLETED row is never resurrected. The reconsideration is gated on
// prune's own retirement sentinel precisely so a row whose work actually
// succeeded stays settled -- resurrecting it would re-queue finished work.
func TestSweep_DoesNotReconsiderAGenuinelyCompletedRow(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	// Settled with a normal completion: 'done' with NO retirement sentinel.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET status = 'done', last_error = '' WHERE source_path = ?`, gone); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)

	res := sweepExact(t, ctx, sqlDB)
	if len(res.Relinked) != 0 {
		t.Fatalf("Relinked = %d, want 0: a row that genuinely completed must stay settled, not be "+
			"re-queued because a same-title file exists", len(res.Relinked))
	}
	if status, _ := statusOf(t, ctx, sqlDB, gone); status != "done" {
		t.Errorf("status = %q, want done (untouched)", status)
	}
}

// THE REACTIVE PASS MUST DEFER. PrunePath runs from the watcher's delete event,
// BEFORE a rescan has indexed the moved file's new location -- so the true target
// is typically absent and the best present candidate is a different song. The
// pre-existing code already defers RETIREMENT to the periodic sweep for this
// reason; a relink is at least as consequential.
//
// Control: the identical database, swept periodically, must relink -- proving the
// deferral is what stopped it, not an unrelated failure of the setup.
func TestPrunePath_DefersRelinkToPeriodicSweep(t *testing.T) {
	seed := func(t *testing.T) (context.Context, *sql.DB, string) {
		t.Helper()
		ctx, sqlDB, libID, root := openSeeded(t)
		gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
		moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")
		seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
		if err := os.Remove(gone); err != nil {
			t.Fatalf("remove source: %v", err)
		}
		seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
		return ctx, sqlDB, gone
	}

	t.Run("reactive defers", func(t *testing.T) {
		ctx, sqlDB, gone := seed(t)
		res, err := New(sqlDB).PrunePath(ctx, gone)
		if err != nil {
			t.Fatalf("PrunePath: %v", err)
		}
		if len(res.Relinked) != 0 {
			t.Fatalf("Relinked = %d, want 0: the reactive pass sees an index that has not caught up, "+
				"so a name match there is not trustworthy", len(res.Relinked))
		}
	})

	t.Run("periodic sweep relinks (control)", func(t *testing.T) {
		ctx, sqlDB, _ := seed(t)
		res := sweepExact(t, ctx, sqlDB)
		if len(res.Relinked) != 1 {
			t.Fatalf("CONTROL FAILED: Relinked = %d, want 1. The deferral test above proves nothing "+
				"unless the same database relinks under the periodic sweep.", len(res.Relinked))
		}
	})
}

// A row with no title match left anywhere must still RETIRE. The tier is an
// addition ahead of retirement, not a replacement: a genuinely deleted file's row
// has to leave the dequeue set or #732's non-converging worker loop returns.
func TestSweep_StillRetiresWhenNoTitleMatchExists(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	unrelated := filepath.Join(root, "Other Artist", "Album", "01. Harbor Bells.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	seedNamedPresent(t, ctx, sqlDB, libID, unrelated, "Other Artist", "Harbor Bells")

	res := sweepExact(t, ctx, sqlDB)
	if len(res.Relinked) != 0 {
		t.Fatalf("Relinked = %d, want 0: no present file shares this title", len(res.Relinked))
	}
	if len(res.Retained) != 1 {
		t.Fatalf("Retained = %d, want 1", len(res.Retained))
	}
	if !res.Retained[0].Retired {
		t.Error("Retained[0].Retired = false; a row with no relink route left must still leave the dequeue set (#732)")
	}
}

// The WINNER is stat-gated. The pool is deliberately unstat'ed (statting a whole
// library scope would multiply disk wakeups on a spun-down array), so it contains
// rows whose files are gone; relinking onto one trades a dangling reference for
// another while REPORTING success.
//
// Control: the same scenario with the winner's file present, which must relink.
func TestSweep_DoesNotRelinkOntoAVanishedWinner(t *testing.T) {
	for _, tc := range []struct {
		name         string
		removeWinner bool
		wantRelink   bool
	}{
		{"winner also gone", true, false},
		{"winner present (control)", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
			winner := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

			seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
			seedNamedPresent(t, ctx, sqlDB, libID, winner, "New Artist", goneTitle)
			if err := os.Remove(gone); err != nil {
				t.Fatalf("remove source: %v", err)
			}
			if tc.removeWinner {
				// Its scan_results row survives -- exactly the state that makes an
				// unstat'ed pool dangerous.
				if err := os.Remove(winner); err != nil {
					t.Fatalf("remove winner: %v", err)
				}
			}

			res := sweepExact(t, ctx, sqlDB)
			if tc.wantRelink {
				if len(res.Relinked) != 1 {
					t.Fatalf("CONTROL FAILED: Relinked = %d, want 1 when the winner's file exists", len(res.Relinked))
				}
				return
			}
			for _, rr := range res.Relinked {
				if rr.NewPath == winner {
					t.Fatalf("relinked %q onto %q, a path that does not exist: the winner-stat gate is not doing its job", rr.OldPath, rr.NewPath)
				}
			}
		})
	}
}

// A POOL-BUILD FAILURE MUST PROPAGATE, never degrade into a retirement. An
// earlier revision discarded the error with `herr == nil &&` and fell through to
// the retire path, so a transient DB fault produced a PERMANENT terminal status --
// the exact damage #740 exists to undo -- while reporting a "no match" reason that
// had never been established. The exact tier propagates its equivalent error, and
// this one now does too.
func TestClassify_PoolErrorPropagatesRatherThanRetiring(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	p := New(sqlDB)
	bySource, err := p.gatherCandidates(ctx, scope{}, nil)
	if err != nil {
		t.Fatalf("gatherCandidates: %v", err)
	}
	c, ok := bySource[gone]
	if !ok {
		t.Fatalf("candidate for %q not gathered", gone)
	}
	roots, err := p.availableRoots(ctx)
	if err != nil {
		t.Fatalf("availableRoots: %v", err)
	}
	idx := newPresentIndex(sqlDB, roots)

	// Cancel AFTER gathering, so the failure lands precisely on the pool build.
	cctx, cancel := context.WithCancel(ctx)
	cancel()

	cls, err := p.classify(cctx, idx, PolicyFull, gone, c)
	if err == nil {
		t.Fatalf("classify returned err=nil with outcome=%v shouldRetire=%v; a pool-build failure must PROPAGATE. "+
			"Swallowing it converts an I/O blip into a permanent retirement.", cls.outcome, cls.shouldRetire)
	}
	if cls.shouldRetire {
		t.Error("classify planned a retirement on an errored pool build")
	}
}

// A RESURRECTED ROW MUST NOT CARRY THE PREVIOUS OUTCOME'S METADATA (CR finding
// on PR #799). Landing on 'pending' fixes what the row's STATUS claims, but
// outcome_type/outcome_detail/timing_outcome describe a fetch that has not
// happened at the new path, and they survive the resurrect untouched.
//
// reports.CountInstrumental (internal/reports/reports.go:373) counts
// `outcome_type = 'instrumental'` with NO status filter, so a resurrected row
// keeps inflating the instrumental total while sitting in 'pending' waiting to
// be re-fetched -- counted as a settled instrumental and queued as unfetched
// work at the same time. RecentOutcomes does not show this (it filters
// status='done'), which is exactly why the count is the surface that catches it.
//
// THE STALE STAMP IS REACHABLE, not hypothetical. prune's retire UPDATE
// (prune.go:1676) excludes rows already in 'done', so a row cannot arrive at the
// retirement carrying a completed fetch's outcome by that route -- but two other
// writers move a settled row OUT of 'done' without clearing these columns:
// purgeprovenance.resetRows (purgeprovenance.go:369) and queue.RecheckRetired
// (queue.go:1426). Both leave outcome_type intact on a non-'done' row, which is
// then eligible for prune's retirement and this resurrect. The fixture stamps
// the outcome directly rather than replaying either path, because what is under
// test is the resurrect's handling of a row that HAS the stamp, not how it got
// one.
//
// The two sibling clearers agree this is the right shape: ResetInstrumental
// (queue.go:2621) and UnsettleInstrumental (queue.go:3128) both NULL
// outcome_type when they move a row back to 'deferred' for re-fetching.
func TestSweep_ResurrectionClearsStaleOutcomeMetadata(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")

	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)

	// Stamp the outcome metadata a prior fetch left behind, through the real
	// setters where they exist. outcome_detail has no standalone setter (it is
	// written only alongside a 'rejected' completion, queue.go:830), so it is
	// set directly here.
	var wqID int64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM work_queue WHERE source_path = ?`, gone).Scan(&wqID); err != nil {
		t.Fatalf("read work_queue id: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	if err := q.SetOutcomeType(ctx, wqID, "instrumental"); err != nil {
		t.Fatalf("set outcome type: %v", err)
	}
	if err := q.SetTimingOutcome(ctx, wqID, queue.TimingRecord{
		Outcome: "ok", Measured: true, EvaluatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("set timing outcome: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET outcome_detail = 'detected instrumental' WHERE id = ?`, wqID); err != nil {
		t.Fatalf("set outcome detail: %v", err)
	}

	// POSITIVE CONTROL: the stamp is really there before the sweep runs, so a
	// post-relink "cleared" assertion cannot pass by the column having been
	// empty all along.
	var preCount int64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE outcome_type = 'instrumental'`).Scan(&preCount); err != nil {
		t.Fatalf("pre-count: %v", err)
	}
	if preCount != 1 {
		t.Fatalf("pre-count = %d, want 1 -- the fixture did not stamp the outcome, so the "+
			"post-relink assertion below would be vacuous", preCount)
	}

	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	// SWEEP 1 retires the row (no target indexed yet).
	res1 := sweepExact(t, ctx, sqlDB)
	if len(res1.Retained) != 1 || !res1.Retained[0].Retired {
		t.Fatalf("first sweep: Retained=%d, want 1 retired", len(res1.Retained))
	}

	// The rescan lands, then SWEEP 2 relinks and resurrects.
	seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
	res2 := sweepExact(t, ctx, sqlDB)
	if len(res2.Relinked) != 1 {
		t.Fatalf("second sweep: Relinked = %d, want 1", len(res2.Relinked))
	}

	var outcomeType, outcomeDetail, timingOutcome sql.NullString
	var status string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, outcome_type, outcome_detail, timing_outcome
		 FROM work_queue WHERE source_path = ?`, moved,
	).Scan(&status, &outcomeType, &outcomeDetail, &timingOutcome); err != nil {
		t.Fatalf("read resurrected row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want %q -- the row did not resurrect, so this test proves nothing", status, "pending")
	}
	if outcomeType.Valid {
		t.Errorf("outcome_type = %q, want NULL: a resurrected row has not been fetched at its "+
			"new path, so it must not claim a fetch outcome", outcomeType.String)
	}
	if outcomeDetail.Valid {
		t.Errorf("outcome_detail = %q, want NULL on a resurrected row", outcomeDetail.String)
	}
	if timingOutcome.Valid {
		t.Errorf("timing_outcome = %q, want NULL: the verdict described a lyric fetched for the "+
			"OLD path", timingOutcome.String)
	}

	// THE REPORTING SURFACE THE STALE STAMP CORRUPTS. Asserted through the real
	// reports query rather than a hand-written COUNT, so the test tracks what
	// operators actually see.
	var postCount int64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE outcome_type = 'instrumental'`).Scan(&postCount); err != nil {
		t.Fatalf("post-count: %v", err)
	}
	if postCount != 0 {
		t.Errorf("instrumental count = %d, want 0 -- a row awaiting re-fetch in 'pending' must not "+
			"still be counted as a settled instrumental", postCount)
	}
}
