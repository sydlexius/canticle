package purgeprovenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/normalize"
)

const (
	// The empty cue is one the edit path writes back as a glyph.
	genOriginal  = "[source:lane-a]\n[00:01.00]first line\n[00:03.00]\n[00:05.00]second line\n"
	genCompanion = "[by:canticle]\n[source:lane-a]\n[timing:canticle-aligner]\n[00:01.40]<00:01.40>first <00:01.90>line\n"
	// The fixture's settled row before a restore, and after one (genRow).
	genUntouched = "done ok done cache=1 word edited=1 offset=1"
	genRestored  = "done - done cache=1 line edited=0 offset=0"
)

// gen is one accepted retiming made by the real edit path, with its row.
type gen struct {
	ctx             context.Context
	db              *sql.DB
	root, lrc, elrc string
	id              int64
}

// genFixture writes original as the .lrc, seeds a settled row (edit mark,
// offset, word tier, sweep verdict, cache entry), then accepts a retiming.
func genFixture(t *testing.T, original, wqStatus string) gen {
	t.Helper()
	ctx, sqlDB, libID, root := openSeeded(t)
	g := gen{ctx: ctx, db: sqlDB, root: root, lrc: filepath.Join(root, "Album", "track.lrc"), elrc: filepath.Join(root, "Album", "track.elrc")}
	if err := os.MkdirAll(filepath.Dir(g.lrc), 0o755); err != nil {
		t.Fatal(err)
	}
	g.write(t, g.lrc, original)
	_, g.id = seedTrack(t, ctx, sqlDB, libID, filepath.Dir(g.lrc), "track.lrc", wqStatus)
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET lyric_edited_at = '2026-01-02T03:04:05Z', lyric_offset_ms = 250, sync_tier = 'word', timing_outcome = 'ok',
		     timing_stamp_source = 'sweep', provider_lane = 'lane-b', source_path = ?`, filepath.Join(root, "Album", "track.flac")); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	// The cache entry a --source purge of this row would invalidate.
	if err := cache.New(sqlDB).Store(ctx, "Artist", "Title", normalize.DurationBucket(0), original); err != nil {
		t.Fatalf("cache store: %v", err)
	}
	g.accept(t)
	return g
}

func (g gen) write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// accept retimes the .lrc through lyrics.ApplyEdit; the first saves the .orig.
func (g gen) accept(t *testing.T) {
	t.Helper()
	lines := lyrics.ParseTimedLRC(mustRead(t, g.lrc)).Lines
	for i := range lines {
		lines[i].StartMS += 400
	}
	if _, err := lyrics.ApplyEdit(g.lrc, lines, nil, lyrics.EditOptions{Roots: []string{g.root}, Generated: &lyrics.GeneratedEdit{}}); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func (g gen) run(t *testing.T, dry bool, report func(Record) error) Result {
	t.Helper()
	res, err := New(g.db).Run(g.ctx, Options{Roots: []string{g.root}, Filter: Filter{Generated: true}, DryRun: dry, Report: report})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// row reads the columns a restore may and may not move, as one string.
func (g gen) row(t *testing.T) (row string) {
	t.Helper()
	if err := g.db.QueryRowContext(g.ctx,
		`SELECT status || ' ' || COALESCE(timing_outcome, '-') || ' ' || (SELECT status FROM scan_results) || ' cache=' || (SELECT COUNT(*) FROM lyrics_cache)
		     || ' ' || sync_tier || ' edited=' || (lyric_edited_at IS NOT NULL) || ' offset=' || (lyric_offset_ms IS NOT NULL) FROM work_queue WHERE id = ?`, g.id).Scan(&row); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return row
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A dry run reports the file and its generated companion and changes nothing.
func TestRunGenerated_DryRunChangesNothing(t *testing.T) {
	g := genFixture(t, genOriginal, "done")
	g.write(t, g.elrc, genCompanion)
	retimed := mustRead(t, g.lrc)
	var reported []string
	res := g.run(t, true, func(r Record) error { reported = append(reported, r.Path); return nil })
	if res.Matched != 1 || res.Restored != 0 || res.CompanionsDeleted != 0 || res.SkippedNoOriginal != 0 || res.RestoredNoRow != 0 {
		t.Fatalf("result = %+v; want one match and nothing applied", res)
	}
	if len(reported) != 2 || reported[0] != g.lrc {
		t.Fatalf("reported %v; want the .lrc then its companion", reported)
	}
	if got := mustRead(t, g.lrc); got != retimed || !exists(g.lrc+".orig") || !exists(g.elrc) {
		t.Errorf("dry run changed the .lrc (%q), its .orig or the companion", got)
	}
	if got := g.row(t); got != genUntouched {
		t.Errorf("dry run moved the row: %q", got)
	}
}

// Apply puts the provider's bytes back, consumes the .orig, removes the
// generated companion and clears the mark, the offset and the timing verdict;
// nothing is re-fetched, whatever lane the row credits.
func TestRunGenerated_ApplyRestores(t *testing.T) {
	g := genFixture(t, genOriginal, "done")
	g.write(t, g.elrc, genCompanion)
	retimed, backedUp := mustRead(t, g.lrc), map[string]string{}
	res := g.run(t, false, func(r Record) error { backedUp[r.Path] = mustRead(t, r.Path); return nil })
	if res.Restored != 1 || res.CompanionsDeleted != 1 || res.Errors != 0 || res.RestoredNoRow != 0 {
		t.Fatalf("result = %+v; want one restore with its companion", res)
	}
	if res.Deleted != 0 || res.WorkItemsRequeued != 0 || res.ScanResultsReset != 0 || res.CacheInvalidated != 0 {
		t.Fatalf("result = %+v; a restore deletes, requeues and invalidates nothing", res)
	}
	if got := mustRead(t, g.lrc); got != genOriginal || retimed == genOriginal {
		t.Errorf(".lrc = %q; want the original bytes", got)
	}
	if exists(g.lrc+".orig") || exists(g.elrc) {
		t.Error("the .orig or the generated companion survived the restore")
	}
	if backedUp[g.lrc] != retimed {
		t.Errorf("backup saw %q; want the generated bytes, captured before the restore", backedUp[g.lrc])
	}
	var src sql.NullString
	if err := g.db.QueryRowContext(g.ctx, `SELECT timing_stamp_source FROM work_queue`).Scan(&src); err != nil || src.Valid {
		t.Errorf("timing_stamp_source = %+v (%v); want NULL with the verdict", src, err)
	}
	if got := g.row(t); got != genRestored {
		t.Errorf("row = %q; want %q", got, genRestored)
	}
}

// A marked file with nothing to restore from is counted and never deleted,
// whether the .orig is missing or is not a regular file.
func TestRunGenerated_NoOriginalIsSkipped(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		g := genFixture(t, genOriginal, "done")
		retimed, other := mustRead(t, g.lrc), filepath.Join(g.root, "elsewhere")
		if err := os.Rename(g.lrc+".orig", other); err != nil {
			t.Fatal(err)
		}
		if symlink {
			if err := os.Symlink(other, g.lrc+".orig"); err != nil {
				t.Skipf("symlink: %v", err)
			}
		}
		reports := 0
		res := g.run(t, false, func(Record) error { reports++; return nil })
		if res.SkippedNoOriginal != 1 || res.Restored != 0 || reports != 0 {
			t.Fatalf("symlink=%v: result = %+v, reports = %d; want one no-original skip", symlink, res, reports)
		}
		if got := mustRead(t, g.lrc); got != retimed || g.row(t) != genUntouched {
			t.Errorf("symlink=%v: the .lrc (%q) or its row (%q) changed", symlink, got, g.row(t))
		}
	}
}

// A .orig that is not the lyric the .lrc holds now (re-fetched after the
// backup, then retimed) is never restored and nothing moves, dry run or apply.
func TestRunGenerated_StaleOriginalIsSkipped(t *testing.T) {
	for name, refetched := range map[string]string{
		"other words":  "[source:lane-a]\n[00:01.00]first line\n[00:03.00]\n[00:05.00]a different line\n",
		"other source": strings.Replace(genOriginal, "lane-a", "lane-c", 1),
		"fewer cues":   "[source:lane-a]\n[00:01.00]first line\n[00:03.00]\n",
		"other fetch":  "[fetched:2026-02-02T00:00:00Z]\n" + genOriginal,
	} {
		orig := strings.Replace(genOriginal, "\n", "\n[fetched:2026-01-01T00:00:00Z]\n", 1)
		for _, dry := range []bool{true, false} {
			g := genFixture(t, orig, "done")
			g.write(t, g.lrc, refetched)
			g.accept(t)
			retimed, stamp := mustRead(t, g.lrc), func(p string) string {
				fi, err := os.Lstat(p)
				return fmt.Sprint(fi.ModTime(), err)
			}
			before, reports := stamp(g.lrc)+stamp(g.lrc+".orig"), 0
			res := g.run(t, dry, func(Record) error { reports++; return nil })
			if res.SkippedOriginalDiffers != 1 || res.Restored != 0 || res.Errors != 0 || reports != 0 {
				t.Fatalf("%s dry=%v: result = %+v, reports = %d; want one original-differs skip and no backup record", name, dry, res, reports)
			}
			if mustRead(t, g.lrc) != retimed || mustRead(t, g.lrc+".orig") != orig || stamp(g.lrc)+stamp(g.lrc+".orig") != before {
				t.Errorf("%s dry=%v: the .lrc or its stale .orig was touched", name, dry)
			}
			if got := g.row(t); got != genUntouched {
				t.Errorf("%s dry=%v: row = %q for a file that was not restored", name, dry, got)
			}
		}
	}
}

// The #483 editor tag and a backfilled [source:] added in place after the .orig
// was saved do not make the backup stale, and the restored file gets both back.
func TestRunGenerated_EditorTagGainedAfterTheBackup(t *testing.T) {
	g := genFixture(t, "[by:canticle]\n[ve:1.2.3]\n[00:01.00]first line\n", "done")
	if ok, err := lyrics.InjectEditorTag(g.lrc); err != nil || !ok {
		t.Fatalf("InjectEditorTag = %v, %v; want the tag added to the generated file", ok, err)
	}
	if n, _, err := lyrics.InjectProvenance(g.lrc, lyrics.ProvenanceTags{Source: "lane-a"}); err != nil || n != 1 {
		t.Fatalf("InjectProvenance = %d, %v; want [source:] added to the generated file", n, err)
	}
	if res := g.run(t, false, nil); res.Restored != 1 || res.SkippedOriginalDiffers != 0 {
		t.Fatalf("result = %+v; want the file restored despite the added [re:canticle]", res)
	}
	if got, want := mustRead(t, g.lrc), "[by:canticle]\n[re:canticle]\n[ve:1.2.3]\n[source:lane-a]\n[00:01.00]first line\n"; got != want {
		t.Errorf("restored .lrc = %q; want %q", got, want)
	}
}

// A file no scan_results row links is still unmarked through the row whose
// source_path is its audio; with no such row it is restored and counted, and
// with that row in flight it is skipped.
func TestRunGenerated_UnlinkedRow(t *testing.T) {
	for _, tc := range []struct {
		stmt, want  string
		noRow, busy int
	}{
		{``, genRestored, 0, 0},
		{`; UPDATE work_queue SET source_path = 'elsewhere.flac'`, genUntouched, 1, 0},
		{`; UPDATE work_queue SET status = 'processing'`, "processing" + genUntouched[4:], 0, 1},
	} {
		g := genFixture(t, genOriginal, "done")
		if _, err := g.db.ExecContext(g.ctx, `DELETE FROM work_queue_scan_results`+tc.stmt); err != nil {
			t.Fatal(err)
		}
		if res := g.run(t, true, nil); res.RestoredNoRow != tc.noRow || res.SkippedProcessing != tc.busy {
			t.Errorf("%q: dry run = %+v; want %d without a row, %d in flight", tc.stmt, res, tc.noRow, tc.busy)
		}
		res := g.run(t, false, nil)
		if res.Restored != 1-tc.busy || res.RestoredNoRow != tc.noRow || res.SkippedProcessing != tc.busy || (mustRead(t, g.lrc) == genOriginal) != (tc.busy == 0) {
			t.Fatalf("%q: result = %+v; want %d restored, %d without a row, %d in flight", tc.stmt, res, 1-tc.busy, tc.noRow, tc.busy)
		}
		if got := g.row(t); got != tc.want {
			t.Errorf("%q: row = %q; want %q", tc.stmt, got, tc.want)
		}
	}
}

// Only a .lrc carrying the marker is selected: an unmarked hand edit with a
// .orig, and a .txt that happens to carry the tag, are left alone.
func TestRunGenerated_SelectsOnlyMarkedLRC(t *testing.T) {
	g := genFixture(t, genOriginal, "done")
	g.write(t, g.lrc, "[source:lane-a]\n[00:02.00]first line\n")
	txt := filepath.Join(g.root, "Album", "other.txt")
	g.write(t, txt, "[timing:canticle-aligner]\nwords\n")
	g.write(t, txt+".orig", "words\n")
	if res := g.run(t, false, nil); res.Scanned != 2 || res.Matched != 0 || res.Restored != 0 {
		t.Fatalf("result = %+v; want two files scanned and none selected", res)
	}
	if !exists(g.lrc+".orig") || !exists(txt+".orig") || g.row(t) != genUntouched {
		t.Errorf("an unselected file's .orig was consumed or its row moved: %q", g.row(t))
	}
}

// Refusals that leave a matched file as it was: a row in flight at the index
// read, one claimed after it, and a backup record that could not be written.
func TestRunGenerated_RefusalsLeaveTheFile(t *testing.T) {
	for _, tc := range []struct {
		name, status  string
		claim, noStat bool
		reportErr     error
		check         func(Result) bool
	}{
		{"in flight", "processing", false, false, nil, func(r Result) bool { return r.SkippedProcessing == 1 && r.Errors == 0 }},
		{"claimed mid-run", "done", true, false, nil, func(r Result) bool { return r.SkippedProcessing == 1 && r.Errors == 0 }},
		{"backup failed", "done", false, false, errors.New("disk full"), func(r Result) bool { return r.Errors == 1 }},
		{"original unreadable", "done", false, true, nil, func(r Result) bool { return r.Errors == 1 && r.SkippedNoOriginal == 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := genFixture(t, genOriginal, tc.status)
			retimed, want := mustRead(t, g.lrc), tc.status+genUntouched[4:]
			if t.Cleanup(func() { lstatFile = os.Lstat }); tc.noStat {
				lstatFile = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
			}
			res := g.run(t, false, func(Record) error {
				if tc.claim {
					want = "processing" + genUntouched[4:]
					if _, err := g.db.ExecContext(g.ctx, `UPDATE work_queue SET status = 'processing'`); err != nil {
						t.Fatal(err)
					}
				}
				return tc.reportErr
			})
			if res.Restored != 0 || !tc.check(res) {
				t.Fatalf("result = %+v", res)
			}
			if got := mustRead(t, g.lrc); got != retimed || !exists(g.lrc+".orig") {
				t.Errorf("the file or its .orig changed: %q", got)
			}
			if got := g.row(t); got != want {
				t.Errorf("row = %q without a restore; want %q", got, want)
			}
		})
	}
}

// The mark is cleared BEFORE the files move: a failed companion delete or
// rename is an error that leaves both files for the next run to finish.
func TestRunGenerated_FileFailureAfterTheMarkConverges(t *testing.T) {
	for _, failRename := range []bool{true, false} {
		g := genFixture(t, genOriginal, "done")
		g.write(t, g.elrc, genCompanion)
		retimed, prevRemove, prevRename := mustRead(t, g.lrc), removeFile, renameFile
		t.Cleanup(func() { removeFile, renameFile = prevRemove, prevRename })
		if failRename {
			renameFile = func(string, string) error { return os.ErrPermission }
		} else {
			removeFile = func(string) error { return os.ErrPermission }
		}
		if res := g.run(t, false, nil); res.Errors != 1 || res.Restored != 0 {
			t.Fatalf("failRename=%v: result = %+v; want one error and no restore", failRename, res)
		}
		if got := g.row(t); got != genRestored {
			t.Errorf("failRename=%v: row = %q; want the mark cleared before the files move", failRename, got)
		}
		if mustRead(t, g.lrc) != retimed || mustRead(t, g.lrc+".orig") != genOriginal {
			t.Errorf("failRename=%v: the generated file or its .orig was lost", failRename)
		}
		removeFile, renameFile = prevRemove, prevRename
		if res := g.run(t, false, nil); res.Restored != 1 || res.Errors != 0 || mustRead(t, g.lrc) != genOriginal {
			t.Fatalf("failRename=%v: rerun = %+v; want the restore finished", failRename, res)
		}
	}
}

// A companion the accept did not write stays: an owned one without the marker
// (and the word tier it gives the row), and a marked one canticle does not own.
func TestRunGenerated_KeepsACompanionItDidNotWrite(t *testing.T) {
	for body, tier := range map[string]string{
		"[by:canticle]\n[source:lane-a]\n[00:01.00]<00:01.00>first <00:01.50>line\n": "word",
		strings.TrimPrefix(genCompanion, "[by:canticle]\n"):                          "line",
	} {
		g := genFixture(t, genOriginal, "done")
		g.write(t, g.elrc, body)
		if res := g.run(t, false, nil); res.Restored != 1 || res.CompanionsDeleted != 0 {
			t.Fatalf("result = %+v; want a restore that keeps the companion", res)
		}
		if got := mustRead(t, g.elrc); got != body || mustRead(t, g.lrc) != genOriginal {
			t.Errorf("companion = %q; want it untouched beside the restored .lrc", got)
		}
		if got, want := g.row(t), strings.Replace(genRestored, "line", tier, 1); got != want {
			t.Errorf("row = %q; want %q", got, want)
		}
	}
}

// A --library run that finds the row only by its audio path restores the file
// only when the row's scan links prove it is that library's: never when the row
// is linked to another library alone, nor when it has no link at all.
func TestRunGenerated_LibraryScopeBoundsTheSourcePathFallback(t *testing.T) {
	for _, tc := range []struct {
		name, stmt string
		restored   bool
	}{
		// Control: linked to the scoped library under another outdir, so the
		// index misses it and the fallback finds it, in scope.
		{"linked here", `UPDATE scan_results SET outdir = 'elsewhere'`, true},
		{"linked to another library", `UPDATE scan_results SET library_id = (SELECT MAX(id) FROM libraries)`, false},
		{"linked to both", `UPDATE scan_results SET outdir = 'elsewhere';
		    INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status)
		        SELECT MAX(id), 'b', 'Artist', 'Title', 'b', 'b.lrc', 'done' FROM libraries;
		    INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) SELECT MIN(id), (SELECT MAX(id) FROM scan_results) FROM work_queue`, false},
		{"not linked", `DELETE FROM work_queue_scan_results`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := genFixture(t, genOriginal, "done")
			var libA int64
			if err := g.db.QueryRowContext(g.ctx, `SELECT MIN(id) FROM libraries`).Scan(&libA); err != nil {
				t.Fatal(err)
			}
			if _, err := g.db.ExecContext(g.ctx, `INSERT INTO libraries (path, name) VALUES ('/other', 'other')`); err != nil {
				t.Fatal(err)
			}
			if _, err := g.db.ExecContext(g.ctx, tc.stmt); err != nil {
				t.Fatal(err)
			}
			retimed := mustRead(t, g.lrc)
			res, err := New(g.db).Run(g.ctx, Options{Roots: []string{g.root}, LibraryID: &libA, Filter: Filter{Generated: true}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if tc.restored {
				if res.Restored != 1 || res.SkippedOtherLibrary != 0 || g.row(t) != genRestored {
					t.Fatalf("result = %+v, row = %q; want the in-scope row unmarked and the file restored", res, g.row(t))
				}
				return
			}
			if res.SkippedOtherLibrary != 1 || res.Restored != 0 || res.Errors != 0 {
				t.Fatalf("result = %+v; want one other-library skip and nothing restored", res)
			}
			if got := g.row(t); got != genUntouched {
				t.Errorf("row = %q; a --library run moved a row it cannot show is its own", got)
			}
			if mustRead(t, g.lrc) != retimed || !exists(g.lrc+".orig") {
				t.Error("the generated file or its .orig changed")
			}
		})
	}
}

// An entry swapped in at the .orig name after it was judged (here a symlink,
// in the backup callback) is never installed: the .lrc gets the bytes that were
// judged, as a regular file, and the swapped entry is left and counted.
func TestRunGenerated_SwappedOriginalIsNotInstalled(t *testing.T) {
	g := genFixture(t, genOriginal, "done")
	evil := filepath.Join(g.root, "evil")
	g.write(t, evil, "[00:01.00]not the lyric\n")
	res := g.run(t, false, func(Record) error {
		if err := os.Remove(g.lrc + ".orig"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(evil, g.lrc+".orig"); err != nil {
			t.Skipf("symlink: %v", err)
		}
		return nil
	})
	fi, err := os.Lstat(g.lrc)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf(".lrc mode = %v (%v); want a regular file, never the swapped-in entry", fi.Mode(), err)
	}
	if got := mustRead(t, g.lrc); got != genOriginal {
		t.Errorf(".lrc = %q; want the judged original bytes", got)
	}
	if res.Restored != 1 || res.Errors != 1 {
		t.Errorf("result = %+v; want the restore and one error for the changed .orig", res)
	}
	if ofi, err := os.Lstat(g.lrc + ".orig"); err != nil || ofi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the swapped .orig was not left in place: %v, %v", ofi, err)
	}
}

// The edit-mark clear's status guard is in the UPDATE itself: a row in flight
// makes the whole clear busy and rolls back the rows already cleared.
func TestClearEditMarks_InFlightRowRollsBackTheBatch(t *testing.T) {
	g := genFixture(t, genOriginal, "done")
	var libID int64
	if err := g.db.QueryRowContext(g.ctx, `SELECT MIN(id) FROM libraries`).Scan(&libID); err != nil {
		t.Fatal(err)
	}
	_, busyID := seedTrack(t, g.ctx, g.db, libID, filepath.Join(g.root, "Other"), "other.lrc", "processing")
	if _, err := g.db.ExecContext(g.ctx, `UPDATE work_queue SET lyric_edited_at = '2026-01-02T03:04:05Z' WHERE id = ?`, busyID); err != nil {
		t.Fatal(err)
	}
	busy, err := New(g.db).clearEditMarks(g.ctx, []int64{g.id, busyID}, false)
	if err != nil || !busy {
		t.Fatalf("clearEditMarks = busy %v, %v; want busy for the in-flight row", busy, err)
	}
	var edited int
	if err := g.db.QueryRowContext(g.ctx, `SELECT COUNT(*) FROM work_queue WHERE lyric_edited_at IS NOT NULL`).Scan(&edited); err != nil {
		t.Fatal(err)
	}
	if edited != 2 || g.row(t) != genUntouched {
		t.Errorf("%d rows still marked, row = %q; want both marks kept (the batch rolled back)", edited, g.row(t))
	}
	if busy, err := New(g.db).clearEditMarks(g.ctx, []int64{g.id, 1 << 40}, false); err != nil || busy || g.row(t) != genRestored {
		t.Errorf("clearEditMarks(done, vanished) = busy %v, %v, row %q; want the done row cleared and the vanished one ignored", busy, err, g.row(t))
	}
}

// A tag re-add that fails after the restore is an error (the CLI exits
// non-zero), though the restore itself stands.
func TestRunGenerated_TagReaddFailureIsAnError(t *testing.T) {
	for _, failEditor := range []bool{true, false} {
		g := genFixture(t, genOriginal, "done")
		t.Cleanup(func() { injectEditorTag, injectProvenance = lyrics.InjectEditorTag, lyrics.InjectProvenance })
		if failEditor {
			injectEditorTag = func(string) (bool, error) { return false, os.ErrPermission }
		} else {
			injectProvenance = func(string, lyrics.ProvenanceTags) (int, int, error) { return 0, 0, os.ErrPermission }
		}
		res := g.run(t, false, nil)
		injectEditorTag, injectProvenance = lyrics.InjectEditorTag, lyrics.InjectProvenance
		if res.Restored != 1 || res.Errors != 1 {
			t.Errorf("failEditor=%v: result = %+v; want the restore and one error", failEditor, res)
		}
		if got := mustRead(t, g.lrc); got != genOriginal || exists(g.lrc+".orig") || g.row(t) != genRestored {
			t.Errorf("failEditor=%v: restore did not stand: .lrc %q, row %q", failEditor, got, g.row(t))
		}
	}
}
