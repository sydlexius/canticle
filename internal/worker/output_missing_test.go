package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/prune"
	"github.com/sydlexius/canticle/internal/queue"
)

// linkScan adds a scan_results row for file in library libID and links it to
// work item wqID through the junction, as the scan enqueue and the work_queue
// dedupe do. It returns the scan_results id.
func linkScan(t *testing.T, sqlDB *sql.DB, libID, wqID int64, file string) int64 {
	t.Helper()
	res, err := sqlDB.Exec(
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename) VALUES (?, ?, 'Synthetic Artist', 'Synthetic', 'pending', ?, ?)`,
		libID, file, filepath.Dir(file), filepath.Base(file))
	if err != nil {
		t.Fatal(err)
	}
	srID, _ := res.LastInsertId()
	if _, err := sqlDB.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wqID, srID); err != nil {
		t.Fatal(err)
	}
	return srID
}

// newLinkedRig is newHealRig with the row's own source linked to it, as a
// scan-enqueued row is: the stale test then has information to work with.
func newLinkedRig(t *testing.T, paths func(srcDir, root, rootB string) []models.OutputPath) *healRig {
	t.Helper()
	r := newHealRig(t, paths)
	linkScan(t, r.db, r.libA, r.id, filepath.Join(r.srcDir, "01.flac"))
	return r
}

func (r *healRig) scanStatus(t *testing.T, id int64) string {
	t.Helper()
	var st string
	if err := r.db.QueryRow(`SELECT status FROM scan_results WHERE id = ?`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// makeDue lets a failed row be claimed again without waiting out its backoff.
func (r *healRig) makeDue(t *testing.T) {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
		t.Fatal(err)
	}
}

// F1, the reviewer's reproduction: the SAME filename in two online libraries,
// both files linked, B's directory missing. The row must not settle: A is
// written, the row fails as it does on main, B's scan result stays open, and when
// B's directory returns the next pass writes B and only then settles.
func TestRunOnce_RealSecondCopyIsWrittenWhenItsDirectoryReturns(t *testing.T) {
	var secondDir string
	rig := newLinkedRig(t, func(srcDir, _, rootB string) []models.OutputPath {
		secondDir = filepath.Join(rootB, "artist", "New")
		return []models.OutputPath{{Outdir: srcDir, Filename: "01.lrc"}, {Outdir: secondDir, Filename: "01.lrc"}}
	})
	srB := linkScan(t, rig.db, rig.libB, rig.id, filepath.Join(secondDir, "01.flac"))
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, attempts, paths := rig.row(t)
	if status != "failed" || attempts != 1 || len(paths) != 2 {
		t.Fatalf("status=%s attempts=%d paths=%+v, want failed/1 with both entries kept", status, attempts, paths)
	}
	if !exists(filepath.Join(rig.srcDir, "01.lrc")) {
		t.Error("the writable entry was not written")
	}
	if st := rig.scanStatus(t, srB); st == "done" {
		t.Errorf("B's scan_results row is %q: it was settled without its copy", st)
	}

	mkSource(t, secondDir) // B's directory returns
	rig.makeDue(t)
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if status, _, _ := rig.row(t); status != "done" {
		t.Fatalf("status after the directory returned = %s, want done", status)
	}
	if !exists(filepath.Join(secondDir, "01.lrc")) {
		t.Error("the second copy was never written")
	}
	if st := rig.scanStatus(t, srB); st != "done" {
		t.Errorf("B's scan_results row = %q after its copy was written, want done", st)
	}
}

// An unmounted second library gives no proof the entry is stale: the row fails
// and keeps both entries, whether or not the missing copy's file is linked.
func TestRunOnce_OfflineSecondLibraryIsNotSettled(t *testing.T) {
	var secondDir string
	rig := newLinkedRig(t, func(srcDir, _, rootB string) []models.OutputPath {
		secondDir = filepath.Join(rootB, "artist", "New")
		return []models.OutputPath{{Outdir: srcDir, Filename: "01.lrc"}, {Outdir: secondDir, Filename: "01.lrc"}}
	})
	takeOffline(t, rig.rootB)
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, _, paths := rig.row(t)
	if status != "failed" || len(paths) != 2 {
		t.Errorf("status=%s paths=%+v, want failed with both entries kept", status, paths)
	}
	if !exists(filepath.Join(rig.srcDir, "01.lrc")) {
		t.Error("the writable entry was not written")
	}
}

// A missing folder in an ONLINE root that no linked file lives in is provably
// stale: the row settles and the entry STAYS in output_paths (the unattended
// worker never removes one).
func TestRunOnce_ProvablyStaleMissingEntrySettlesAndIsKept(t *testing.T) {
	rig := newLinkedRig(t, func(srcDir, root, _ string) []models.OutputPath {
		return []models.OutputPath{
			{Outdir: filepath.Join(root, "artist", "OldAlbum"), Filename: "01.lrc"},
			{Outdir: srcDir, Filename: "01.lrc"},
		}
	})
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, _, paths := rig.row(t)
	if status != "done" || len(paths) != 2 {
		t.Errorf("status=%s paths=%+v, want done with both entries kept", status, paths)
	}
	if !exists(filepath.Join(rig.srcDir, "01.lrc")) {
		t.Error("the writable entry was not written")
	}
}

// A row with NO linked scan_results (webhook-enqueued work) gives the stale test
// no information: the missing entry is a possible real copy, so the row fails.
func TestRunOnce_MissingEntryWithNoLinkRowsIsNotSettled(t *testing.T) {
	rig := newLinkedRig(t, func(srcDir, root, _ string) []models.OutputPath {
		return []models.OutputPath{
			{Outdir: filepath.Join(root, "artist", "OldAlbum"), Filename: "01.lrc"},
			{Outdir: srcDir, Filename: "01.lrc"},
		}
	})
	if _, err := rig.db.Exec(`DELETE FROM work_queue_scan_results WHERE work_queue_id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, _, paths := rig.row(t)
	if status != "failed" || len(paths) != 2 {
		t.Errorf("status=%s paths=%+v, want failed with both entries kept", status, paths)
	}
}

// A stale single entry whose correct directory is derivable is corrected in the
// row and written in the same pass, not failed into backoff forever.
func TestRunOnce_HealsStaleSingleEntry(t *testing.T) {
	rig := newHealRig(t, func(_, root, _ string) []models.OutputPath {
		return []models.OutputPath{{Outdir: filepath.Join(root, "artist", "Old"), Filename: "01.lrc"}}
	})
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, attempts, paths := rig.row(t)
	if status != "done" || attempts != 0 || len(paths) != 1 || paths[0].Outdir != rig.srcDir {
		t.Errorf("status=%s attempts=%d paths=%+v, want done/0 at the corrected directory", status, attempts, paths)
	}
	if !exists(filepath.Join(rig.srcDir, "01.lrc")) {
		t.Error("the lyric was not written to the corrected directory")
	}
}

// F2: a single entry in ANOTHER library is that library's copy, not this row's
// stale path. It is not rewritten to the row's own library: the row fails before
// any fetch with its output_paths unchanged.
func TestRunOnce_SingleEntryInAnotherLibraryIsNotHealed(t *testing.T) {
	var want models.OutputPath
	rig := newHealRig(t, func(_, _, rootB string) []models.OutputPath {
		want = models.OutputPath{Outdir: filepath.Join(rootB, "artist", "Old"), Filename: "01.lrc"}
		return []models.OutputPath{want}
	})
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, attempts, paths := rig.row(t)
	if status != "failed" || attempts != 1 || len(paths) != 1 || paths[0] != want || rig.fetcher.calls != 0 {
		t.Errorf("status=%s attempts=%d paths=%+v fetches=%d, want failed/1, unchanged, no fetch", status, attempts, paths, rig.fetcher.calls)
	}
	if exists(filepath.Join(rig.srcDir, "01.lrc")) {
		t.Error("the lyric was written into the row's own library")
	}
}

// With the source audio gone the single entry is NOT healed: the row fails
// without a fetch and its output_paths is unchanged.
func TestRunOnce_SingleEntryWithSourceGoneIsNotHealed(t *testing.T) {
	var want models.OutputPath
	rig := newHealRig(t, func(_, root, _ string) []models.OutputPath {
		want = models.OutputPath{Outdir: filepath.Join(root, "artist", "Old"), Filename: "01.lrc"}
		return []models.OutputPath{want}
	})
	if err := os.Remove(filepath.Join(rig.srcDir, "01.flac")); err != nil {
		t.Fatal(err)
	}
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, attempts, paths := rig.row(t)
	if status != "failed" || attempts != 1 || len(paths) != 1 || paths[0] != want || rig.fetcher.calls != 0 {
		t.Errorf("status=%s attempts=%d paths=%+v fetches=%d, want failed/1, unchanged, no fetch", status, attempts, paths, rig.fetcher.calls)
	}
}

// Several entries, none of whose directory exists: the row fails as an ordinary
// failure BEFORE any provider request, and nothing is removed.
func TestRunOnce_AllEntriesMissingFailsWithoutFetch(t *testing.T) {
	var want []models.OutputPath
	rig := newHealRig(t, func(_, root, _ string) []models.OutputPath {
		want = []models.OutputPath{
			{Outdir: filepath.Join(root, "artist", "Gone1"), Filename: "a.lrc"},
			{Outdir: filepath.Join(root, "artist", "Gone2"), Filename: "b.lrc"},
		}
		return want
	})
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, attempts, paths := rig.row(t)
	if status != "failed" || attempts != 1 || rig.fetcher.calls != 0 {
		t.Errorf("status=%s attempts=%d fetches=%d, want failed/1/0", status, attempts, rig.fetcher.calls)
	}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("output_paths = %+v, want unchanged %+v", paths, want)
	}
}

// A directory that vanishes between the preflight and the write is the fallback:
// with nothing written the row fails as an ordinary failure, entries untouched.
func TestRunOnce_DirVanishingMidPassFailsWithEntriesKept(t *testing.T) {
	var tmp1, tmp2 string
	rig := newHealRig(t, func(_, root, _ string) []models.OutputPath {
		tmp1, tmp2 = filepath.Join(root, "artist", "Tmp1"), filepath.Join(root, "artist", "Tmp2")
		for _, d := range []string{tmp1, tmp2} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return []models.OutputPath{{Outdir: tmp1, Filename: "01.lrc"}, {Outdir: tmp2, Filename: "02.lrc"}}
	})
	rig.fetcher.onCall = func() { // the directories vanish after the preflight
		_ = os.RemoveAll(tmp1)
		_ = os.RemoveAll(tmp2)
	}
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, attempts, paths := rig.row(t)
	if status != "failed" || attempts != 1 || len(paths) != 2 || rig.fetcher.calls != 1 {
		t.Errorf("status=%s attempts=%d paths=%+v fetches=%d, want failed/1 with both entries kept", status, attempts, paths, rig.fetcher.calls)
	}
}

// detectorRig is a linked rig whose first entry is missing and whose second
// exists, with a detector that finds the track instrumental.
func detectorRig(t *testing.T) *healRig {
	t.Helper()
	rig := newLinkedRig(t, func(srcDir, root, _ string) []models.OutputPath {
		return []models.OutputPath{
			{Outdir: filepath.Join(root, "artist", "Gone"), Filename: "01.lrc"},
			{Outdir: srcDir, Filename: "01.lrc"},
		}
	})
	rig.fetcher.err = musixmatch.ErrNotFound
	rig.w.EnableAudioDetector(&fakeDetector{instrumental: true, version: "9.9.9"})
	rig.w.SetInstrumentalDetectionDefault(true)
	return rig
}

// The detector-instrumental marker write writes the entry it can and settles
// only because the missing entry is provably stale.
func TestRunOnce_DetectorInstrumentalSettlesPastAStaleEntry(t *testing.T) {
	rig := detectorRig(t)
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, _, paths := rig.row(t)
	if status != "done" || len(paths) != 2 {
		t.Errorf("status=%s paths=%+v, want done with both entries kept", status, paths)
	}
	if !exists(filepath.Join(rig.srcDir, "01.txt")) {
		t.Error("the instrumental marker was not written to the existing directory")
	}
}

// F1 at the detector site: a missing entry that is NOT provably stale (no link
// rows) must not let the instrumental verdict settle the row.
func TestRunOnce_DetectorInstrumentalDoesNotSettlePastARealCopy(t *testing.T) {
	rig := detectorRig(t)
	if _, err := rig.db.Exec(`DELETE FROM work_queue_scan_results WHERE work_queue_id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	if err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, _, paths := rig.row(t)
	if status == "done" || len(paths) != 2 {
		t.Errorf("status=%s paths=%+v, want the row left open with both entries kept", status, paths)
	}
	if !exists(filepath.Join(rig.srcDir, "01.txt")) {
		t.Error("the writable entry was not written")
	}
}

// recheckWithMissingEntry flips a word-recheck row to two entries, the first in
// a directory that does not exist, under a configured, online library root.
func recheckWithMissingEntry(t *testing.T, linked bool) (*recheckRig, *Worker, string) {
	t.Helper()
	primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
	rig, w := newRecheckRig(t, primary, nil, false)
	lib := filepath.Dir(rig.lrc)
	libRow, err := library.New(rig.db).Add(context.Background(), lib, "recheck", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	w.SetOutputHealer(prune.New(rig.db))
	if linked {
		linkScan(t, rig.db, libRow.ID, rig.id, filepath.Join(lib, "track.flac"))
	}
	two, _ := json.Marshal([]models.OutputPath{
		{Outdir: filepath.Join(lib, "Gone"), Filename: "track.lrc"},
		{Outdir: lib, Filename: "track.lrc"},
	})
	if _, err := rig.db.Exec(`UPDATE work_queue SET output_paths = ? WHERE id = ?`, string(two), rig.id); err != nil {
		t.Fatal(err)
	}
	return rig, w, string(two)
}

func wordRecheckState(t *testing.T, rig *recheckRig) (state, paths string) {
	t.Helper()
	var st sql.NullString
	if err := rig.db.QueryRow(`SELECT word_timing_state, output_paths FROM work_queue WHERE id = ?`, rig.id).Scan(&st, &paths); err != nil {
		t.Fatal(err)
	}
	return st.String, paths
}

// The word-recheck write skips a provably stale missing entry, settles, and
// keeps the entry in the row.
func TestWordRecheck_SettlesPastAStaleEntryAndKeepsIt(t *testing.T) {
	rig, w, two := recheckWithMissingEntry(t, true)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got, _ := os.ReadFile(rig.lrc); !bytes.Contains(got, []byte("word line")) {
		t.Fatalf(".lrc = %s, want the word result written", got)
	}
	state, paths := wordRecheckState(t, rig)
	if state == "queued" || paths != two {
		t.Errorf("word_timing_state=%q output_paths=%s, want the recheck settled with both entries kept", state, paths)
	}
}

// F1 at the word-recheck site: an entry that is not provably stale (no link
// rows) leaves the recheck queued, so it retries when the directory returns.
func TestWordRecheck_DoesNotSettlePastARealCopy(t *testing.T) {
	rig, w, two := recheckWithMissingEntry(t, false)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	state, paths := wordRecheckState(t, rig)
	if state != "queued" || paths != two {
		t.Errorf("word_timing_state=%q output_paths=%s, want the recheck still queued with both entries kept", state, paths)
	}
}

// F3a: one entry kept (a better lyric already on disk) plus one skipped, provably
// stale entry takes the KEPT path: nothing landed, so nothing is stamped or
// cached for content that is not on disk.
func TestRunOnce_KeptEntryPlusStaleSkippedTakesTheKeptPath(t *testing.T) {
	q := &fakeQueue{}
	c := &fakeCache{}
	track := models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"}
	dir := t.TempDir()
	q.items = []queue.WorkItem{{ID: 77, Inputs: models.Inputs{Track: track, OutputPaths: []models.OutputPath{
		{Outdir: dir, Filename: "a.lrc"}, {Outdir: filepath.Join(dir, "gone"), Filename: "b.lrc"},
	}}}}
	wr := &scriptedWriter{errs: []error{lyrics.ErrKeptBetter, lyrics.ErrOutputDirMissing}}
	w := New(q, c, &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "words"}}}, wr)
	h := newFakeHealer(nil)
	h.stale = true
	w.SetOutputHealer(h)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.completed) != 1 || q.completed[0] != 77 || len(q.failed) != 0 {
		t.Fatalf("completed=%v failed=%v, want the row settled", q.completed, q.failed)
	}
	if len(c.stores) != 0 {
		t.Errorf("cache stores = %d: the kept path must not cache a result that was not written", len(c.stores))
	}
}
