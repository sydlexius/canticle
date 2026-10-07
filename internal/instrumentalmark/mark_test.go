package instrumentalmark

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

type fixture struct {
	ctx  context.Context
	db   *sql.DB
	dir  string
	id   int64
	m    *Marker
	q    *queue.DBQueue
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "music")
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.Open(ctx, filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	lib, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status) VALUES (?, ?, 'Artist', 'Title', ?, 'song.flac', 'done')`,
		lib.ID, filepath.Join(dir, "song.flac"), dir)
	if err != nil {
		t.Fatal(err)
	}
	srID, _ := res.LastInsertId()
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:       dir,
		Filename:     "song.flac",
		SourcePath:   filepath.Join(dir, "song.flac"),
		ScanResultID: srID,
		OutputPaths:  []models.OutputPath{{Outdir: dir, Filename: "song.flac"}},
	}, queue.PriorityScan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'done', outcome_type = 'synced', sync_tier = 'line' WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	return &fixture{ctx: ctx, db: sqlDB, dir: dir, id: item.ID, m: New(sqlDB, lyrics.NewLRCWriter(root)), q: q, root: root}
}

func (f *fixture) write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(f.dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, name))
	return err == nil
}

func (f *fixture) marked(t *testing.T) bool {
	t.Helper()
	_, ok, err := f.q.ManualInstrumental(f.ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

type recorder struct{ recs []Record }

func (r *recorder) report(rec Record) error { r.recs = append(r.recs, rec); return nil }

func TestMarkReplacesSyncedLyricsBackupFirst(t *testing.T) {
	f := newFixture(t)
	lrcBody := "[ar:A]\n[00:01.00]hello\n"
	f.write(t, "song.lrc", lrcBody)
	f.write(t, "song.elrc", "[by:canticle]\n[00:01.00]<00:01.00>hello\n")
	if _, err := f.db.ExecContext(f.ctx, `UPDATE work_queue SET word_timing_state = 'queued' WHERE id = ?`, f.id); err != nil {
		t.Fatal(err)
	}
	if err := cache.New(f.db).Store(f.ctx, "Artist", "Title", 0, "old lyrics"); err != nil {
		t.Fatal(err)
	}

	// The backup must exist before any file changes: check inside Report.
	jf, err := os.Create(filepath.Join(t.TempDir(), "backup.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jf.Close() }()
	report := func(rec Record) error {
		if !f.exists("song.lrc") || f.marked(t) {
			t.Errorf("Report ran after a change: lrc present=%v marked=%v", f.exists("song.lrc"), f.marked(t))
		}
		return AppendRecord(jf, rec)
	}
	res, err := f.m.Mark(f.ctx, f.id, Options{Report: report})
	if err != nil || res.Outcome != OutcomeMarked || res.FilesBackedUp != 2 {
		t.Fatalf("Mark = %+v, %v; want marked with 2 files", res, err)
	}
	if f.exists("song.lrc") || f.exists("song.elrc") {
		t.Error("lyric files survived the mark")
	}
	if !lyrics.ManualMarkerOnDisk(filepath.Join(f.dir, "song.txt")) {
		t.Error("no manual marker on disk")
	}
	if !f.marked(t) {
		t.Error("row not marked")
	}
	var restored bool
	raw, _ := os.ReadFile(jf.Name())
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for _, l := range lines {
		var r Record
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatal(err)
		}
		restored = restored || (r.Op == OpMark && r.WorkItemID == f.id && filepath.Base(r.Path) == "song.lrc" && string(r.Content) == lrcBody)
	}
	if !restored || len(lines) != 2 {
		t.Errorf("backup = %q; want the .lrc byte-for-byte plus the .elrc", raw)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM lyrics_cache`).Scan(&n); err != nil || n != 0 {
		t.Errorf("cache entries = %d, %v; want 0", n, err)
	}
	var wts sql.NullString
	if err := f.db.QueryRow(`SELECT word_timing_state FROM work_queue WHERE id = ?`, f.id).Scan(&wts); err != nil || wts.Valid {
		t.Errorf("word_timing_state = %v, %v; want NULL so no recheck asks a lane", wts, err)
	}
	if item, err := f.q.Dequeue(f.ctx); err == nil || item.ID != 0 {
		t.Errorf("Dequeue = %+v, %v; want nothing claimable", item, err)
	}
}

func TestMarkWithNoLyricFilesAndSecondMarkIsNoop(t *testing.T) {
	f := newFixture(t)
	rec := &recorder{}
	res, err := f.m.Mark(f.ctx, f.id, Options{Report: rec.report})
	if err != nil || res.Outcome != OutcomeMarked || res.FilesBackedUp != 0 || len(rec.recs) != 0 {
		t.Fatalf("Mark = %+v, %v, %d records", res, err, len(rec.recs))
	}
	if !lyrics.ManualMarkerOnDisk(filepath.Join(f.dir, "song.txt")) {
		t.Fatal("no marker written")
	}
	res, err = f.m.Mark(f.ctx, f.id, Options{Report: rec.report})
	if err != nil || res.Outcome != OutcomeAlreadyMarked || len(rec.recs) != 0 {
		t.Errorf("second Mark = %+v, %v, %d records; want a no-op", res, err, len(rec.recs))
	}
	// A marked row whose marker is gone (a crash between the steps) is repaired.
	if err := os.Remove(filepath.Join(f.dir, "song.txt")); err != nil {
		t.Fatal(err)
	}
	if res, err = f.m.Mark(f.ctx, f.id, Options{}); err != nil || res.Outcome != OutcomeMarked || !lyrics.ManualMarkerOnDisk(filepath.Join(f.dir, "song.txt")) {
		t.Errorf("repair = %+v, %v", res, err)
	}
}

func TestMarkRefusesInFlightRow(t *testing.T) {
	f := newFixture(t)
	f.write(t, "song.lrc", "[00:01.00]hi\n")
	if _, err := f.db.ExecContext(f.ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, f.id); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	_, err := f.m.Mark(f.ctx, f.id, Options{Report: rec.report})
	if !errors.Is(err, queue.ErrManualInstrumentalInFlight) {
		t.Fatalf("err = %v; want ErrManualInstrumentalInFlight", err)
	}
	if len(rec.recs) != 0 || !f.exists("song.lrc") || f.exists("song.txt") {
		t.Error("an in-flight refusal changed something")
	}
	if _, err := f.m.Mark(f.ctx, 99999, Options{}); !errors.Is(err, queue.ErrManualInstrumentalNotFound) {
		t.Errorf("missing row err = %v", err)
	}
}

func TestMarkBackupFailureChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.write(t, "song.lrc", "[00:01.00]hi\n")
	_, err := f.m.Mark(f.ctx, f.id, Options{Report: func(Record) error { return errors.New("disk full") }})
	if err == nil {
		t.Fatal("want an error")
	}
	if !f.exists("song.lrc") || f.exists("song.txt") || f.marked(t) {
		t.Error("a failed backup left a change behind")
	}
}

func TestMarkRowFailureLeavesFilesUntouched(t *testing.T) {
	f := newFixture(t)
	f.write(t, "song.lrc", "[00:01.00]hi\n")
	if _, err := f.db.Exec(`CREATE TRIGGER block_mark BEFORE UPDATE OF manual_instrumental_at ON work_queue BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if _, err := f.m.Mark(f.ctx, f.id, Options{Report: rec.report}); err == nil {
		t.Fatal("want an error")
	}
	if len(rec.recs) != 1 || !f.exists("song.lrc") || f.exists("song.txt") || f.marked(t) {
		t.Errorf("row failure: records=%d lrc=%v txt=%v marked=%v; want a record, files untouched, row unmarked",
			len(rec.recs), f.exists("song.lrc"), f.exists("song.txt"), f.marked(t))
	}
}

func TestMarkWriteFailureUnmarksTheRow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t)
	lrc := f.write(t, "song.lrc", "[00:01.00]hi\n")
	// Backup succeeds, then the directory turns read-only so the marker write fails.
	report := func(Record) error { return os.Chmod(f.dir, 0o555) }
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o755) })
	if _, err := f.m.Mark(f.ctx, f.id, Options{Report: report}); err == nil {
		t.Fatal("want a write error")
	}
	_ = os.Chmod(f.dir, 0o755)
	if f.marked(t) {
		t.Error("row stayed marked after a failed marker write")
	}
	var status string
	if err := f.db.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, f.id).Scan(&status); err != nil || status != "deferred" {
		t.Errorf("status = %q, %v; want deferred so a scan refetches", status, err)
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("original lyrics lost: %v", err)
	}
	// A retry recovers.
	if res, err := f.m.Mark(f.ctx, f.id, Options{}); err != nil || res.Outcome != OutcomeMarked {
		t.Errorf("retry = %+v, %v", res, err)
	}
}

func TestMarkRefusesSymlinkedSidecar(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(t.TempDir(), "elsewhere.lrc")
	if err := os.WriteFile(target, []byte("[00:01.00]x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.dir, "song.lrc")); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if _, err := f.m.Mark(f.ctx, f.id, Options{Report: rec.report}); !errors.Is(err, ErrSymlinkedSidecar) {
		t.Fatalf("err = %v; want ErrSymlinkedSidecar", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "[00:01.00]x\n" || len(rec.recs) != 0 || f.marked(t) {
		t.Error("a symlinked sidecar was followed, backed up or the row marked")
	}
}

func TestMarkDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.write(t, "song.lrc", "[00:01.00]hi\n")
	rec := &recorder{}
	res, err := f.m.Mark(f.ctx, f.id, Options{DryRun: true, Report: rec.report})
	if err != nil || res.Outcome != OutcomeDryRun || res.FilesBackedUp != 1 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if len(rec.recs) != 0 || !f.exists("song.lrc") || f.exists("song.txt") || f.marked(t) {
		t.Error("dry run changed something")
	}
}
