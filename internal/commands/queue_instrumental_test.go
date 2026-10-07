package commands

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/instrumentalmark"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

const (
	qiArtist = "Zephyrine Quartz"
	qiTitle  = "Moonlit Ledger"
	qiAlbum  = "secretalbum"
	qiLRC    = "[00:01.00]hidden words\n"
)

type qiFixture struct {
	ctx            context.Context
	cfg, dbPath    string
	root, dir      string
	audio, lrcPath string
	id, strayID    int64
}

// newQIFixture seeds one done row with a synced .lrc beside its audio inside a
// library root, plus a stray row outside any library (marking it fails).
func newQIFixture(t *testing.T) *qiFixture {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	f := &qiFixture{ctx: ctx, dbPath: filepath.Join(base, "q.db"), cfg: filepath.Join(base, "config.toml")}
	f.root = filepath.Join(base, "music")
	f.dir = filepath.Join(f.root, qiAlbum)
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.audio = filepath.Join(f.dir, "song.flac")
	f.lrcPath = filepath.Join(f.dir, "song.lrc")
	if err := os.WriteFile(f.lrcPath, []byte(qiLRC), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cfg, []byte("[db]\npath = \""+strings.ReplaceAll(f.dbPath, `\`, `\\`)+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.Open(ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	lib, err := library.New(sqlDB).Add(ctx, f.root, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status) VALUES (?, ?, ?, ?, ?, 'song.flac', 'done')`,
		lib.ID, f.audio, qiArtist, qiTitle, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	srID, _ := res.LastInsertId()
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	enq := func(artist, outdir string, sr int64) int64 {
		item, eerr := q.Enqueue(ctx, models.Inputs{
			Track:        models.Track{ArtistName: artist, TrackName: qiTitle},
			Outdir:       outdir,
			Filename:     "song.flac",
			SourcePath:   filepath.Join(outdir, "song.flac"),
			ScanResultID: sr,
			OutputPaths:  []models.OutputPath{{Outdir: outdir, Filename: "song.flac"}},
		}, queue.PriorityScan)
		if eerr != nil {
			t.Fatal(eerr)
		}
		if _, eerr = sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'done', outcome_type = 'synced', sync_tier = 'line' WHERE id = ?`, item.ID); eerr != nil {
			t.Fatal(eerr)
		}
		return item.ID
	}
	f.id = enq(qiArtist, f.dir, srID)
	f.strayID = enq("Stray Artist", filepath.Join(base, "elsewhere"), 0)
	return f
}

func (f *qiFixture) run(t *testing.T, unmark bool, a QueueMarkInstrumentalCmd) (string, int) {
	t.Helper()
	a.ConfigPath = f.cfg
	var out bytes.Buffer
	code := runQueueInstrumental(f.ctx, &out, unmark, a)
	return out.String(), code
}

func (f *qiFixture) row(t *testing.T, id int64) (marked bool) {
	t.Helper()
	sqlDB, err := db.Open(f.ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	var status string
	var at sql.NullString
	if err := sqlDB.QueryRowContext(f.ctx, `SELECT status, manual_instrumental_at FROM work_queue WHERE id = ?`, id).Scan(&status, &at); err != nil {
		t.Fatal(err)
	}
	return at.Valid
}

func (f *qiFixture) markerExists() bool {
	_, err := os.Lstat(filepath.Join(f.dir, "song.txt"))
	return err == nil
}

func qiNoLeak(t *testing.T, label, out string, f *qiFixture) {
	t.Helper()
	for _, s := range []string{qiArtist, qiTitle, qiAlbum, f.root, f.dir, "song.flac", "song.lrc", "hidden words", "Stray Artist"} {
		if strings.Contains(out, s) {
			t.Errorf("%s stdout leaks %q:\n%s", label, s, out)
		}
	}
}

func TestQueueInstrumentalDryRunChangesNothing(t *testing.T) {
	f := newQIFixture(t)
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	out, code := f.run(t, false, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Backup: backup})
	if code != 0 || !strings.Contains(out, "dry run: nothing changed") || !strings.Contains(out, "would mark: 1") {
		t.Fatalf("dry mark = %d:\n%s", code, out)
	}
	qiNoLeak(t, "dry mark", out, f)
	if marked := f.row(t, f.id); marked || f.markerExists() {
		t.Errorf("dry run changed state: marked=%v marker=%v", marked, f.markerExists())
	}
	if b, _ := os.ReadFile(f.lrcPath); string(b) != qiLRC {
		t.Errorf("lrc changed in a dry run: %q", b)
	}
	if _, err := os.Stat(backup); err == nil {
		t.Error("dry run wrote a backup file")
	}
	// Dry-run unmark of an unmarked row also changes nothing and leaks nothing.
	out, code = f.run(t, true, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Backup: backup})
	if code != 0 || !strings.Contains(out, "not marked: 1") {
		t.Fatalf("dry unmark = %d:\n%s", code, out)
	}
	qiNoLeak(t, "dry unmark", out, f)
}

func TestQueueInstrumentalMarkThenUnmarkRoundTrip(t *testing.T) {
	f := newQIFixture(t)
	markBackup := filepath.Join(t.TempDir(), "mark.jsonl")
	out, code := f.run(t, false, QueueMarkInstrumentalCmd{Paths: []string{f.audio}, Yes: true, Backup: markBackup})
	t.Logf("real mark stdout:\n%s", out)
	if code != 0 || !strings.Contains(out, "marked: 1") || !strings.Contains(out, "lyric files backed up: 1") ||
		!strings.Contains(out, "backup: "+markBackup) {
		t.Fatalf("real mark = %d:\n%s", code, out)
	}
	qiNoLeak(t, "real mark", out, f)
	if marked := f.row(t, f.id); !marked || !f.markerExists() {
		t.Fatalf("after mark: marked=%v marker=%v", marked, f.markerExists())
	}
	if _, err := os.Stat(f.lrcPath); err == nil {
		t.Error("the .lrc should have been replaced")
	}
	// The backup holds the replaced bytes, restorable to their path.
	fh, err := os.Open(markBackup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	sc := bufio.NewScanner(fh)
	var recs []instrumentalmark.Record
	for sc.Scan() {
		var r instrumentalmark.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	if len(recs) != 1 || recs[0].Op != instrumentalmark.OpMark || !sameFile(t, recs[0].Path, f.lrcPath) || string(recs[0].Content) != qiLRC {
		t.Fatalf("backup records = %+v; want one restorable record of the .lrc", recs)
	}

	// Dry-run unmark first: still marked.
	out, code = f.run(t, true, QueueMarkInstrumentalCmd{IDs: []int64{f.id}})
	if code != 0 || !strings.Contains(out, "would unmark: 1") {
		t.Fatalf("dry unmark = %d:\n%s", code, out)
	}
	qiNoLeak(t, "dry unmark", out, f)
	if marked := f.row(t, f.id); !marked || !f.markerExists() {
		t.Fatal("dry-run unmark changed state")
	}

	unmarkBackup := filepath.Join(t.TempDir(), "unmark.jsonl")
	out, code = f.run(t, true, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Yes: true, Backup: unmarkBackup})
	if code != 0 || !strings.Contains(out, "unmarked: 1") || !strings.Contains(out, "lyric files backed up: 1") {
		t.Fatalf("real unmark = %d:\n%s", code, out)
	}
	qiNoLeak(t, "real unmark", out, f)
	if marked := f.row(t, f.id); marked || f.markerExists() {
		t.Errorf("after unmark: marked=%v marker=%v; want cleared and removed", marked, f.markerExists())
	}
	f.requireDequeues(t, f.id)
	if b, err := os.ReadFile(unmarkBackup); err != nil || !strings.Contains(string(b), `"op":"unmark"`) {
		t.Errorf("unmark backup = %q, %v", b, err)
	}
}

func TestQueueInstrumentalCountsMissingAndFailures(t *testing.T) {
	f := newQIFixture(t)
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	// The stray row (outside any library) fails; the good row after it is
	// still processed; the unknown id and unknown path are counted.
	out, code := f.run(t, false, QueueMarkInstrumentalCmd{
		IDs:   []int64{f.strayID, 987654, f.id},
		Paths: []string{filepath.Join(f.dir, "nope.flac")},
		Yes:   true, Backup: backup,
	})
	if code != 1 {
		t.Errorf("exit = %d; want 1 when a row failed", code)
	}
	for _, want := range []string{"marked: 1", "not found: 2", "failed: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	qiNoLeak(t, "mixed", out, f)
	if marked := f.row(t, f.id); !marked {
		t.Error("the good row was not processed after a failure")
	}
	if marked := f.row(t, f.strayID); marked {
		t.Error("the failed row was marked")
	}
}

func TestQueueInstrumentalNeedsASelector(t *testing.T) {
	f := newQIFixture(t)
	if out, code := f.run(t, false, QueueMarkInstrumentalCmd{}); code != 2 || !strings.Contains(out, "--id or --path") {
		t.Errorf("no selector = %d:\n%s", code, out)
	}
}

// sameFile compares two paths after symlink resolution (the service records
// the resolved path; t.TempDir may sit behind a symlink).
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(filepath.Dir(a))
	if err != nil {
		t.Fatal(err)
	}
	rb, err := filepath.EvalSymlinks(filepath.Dir(b))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(ra, filepath.Base(a)) == filepath.Join(rb, filepath.Base(b))
}

// requireDequeues asserts the worker would pick row id up again.
func (f *qiFixture) requireDequeues(t *testing.T, id int64) {
	t.Helper()
	sqlDB, err := db.Open(f.ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Dequeue(f.ctx)
	if err != nil || item.ID != id {
		t.Fatalf("Dequeue = %+v, %v; want row %d re-queued", item, err, id)
	}
}

func (f *qiFixture) setStatus(t *testing.T, id int64, status string) {
	t.Helper()
	sqlDB, err := db.Open(f.ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	if _, err := sqlDB.ExecContext(f.ctx, `UPDATE work_queue SET status = ? WHERE id = ?`, status, id); err != nil {
		t.Fatal(err)
	}
}

func TestQueueInstrumentalBackupOpenFailureRefusesTheRow(t *testing.T) {
	f := newQIFixture(t)
	// The backup's directory does not exist, so opening it fails.
	backup := filepath.Join(t.TempDir(), "missing-dir", "b.jsonl")
	out, code := f.run(t, false, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Yes: true, Backup: backup})
	if code != 1 {
		t.Errorf("exit = %d; want 1", code)
	}
	for _, want := range []string{"failed: 1", "marked: 0", "backup: not written (see the errors above)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "none written") {
		t.Errorf("stdout claims nothing needed backing up:\n%s", out)
	}
	qiNoLeak(t, "backup open failure", out, f)
	if marked := f.row(t, f.id); marked || f.markerExists() {
		t.Errorf("row changed despite no backup: marked=%v marker=%v", marked, f.markerExists())
	}
	if b, _ := os.ReadFile(f.lrcPath); string(b) != qiLRC {
		t.Errorf("lrc not intact: %q", b)
	}
}

func TestQueueInstrumentalBackupFileIsPrivate(t *testing.T) {
	f := newQIFixture(t)
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	if _, code := f.run(t, false, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Yes: true, Backup: backup}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	fi, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %o; want 600", perm)
	}
}

func TestQueueInstrumentalDuplicateSelectorsCountOnce(t *testing.T) {
	for _, yes := range []bool{false, true} {
		for name, mk := range map[string]func(f *qiFixture) QueueMarkInstrumentalCmd{
			"id twice": func(f *qiFixture) QueueMarkInstrumentalCmd { return QueueMarkInstrumentalCmd{IDs: []int64{f.id, f.id}} },
			"id and path": func(f *qiFixture) QueueMarkInstrumentalCmd {
				return QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Paths: []string{f.audio}}
			},
		} {
			f := newQIFixture(t)
			a := mk(f)
			a.Yes, a.Backup = yes, filepath.Join(t.TempDir(), "b.jsonl")
			want := "would mark: 1"
			if yes {
				want = "marked: 1"
			}
			out, code := f.run(t, false, a)
			if code != 0 || !strings.Contains(out, want) || strings.Contains(out, "already marked: 1") {
				t.Errorf("%s (yes=%v) = %d; want %q once and no already-marked:\n%s", name, yes, code, want, out)
			}
		}
	}
}

func TestQueueInstrumentalInFlightIsCountedAndExitsNonZero(t *testing.T) {
	f := newQIFixture(t)
	f.setStatus(t, f.id, "processing")
	out, code := f.run(t, false, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Yes: true, Backup: filepath.Join(t.TempDir(), "b.jsonl")})
	if code != 1 {
		t.Errorf("exit = %d; want 1 for an in-flight row", code)
	}
	for _, want := range []string{"in flight: 1", "failed: 0", "marked: 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if marked := f.row(t, f.id); marked {
		t.Error("in-flight row was marked")
	}
}

func TestQueueInstrumentalRefusesUnsafeBackupPath(t *testing.T) {
	f := newQIFixture(t)
	outside := t.TempDir()
	cases := map[string]string{
		"lrc extension":      filepath.Join(outside, "b.lrc"),
		"txt upper case":     filepath.Join(outside, "b.TXT"),
		"elrc extension":     filepath.Join(outside, "b.elrc"),
		"inside library":     filepath.Join(f.root, "b.jsonl"),
		"inside library dir": filepath.Join(f.dir, "b.jsonl"),
	}
	for name, backup := range cases {
		out, code := f.run(t, false, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Yes: true, Backup: backup})
		if code != 2 || !strings.Contains(out, "--backup must not be") {
			t.Errorf("%s: exit = %d; want 2 with a refusal:\n%s", name, code, out)
		}
		qiNoLeak(t, name, out, f)
		if _, err := os.Stat(backup); err == nil {
			t.Errorf("%s: backup file was created", name)
		}
	}
	if marked := f.row(t, f.id); marked || f.markerExists() {
		t.Errorf("a refused run changed state: marked=%v marker=%v", marked, f.markerExists())
	}
	if b, _ := os.ReadFile(f.lrcPath); string(b) != qiLRC {
		t.Errorf("lrc not intact: %q", b)
	}
}

func TestQueueInstrumentalTightensAnExistingBackupFile(t *testing.T) {
	f := newQIFixture(t)
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	if err := os.WriteFile(backup, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backup, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := f.run(t, false, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Yes: true, Backup: backup}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	fi, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("existing backup mode = %o; want 600", perm)
	}
}

func TestQueueInstrumentalRefusesSymlinkedBackupToALyric(t *testing.T) {
	f := newQIFixture(t)
	link := filepath.Join(t.TempDir(), "backup.jsonl")
	if err := os.Symlink(f.lrcPath, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	out, code := f.run(t, false, QueueMarkInstrumentalCmd{IDs: []int64{f.id}, Yes: true, Backup: link})
	if code != 2 || !strings.Contains(out, "--backup must not be") {
		t.Errorf("exit = %d; want 2 with a refusal:\n%s", code, out)
	}
	if marked := f.row(t, f.id); marked || f.markerExists() {
		t.Errorf("a refused run changed state: marked=%v marker=%v", marked, f.markerExists())
	}
	if b, _ := os.ReadFile(f.lrcPath); string(b) != qiLRC {
		t.Errorf("lrc not intact: %q", b)
	}
}

func TestQueueInstrumentalPathMatchesExactlyAndCountsOnce(t *testing.T) {
	f := newQIFixture(t)
	unclean := f.dir + "/../" + qiAlbum + "/song.flac"
	for name, paths := range map[string][]string{
		"unclean":   {unclean},
		"relative":  {"song.flac"},
		"dup twice": {filepath.Join(f.dir, "nope.flac"), filepath.Join(f.dir, "nope.flac")},
	} {
		out, code := f.run(t, false, QueueMarkInstrumentalCmd{Paths: paths})
		want := "not found: 1"
		if code != 1 || !strings.Contains(out, want) || strings.Contains(out, "would mark: 1") {
			t.Errorf("%s: exit = %d; want 1 with %q and nothing selected:\n%s", name, code, want, out)
		}
	}
}
