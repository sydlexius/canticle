package lyricblock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

type fx struct {
	ctx  context.Context
	db   *sql.DB
	svc  *Service
	sw   *selfwrite.Registry
	root string
	dir  string
	id   int64
}

func newFx(t *testing.T) *fx {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "music")
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, filepath.Join(base, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var id int64
	if err := d.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename, status, outcome_type, sync_tier, lyric_edited_at, lyric_offset_ms)
		VALUES ('Vexa Dunn', 'Quill Moor', 'vexa dunn', 'quill moor', ?, 'song.flac', 'done', 'synced', 'line', '2026-01-01T00:00:00Z', 250) RETURNING id`, dir).Scan(&id); err != nil {
		t.Fatal(err)
	}
	sw := selfwrite.New(time.Minute)
	return &fx{ctx: ctx, db: d, svc: New(d, nil, sw), sw: sw, root: root, dir: dir, id: id}
}

func (f *fx) write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fx) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, name))
	return err == nil
}

func (f *fx) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fx) mustExec(t *testing.T, q string) {
	t.Helper()
	if _, err := f.db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// req builds a request; a nil report becomes a no-op sink, since a mark with
// files and no Report is refused (ErrNoBackupSink).
func (f *fx) req(report func(Backup) error) MarkRequest {
	if report == nil {
		report = func(Backup) error { return nil }
	}
	return MarkRequest{WorkItemID: f.id, Roots: []string{f.root}, Report: report}
}

func (f *fx) status(t *testing.T) string {
	t.Helper()
	var s string
	if err := f.db.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, f.id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMarkDryRunWritesNothing(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	r := f.req(func(Backup) error { t.Error("Report called in a dry run"); return nil })
	r.DryRun = true
	res, err := f.svc.Mark(f.ctx, r)
	if err != nil || !res.DryRun || res.Files != 1 {
		t.Fatalf("Mark = %+v, %v", res, err)
	}
	if !f.exists("song.lrc") || f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 || f.status(t) != "done" {
		t.Error("dry run changed state")
	}
}

func TestMarkBackupBeforeUnlinkRestoresByteForByte(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	f.write(t, "song.elrc", elrcBody)
	if err := cache.New(f.db).Store(f.ctx, "Vexa Dunn", "Quill Moor", 0, "old"); err != nil {
		t.Fatal(err)
	}
	var recs []Backup
	res, err := f.svc.Mark(f.ctx, f.req(func(b Backup) error {
		if !f.exists("song.lrc") || !f.exists("song.elrc") || f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 {
			t.Error("Report ran after a change")
		}
		recs = append(recs, b)
		return nil
	}))
	if err != nil || res.Files != 2 || res.Removed != 2 || res.NewBlocks != 1 || !res.Reopened || res.CacheInvalidated != 1 {
		t.Fatalf("Mark = %+v, %v", res, err)
	}
	if f.exists("song.lrc") || f.exists("song.elrc") || len(recs) != 2 {
		t.Fatalf("files survived or got %d records", len(recs))
	}
	want := map[string]string{"song.lrc": lrcBody, "song.elrc": elrcBody}
	for _, b := range recs {
		if string(b.Content) != want[filepath.Base(b.Path)] || b.WorkItemID != f.id || b.Op != opMark {
			t.Errorf("backup %s = %+v", filepath.Base(b.Path), b)
		}
		// The cleared row state rides on every record so a restore can re-protect the file.
		if b.Meta["lyric_edited_at"] != "2026-01-01T00:00:00Z" || b.Meta["lyric_offset_ms"] != "250" || b.Meta["sync_tier"] != "line" {
			t.Errorf("backup Meta = %v, want the row's edit state", b.Meta)
		}
		if !f.sw.Suppress(b.Path) {
			t.Errorf("%s not recorded with selfwrite", filepath.Base(b.Path))
		}
	}
	if f.count(t, `SELECT COUNT(*) FROM work_queue WHERE id = ? AND status = 'pending' AND outcome_type IS NULL AND lyric_edited_at IS NULL AND lyric_offset_ms IS NULL AND sync_tier IS NULL`, f.id) != 1 ||
		f.count(t, `SELECT COUNT(*) FROM lyrics_cache`) != 0 {
		t.Error("row not reopened or cache not invalidated")
	}
}

func TestMarkClearsStaleRowState(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	f.mustExec(t, `UPDATE work_queue SET word_timing_state = 'served', word_timing_generation = 3, upgrade_queued = 1, upgrade_miss_count = 4,
		timing_stamp_source = 'sweep', missync_recheck_generation = 2`)
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM work_queue WHERE word_timing_state IS NULL AND word_timing_generation IS NULL AND upgrade_queued = 0
		AND upgrade_miss_count = 0 AND timing_stamp_source IS NULL AND missync_recheck_generation IS NULL`) != 1 {
		t.Error("stale word/upgrade/stamp state survived the mark")
	}
}

func TestMarkTransactionFailureLeavesFilesUntouched(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	if err := cache.New(f.db).Store(f.ctx, "Vexa Dunn", "Quill Moor", 0, "old"); err != nil {
		t.Fatal(err)
	}
	// The cache delete runs after the block insert: a failure there must roll
	// the block back and leave the file alone.
	f.mustExec(t, `CREATE TRIGGER boom BEFORE DELETE ON lyrics_cache BEGIN SELECT RAISE(ABORT, 'boom'); END`)
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Mark err = %v, want the injected failure", err)
	}
	if !f.exists("song.lrc") || f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 || f.status(t) != "done" {
		t.Error("state changed despite the failed transaction")
	}
}

func TestMarkRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fx, t *testing.T)
		roots []string
		want  error
	}{
		{"processing", func(f *fx, t *testing.T) { f.mustExec(t, `UPDATE work_queue SET status = 'processing'`) }, nil, ErrBusy},
		{"failed", func(f *fx, t *testing.T) { f.mustExec(t, `UPDATE work_queue SET status = 'failed'`) }, nil, ErrNotMarkable},
		{"unavailable", func(f *fx, t *testing.T) { f.mustExec(t, `UPDATE work_queue SET status = 'unavailable'`) }, nil, ErrNotMarkable},
		{"manual instrumental", func(f *fx, t *testing.T) {
			f.mustExec(t, `UPDATE work_queue SET manual_instrumental_at = '2026-01-01T00:00:00Z'`)
		}, nil, ErrManualInstrumental},
		{"outside roots", func(*fx, *testing.T) {}, []string{"/nonexistent-root"}, ErrOutsideRoots},
		{"no sidecar", func(f *fx, t *testing.T) { _ = os.Remove(filepath.Join(f.dir, "song.lrc")) }, nil, ErrNoSidecar},
		{"marker only", func(f *fx, t *testing.T) {
			f.write(t, "song.lrc", "[source:musixmatch]\n[00:00.00]♪ Instrumental ♪\n")
		}, nil, ErrNoFingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			f.write(t, "song.lrc", lrcBody)
			tc.setup(f, t)
			r := f.req(func(Backup) error { t.Error("Report called on a refused mark"); return nil })
			if tc.roots != nil {
				r.Roots = tc.roots
			}
			if _, err := f.svc.Mark(f.ctx, r); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 {
				t.Error("a block was written")
			}
		})
	}
}

// The SQL guard (not the Go pre-check) must refuse a row a worker claims
// between the load and the transaction.
func TestMarkRefusesRowClaimedBeforeTheTransaction(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	_, err := f.svc.Mark(f.ctx, f.req(func(Backup) error {
		f.mustExec(t, `UPDATE work_queue SET status = 'processing'`)
		return nil
	}))
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if !f.exists("song.lrc") || f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 || f.status(t) != "processing" {
		t.Error("a claimed row was changed or its file touched")
	}
}

func TestMarkBacksUpAndBlocksFileThatAppearedAfterInventory(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	var got []string
	first := true
	res, err := f.svc.Mark(f.ctx, f.req(func(b Backup) error {
		got = append(got, filepath.Base(b.Path))
		if first { // appears after the first inventory, before the unlink
			first = false
			f.write(t, "song.txt", "Late words nobody saw\n")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || f.exists("song.txt") || f.exists("song.lrc") {
		t.Errorf("backups = %v, txt present=%v", got, f.exists("song.txt"))
	}
	// Its words are blocked too, or the next fetch could bring them straight back.
	late := models.Song{Lyrics: models.Lyrics{LyricsBody: "Late words nobody saw\n"}}
	if res.NewBlocks != 2 || !f.svc.store.AnyBlocked(f.ctx, "Vexa Dunn", "Quill Moor", SongFingerprints(late)) {
		t.Errorf("NewBlocks = %d; the late file's fingerprint was not blocked", res.NewBlocks)
	}
}

// A worker (or anyone) rewriting a file between the passes must not let the
// second pass unlink bytes that were never backed up.
func TestMarkReportsAgainAFileRewrittenBetweenPasses(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	rewritten := "[00:02.00]Wholly other words\n"
	var seen []string
	first := true
	res, err := f.svc.Mark(f.ctx, f.req(func(b Backup) error {
		seen = append(seen, string(b.Content))
		if first {
			first = false
			f.write(t, "song.lrc", rewritten)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != lrcBody || seen[1] != rewritten {
		t.Fatalf("reported %q; want the original then the rewritten bytes", seen)
	}
	other := models.Song{Lyrics: models.Lyrics{LyricsBody: "Wholly other words\n"}}
	if f.exists("song.lrc") || res.NewBlocks != 2 || !f.svc.store.AnyBlocked(f.ctx, "Vexa Dunn", "Quill Moor", SongFingerprints(other)) {
		t.Errorf("rewritten file survived or was not blocked (NewBlocks=%d)", res.NewBlocks)
	}
}

// After the blocks commit the row stays settled until the files are gone, so a
// partial unlink leaves a 'done' row (no worker can claim it) and a retry finishes.
func TestMarkPartialUnlinkIsCompletedByRetry(t *testing.T) {
	f := newFx(t)
	dir2 := filepath.Join(f.root, "second")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustExec(t, fmt.Sprintf(`UPDATE work_queue SET output_paths = '[{"outdir":%q,"filename":"song.flac"},{"outdir":%q,"filename":"song.flac"}]'`, f.dir, dir2))
	f.write(t, "song.lrc", lrcBody)
	if err := os.WriteFile(filepath.Join(dir2, "song.lrc"), []byte(lrcBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir2, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir2, 0o755) })
	res, err := f.svc.Mark(f.ctx, f.req(nil))
	if err == nil || res.Removed != 1 || res.Reopened {
		t.Fatalf("Mark = %+v, %v; want a partial failure with 1 removed and no reopen", res, err)
	}
	if f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 1 || f.status(t) != "done" {
		t.Error("blocks must stand and the row stay settled after a partial unlink")
	}
	if err := os.Chmod(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if res, err = f.svc.Mark(f.ctx, f.req(nil)); err != nil || res.Removed != 1 || res.NewBlocks != 0 || !res.Reopened {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if _, err := os.Lstat(filepath.Join(dir2, "song.lrc")); err == nil || f.status(t) != "pending" {
		t.Error("retry did not finish the unlink and the reopen")
	}
}

// A crash between the unlink and the reopen leaves a settled row, no file and a
// block: Mark again must reopen it, not fail with ErrNoSidecar.
func TestMarkAfterUnlinkBeforeReopenJustReopens(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); err != nil {
		t.Fatal(err)
	}
	f.mustExec(t, `UPDATE work_queue SET status = 'done', outcome_type = 'synced'`) // the state a crash would leave
	r := f.req(func(Backup) error { t.Error("Report called with nothing to remove"); return nil })
	r.DryRun = true
	if res, err := f.svc.Mark(f.ctx, r); err != nil || !res.DryRun || res.Reopened || f.status(t) != "done" {
		t.Fatalf("dry run = %+v, %v, status %s", res, err, f.status(t))
	}
	res, err := f.svc.Mark(f.ctx, f.req(nil))
	if err != nil || !res.Reopened || res.Removed != 0 || f.status(t) != "pending" {
		t.Fatalf("Mark = %+v, %v, status %s", res, err, f.status(t))
	}
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); !errors.Is(err, ErrNoSidecar) {
		t.Errorf("a pending row with no file must still be ErrNoSidecar, got %v", err)
	}
}

// Blocks are stored under the row's own artist_key/title_key even when the
// album artist differs from the track artist (the enforcement side stamps the
// same keys).
func TestMarkBlocksUnderTheRowsOwnKeys(t *testing.T) {
	f := newFx(t)
	f.mustExec(t, `UPDATE work_queue SET album_artist = 'Ostrel Fenn'`)
	f.write(t, "song.lrc", lrcBody)
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); err != nil {
		t.Fatal(err)
	}
	bs, err := f.svc.store.List(f.ctx, ListFilter{WorkItemID: f.id})
	if err != nil || len(bs) != 1 || bs[0].ArtistKey != "vexa dunn" || bs[0].TitleKey != "quill moor" {
		t.Fatalf("blocks = %+v, %v; want one under the row's keys", bs, err)
	}
}

// TestMarkBlocksEveryFormTheNextFetchCanTake: the block is keyed on the words,
// so a lyric marked from any on-disk form matches the same words fetched as a
// synced, unsynced or word-timed result.
func TestMarkBlocksEveryFormTheNextFetchCanTake(t *testing.T) {
	synced := models.Song{Subtitles: models.Synced{Lines: []models.Lines{
		{Text: "Zorp the Blent", Time: models.MsToTime(1000)}, {Text: "Frabble on", Time: models.MsToTime(5000)}}}}
	unsynced := models.Song{Lyrics: models.Lyrics{LyricsBody: "Zorp the Blent\nFrabble on\n"}}
	wordTimed := synced
	wordTimed.Subtitles.Lines = []models.Lines{{Text: "<00:01.00>Zorp the <00:02.00>Blent", Time: models.MsToTime(1000)}, {Text: "Frabble on", Time: models.MsToTime(5000)}}
	for _, disk := range []struct{ name, body string }{{"song.lrc", lrcBody}, {"song.txt", "Zorp the Blent\nFrabble on\n"}, {"song.elrc", "[by:canticle]\n[00:01.00]<00:01.00>Zorp the <00:02.00>Blent\n[00:05.00]<00:05.00>Frabble on\n"}} {
		t.Run(disk.name, func(t *testing.T) {
			f := newFx(t)
			f.write(t, disk.name, disk.body)
			if _, err := f.svc.Mark(f.ctx, f.req(nil)); err != nil {
				t.Fatal(err)
			}
			for name, song := range map[string]models.Song{"synced": synced, "unsynced": unsynced, "word-timed": wordTimed} {
				if !f.svc.store.AnyBlocked(f.ctx, "Vexa Dunn", "Quill Moor", SongFingerprints(song)) {
					t.Errorf("%s result not blocked after marking from %s", name, disk.name)
				}
			}
			other := models.Song{Lyrics: models.Lyrics{LyricsBody: "A different song entirely\n"}}
			if f.svc.store.AnyBlocked(f.ctx, "Vexa Dunn", "Quill Moor", SongFingerprints(other)) {
				t.Error("an unrelated body is blocked")
			}
		})
	}
}

func TestMarkWithoutAReportSinkRefusesAndChangesNothing(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	_, err := f.svc.Mark(f.ctx, MarkRequest{WorkItemID: f.id, Roots: []string{f.root}})
	if !errors.Is(err, ErrNoBackupSink) {
		t.Fatalf("err = %v, want ErrNoBackupSink", err)
	}
	if !f.exists("song.lrc") || f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 || f.status(t) != "done" {
		t.Error("a refused mark changed state")
	}
}
