package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
	"github.com/sydlexius/canticle/internal/titlerepair"
)

// Every artist, title, album and lyric line below is invented.
const (
	ritArtist = "Velvet Abacus Trio"
	ritAlbum  = "quietshelf"
	ritLRC    = "[source:musixmatch]\n[00:01.00]Glimmer the drab\n"
	ritNoFile = "-" // row(): write no audio file, so the album read fails
)

// ritSettled is the completed_at of every fixture row.
var ritSettled = time.Date(2022, 3, 4, 5, 6, 7, 0, time.UTC)

// ritFixture is a config, a real database and (unless bare) one library root.
type ritFixture struct {
	ctx                     context.Context
	cfg, dbPath, root, dir  string
	backup, tail, tailApply string
	n                       int
}

// newRITFixture writes a config with the given title-rule mode plus extra TOML.
func newRITFixture(t *testing.T, mode, extra string, bare bool) *ritFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &ritFixture{ctx: context.Background(), cfg: filepath.Join(base, "config.toml"), dbPath: filepath.Join(base, "q.db"),
		root: filepath.Join(base, "music"), backup: filepath.Join(base, "b.jsonl"), tail: filepath.Join(base, "rows.tsv")}
	f.dir, f.tailApply = filepath.Join(f.root, ritAlbum), f.backup+".rows.tsv"
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[db]\npath = %q\n[instrumental_title]\nmode = %q\n%s", f.dbPath, mode, extra)
	if err := os.WriteFile(f.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bare {
		sqlDB, oerr := dbtest.OpenAt(f.ctx, t, f.dbPath)
		if oerr != nil {
			t.Fatal(oerr)
		}
		defer func() { _ = sqlDB.Close() }()
		if _, aerr := library.New(sqlDB).Add(f.ctx, f.root, "lib", models.LibrarySettings{}); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return f
}

// db opens the database for fn; q runs one statement and returns the first
// column of its first row as text ("" when it returns no row).
func (f *ritFixture) db(t *testing.T, fn func(q func(string, ...any) string)) {
	t.Helper()
	sqlDB, err := dbtest.OpenAt(f.ctx, t, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	fn(func(query string, args ...any) string {
		var v string
		rows, qerr := sqlDB.QueryContext(f.ctx, query, args...)
		if qerr != nil {
			t.Fatal(qerr)
		}
		defer func() { _ = rows.Close() }()
		if rows.Next() {
			if serr := rows.Scan(&v); serr != nil {
				t.Fatal(serr)
			}
		}
		return v
	})
}

// row seeds a row musixmatch settled with a synced lyric, Strong by its title,
// whose stored album is plain. Its audio file carries fileAlbum (ritNoFile: no
// audio file at all) and its .lrc is dated off from the settle.
func (f *ritFixture) row(t *testing.T, fileAlbum string, off time.Duration) (id, lrc string) {
	t.Helper()
	return f.titled(t, "(Instrumental)", fileAlbum, off)
}

// titled is row with the given title suffix in place of "(Instrumental)".
func (f *ritFixture) titled(t *testing.T, suffix, fileAlbum string, off time.Duration) (id, lrc string) {
	t.Helper()
	f.n++
	audio := fmt.Sprintf("%d.flac", f.n)
	title := fmt.Sprintf("Paper Lighthouse %d %s", f.n, suffix)
	if fileAlbum != ritNoFile {
		if err := testutil.WriteFLACFileWithComments(f.dir, audio, 44100, 441000, map[string]string{"ALBUM": fileAlbum}); err != nil {
			t.Fatal(err)
		}
	}
	lrc = filepath.Join(f.dir, fmt.Sprintf("%d.lrc", f.n))
	if err := os.WriteFile(lrc, []byte(ritLRC), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lrc, ritSettled.Add(off), ritSettled.Add(off)); err != nil {
		t.Fatal(err)
	}
	f.db(t, func(q func(string, ...any) string) {
		id = q(`INSERT INTO work_queue (artist, title, album, artist_key, title_key, outdir, filename, source_path, status, outcome_type, provider_lane, completed_at)
			VALUES (?, ?, 'Plain Shelf', ?, ?, ?, ?, ?, 'done', 'synced', ?, ?) RETURNING id`, ritArtist, title, normalize.NormalizeKey(ritArtist),
			normalize.NormalizeKey(title), f.dir, audio, filepath.Join(f.dir, audio), providers.Musixmatch, ritSettled.Format(time.RFC3339))
	})
	return id, lrc
}

// state is "status|lane|block count" of one row.
func (f *ritFixture) state(t *testing.T, id string) (s string) {
	t.Helper()
	f.db(t, func(q func(string, ...any) string) {
		s = q(`SELECT status || '|' || COALESCE(provider_lane, 'null') || '|' || (SELECT COUNT(*) FROM lyric_blocks) FROM work_queue WHERE id = ?`, id)
	})
	return s
}

func (f *ritFixture) run(t *testing.T, a ScanReconcileInstrumentalTitlesCmd) (string, int) {
	t.Helper()
	a.ConfigPath = f.cfg
	var out bytes.Buffer
	code := runReconcileInstrumentalTitles(f.ctx, &out, a)
	return out.String(), code
}

// ritNoLeak fails when text names anything private: stdout and the tail file
// carry counts, row ids and fixed words only.
func ritNoLeak(t *testing.T, label, text string, f *ritFixture) {
	t.Helper()
	for _, s := range []string{ritArtist, "Paper Lighthouse", "Plain Shelf", f.root, ritAlbum, ".flac", ".lrc", "Glimmer"} {
		if strings.Contains(text, s) {
			t.Errorf("%s leaks %q:\n%s", label, s, text)
		}
	}
}

func ritWant(t *testing.T, label, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out+"\n", l+"\n") {
			t.Errorf("%s lacks the line %q:\n%s", label, l, out)
		}
	}
}

func ritRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A dry run counts and changes nothing; --yes removes the lyric file dated at
// the settle, with one backup record and one tail line per row, and reopens the
// row. The file dated three hours later is kept and counted not provable. A
// Weak title and an a cappella title are counted and left alone in both modes,
// though their files are dated at the settle too.
func TestReconcileInstrumentalTitlesDryRunThenApply(t *testing.T) {
	f := newRITFixture(t, "strong", "", false)
	good, goodLRC := f.row(t, "", 0)
	late, lateLRC := f.row(t, "", 3*time.Hour)
	weak, weakLRC := f.titled(t, "- Instrumental Demo", "", 0)
	capp, cappLRC := f.titled(t, "(A Cappella)", "", 0)
	// kept fails when a Weak or a cappella row, or its lyric file, was touched.
	kept := func(label string, blocks int) {
		t.Helper()
		for _, r := range []struct{ id, lrc string }{{weak, weakLRC}, {capp, cappLRC}} {
			if got, want := f.state(t, r.id), fmt.Sprintf("done|musixmatch|%d", blocks); got != want || ritRead(t, r.lrc) != ritLRC {
				t.Errorf("%s: row %s = %s, want %s with its lyric file as written", label, r.id, got, want)
			}
		}
	}

	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{Tail: f.tail, Backup: f.backup})
	if code != 0 {
		t.Fatalf("dry run = %d:\n%s", code, out)
	}
	ritWant(t, "dry run", out, "dry run: nothing changed (pass --yes with --backup <file> to repair)", "instrumental_title.mode: strong",
		"rows examined: 4", "no signal or excluded (a cappella, karaoke, sing-along): 1", "weak: 1", "strong: 2", "strong instrumental bracket: 2", "candidates: 2",
		"skipped, not provable (file not dated at the settle): 1", "rows that would be repaired: 1", "lyric files that would be backed up: 1")
	if strings.Contains(out, "rows repaired") || strings.Contains(out, "backup:") {
		t.Errorf("dry run prints an applied line:\n%s", out)
	}
	ritNoLeak(t, "dry run stdout", out, f)
	if got, want := ritRead(t, f.tail), good+"\twould-repair\n"+late+"\tnot-provable\n"; got != want {
		t.Errorf("dry run tail = %q, want %q", got, want)
	}
	if ritRead(t, goodLRC) != ritLRC || f.state(t, good) != "done|musixmatch|0" {
		t.Errorf("dry run changed the row or its file: %s", f.state(t, good))
	}
	kept("dry run", 0)
	for _, p := range []string{f.backup, f.tailApply} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("dry run created %s", filepath.Base(p))
		}
	}

	out, code = f.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: f.backup})
	if code != 0 {
		t.Fatalf("apply = %d:\n%s", code, out)
	}
	ritWant(t, "apply", out, "rows examined: 4", "no signal or excluded (a cappella, karaoke, sing-along): 1", "weak: 1", "candidates: 2", "skipped, not provable (file not dated at the settle): 1", "errors: 0", "rows repaired: 1",
		"lyric files backed up: 1", "lyric files removed: 1", "new blocks: 1", "rows reopened: 1", "backup: "+f.backup)
	ritNoLeak(t, "apply stdout", out, f)
	if _, err := os.Stat(goodLRC); err == nil {
		t.Error("the lyric file dated at the settle was not removed")
	}
	if ritRead(t, lateLRC) != ritLRC || f.state(t, late) != "done|musixmatch|1" {
		t.Errorf("the row whose file is dated outside the window was touched: %s", f.state(t, late))
	}
	if got := f.state(t, good); got != "pending|null|1" {
		t.Errorf("repaired row = %s, want it reopened with one block", got)
	}
	kept("apply", 1)
	records := strings.Split(strings.TrimSpace(ritRead(t, f.backup)), "\n")
	if len(records) != 1 || !strings.Contains(records[0], `"op":"reconcile-instrumental-titles","work_item_id":`+good+`,`) {
		t.Errorf("backup = %q; want one record for row %s", records, good)
	}
	tail := ritRead(t, f.tailApply)
	if want := good + "\trepaired\n" + late + "\tnot-provable\n"; tail != want {
		t.Errorf("apply tail = %q, want %q", tail, want)
	}
	ritNoLeak(t, "apply tail", tail, f)
}

// Each refusal exits non-zero before anything is changed.
// The dry run splits the Weak rows by the verdict stored for them, from the
// column alone, and a Strong row's verdict does not reach the weak lines.
func TestReconcileInstrumentalTitlesSplitsWeakRowsByStoredVerdict(t *testing.T) {
	f := newRITFixture(t, "strong", "", false)
	set := func(id, result string) {
		t.Helper()
		f.db(t, func(q func(string, ...any) string) {
			q("UPDATE work_queue SET instrumental_result = "+result+" WHERE id = ? RETURNING id", id)
		})
	}
	w1, _ := f.titled(t, "- Instrumental Demo", "", 0)
	w2, _ := f.titled(t, "- Instrumental Demo", "", 0)
	f.titled(t, "- Instrumental Demo", "", 0) // never scored: NULL
	strong, _ := f.row(t, "", 0)
	set(w1, "1")
	set(w2, "0")
	set(strong, "1")

	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{Tail: f.tail, Backup: f.backup})
	if code != 0 {
		t.Fatalf("dry run = %d:\n%s", code, out)
	}
	ritWant(t, "dry run", out, "weak: 3", "weak, detector says instrumental: 1", "weak, detector says not instrumental: 1",
		"weak, never scored: 1", "strong: 1", "strong, detector says instrumental: 1")
	ritNoLeak(t, "dry run stdout", out, f)
}

func TestReconcileInstrumentalTitlesRefusals(t *testing.T) {
	f := newRITFixture(t, "off", "", false)
	id, lrc := f.row(t, "", 0)
	untouched := func(label string, f *ritFixture, id, lrc string) {
		t.Helper()
		if ritRead(t, lrc) != ritLRC || f.state(t, id) != "done|musixmatch|0" {
			t.Errorf("%s changed the row or its file: %s", label, f.state(t, id))
		}
		if _, err := os.Stat(f.backup); err == nil {
			t.Errorf("%s wrote a backup file", label)
		}
	}
	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: f.backup})
	if code != 1 || !strings.Contains(out, "refused: instrumental_title.mode is off") {
		t.Errorf("--yes with the mode off = %d:\n%s", code, out)
	}
	untouched("--yes with the mode off", f, id, lrc)
	out, code = f.run(t, ScanReconcileInstrumentalTitlesCmd{})
	if code != 0 {
		t.Errorf("dry run with the mode off = %d:\n%s", code, out)
	}
	ritWant(t, "dry run with the mode off", out, "instrumental_title.mode: off", "the mode is off: --yes is refused until it is on", "rows that would be repaired: 1")

	on := newRITFixture(t, "strong", "", false)
	id, lrc = on.row(t, "", 0)
	if out, code = on.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true}); code != 2 || !strings.Contains(out, "--yes needs --backup <file>") {
		t.Errorf("--yes without --backup = %d:\n%s", code, out)
	}
	untouched("--yes without --backup", on, id, lrc)
	inside := filepath.Join(on.dir, "b.jsonl")
	if out, code = on.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: inside}); code != 2 {
		t.Errorf("--backup inside the library = %d:\n%s", code, out)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Error("a backup was opened inside the library root")
	}
	if out, code = on.run(t, ScanReconcileInstrumentalTitlesCmd{Library: "no-such-library"}); code != 1 || !strings.Contains(out, "not found") {
		t.Errorf("unknown --library = %d:\n%s", code, out)
	}
	// The same file under another spelling, and the database and its sidecars.
	for arg, want := range map[string]string{
		filepath.Dir(on.backup) + "/./" + filepath.Base(on.backup): "must be two different files", // not Join: it cleans
		on.dbPath:          "must not be the database file",
		on.dbPath + "-wal": "must not be the database file",
		on.dbPath + "-shm": "must not be the database file",
	} {
		if out, code = on.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: on.backup, Tail: arg}); code != 2 || !strings.Contains(out, want) {
			t.Errorf("--tail %s = %d, want 2 and %q:\n%s", filepath.Base(arg), code, want, out)
		}
	}
	if out, code = on.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: on.dbPath}); code != 2 || !strings.Contains(out, "must not be the database file") {
		t.Errorf("--backup at the database = %d:\n%s", code, out)
	}
	untouched("the refusals", on, id, lrc)

	bare := newRITFixture(t, "strong", "", true)
	bare.row(t, "", 0)
	if out, code = bare.run(t, ScanReconcileInstrumentalTitlesCmd{}); code != 1 || !strings.Contains(out, "refused: no usable library root") || strings.Contains(out, "rows examined") {
		t.Errorf("no library root = %d:\n%s", code, out)
	}
	if got := usableRoots([]models.Library{{Path: ""}, {Path: "  "}, {Path: string(filepath.Separator)}, {Path: on.root}}); len(got) != 1 || got[0] != on.root {
		t.Errorf("usableRoots = %q; an empty root and a filesystem root must be dropped", got)
	}
}

// A library whose path is the filesystem root is no root at all: a row outside
// every real library is skipped, where "/" as a root would hold every path.
func TestReconcileInstrumentalTitlesIgnoresAFilesystemRootLibrary(t *testing.T) {
	f := newRITFixture(t, "strong", "", false)
	f.db(t, func(q func(string, ...any) string) {
		q(`INSERT INTO libraries (path, name) VALUES (?, 'slash')`, string(filepath.Separator))
	})
	f.dir = filepath.Join(filepath.Dir(f.root), "elsewhere")
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id, lrc := f.row(t, "", 0)
	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: f.backup})
	if code != 0 {
		t.Fatalf("apply = %d:\n%s", code, out)
	}
	ritWant(t, "filesystem-root library", out, "candidates: 1", "skipped, outside library roots: 1", "rows repaired: 0")
	if ritRead(t, lrc) != ritLRC || f.state(t, id) != "done|musixmatch|0" {
		t.Errorf("a row outside every real root was touched: %s", f.state(t, id))
	}
}

// An applied repair keeps the lyric file of a sing-along product, named by the
// title or by the album, byte for byte and with the row still settled. The
// plain instrumental title beside them is repaired.
func TestReconcileInstrumentalTitlesKeepsLyricsOnASingAlongRelease(t *testing.T) {
	f := newRITFixture(t, "strong", "", false)
	// seed is a settled row with the given title whose album, on the row and on
	// the audio file, is the given one. title_key keeps the fixture's unique
	// value, since two rows share a title.
	seed := func(title, album string) (id, lrc string) {
		id, lrc = f.titled(t, "", album, 0)
		f.db(t, func(q func(string, ...any) string) {
			q(`UPDATE work_queue SET title = ?, album = ? WHERE id = ? RETURNING id`, title, album, id)
		})
		return id, lrc
	}
	type row struct{ label, id, lrc string }
	var kept []row
	id, lrc := seed("Glass Harbor (Performance Track)", "Quiet Shelf")
	kept = append(kept, row{"sing-along title", id, lrc})
	id, lrc = seed("Glass Harbor (Instrumental)", "Quiet Shelf (Performance Tracks)")
	kept = append(kept, row{"sing-along album", id, lrc})
	good, goodLRC := seed("Glass Harbor (Instrumental)", "Quiet Shelf")

	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: f.backup})
	if code != 0 {
		t.Fatalf("apply = %d:\n%s", code, out)
	}
	ritWant(t, "apply", out, "rows examined: 3", "no signal or excluded (a cappella, karaoke, sing-along): 2",
		"strong: 1", "rows repaired: 1", "lyric files removed: 1", "rows reopened: 1")
	for _, s := range []string{"Glass Harbor", "Quiet Shelf"} {
		if strings.Contains(out, s) {
			t.Errorf("apply stdout leaks %q:\n%s", s, out)
		}
	}
	for _, r := range kept {
		if _, err := os.Stat(r.lrc); err != nil {
			t.Errorf("%s: the lyric file was removed: %v", r.label, err)
			continue
		}
		if got := f.state(t, r.id); ritRead(t, r.lrc) != ritLRC || got != "done|musixmatch|1" {
			t.Errorf("%s: row = %s, want it still settled with its lyric file as written", r.label, got)
		}
	}
	if _, err := os.Stat(goodLRC); err == nil {
		t.Error("the lyric file of the plain instrumental title was not removed")
	}
	if got := f.state(t, good); got != "pending|null|1" {
		t.Errorf("plain instrumental row = %s, want it reopened with one block", got)
	}
}

// The album is the audio FILE's, as the worker sees it: a karaoke album on the
// file withdraws the Strong verdict whatever the row stores, and a file that
// cannot be read is skipped as unreadable, never treated as having no album.
// With enrichment off the worker never reads the file, so neither does this.
func TestReconcileInstrumentalTitlesClassifiesWithTheFilesAlbum(t *testing.T) {
	seed := func(f *ritFixture) {
		f.row(t, "Karaoke Nights", 0)
		f.row(t, ritNoFile, 0)
	}
	f := newRITFixture(t, "strong", "", false)
	seed(f)
	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{})
	if code != 0 {
		t.Fatalf("dry run = %d:\n%s", code, out)
	}
	ritWant(t, "enrichment on", out, "no signal or excluded (a cappella, karaoke, sing-along): 1", "strong: 1",
		"strong, skipped, audio file unreadable: 1", "candidates: 0", "rows that would be repaired: 0")

	off := newRITFixture(t, "strong", "[enrichment]\nenabled = false\n", false)
	seed(off)
	out, _ = off.run(t, ScanReconcileInstrumentalTitlesCmd{})
	ritWant(t, "enrichment off", out, "strong: 2", "strong, skipped, audio file unreadable: 0", "candidates: 2")
}

// The worker answers the album from the audio_metadata row when it describes
// the file on disk, without opening the file; so does the command. The file's
// own tag is plain, the cached album is a karaoke one, and the cache wins.
func TestReconcileInstrumentalTitlesReadsTheMetadataCacheLikeTheWorker(t *testing.T) {
	f := newRITFixture(t, "strong", "", false)
	f.row(t, "Plain Shelf", 0)
	audio := filepath.Join(f.dir, "1.flac")
	info, err := os.Stat(audio)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := dbtest.OpenAt(f.ctx, t, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	err = audiometa.New(sqlDB, scanner.DurationReaderVersion).Record(f.ctx, pathutil.CanonicalPath(audio),
		scanner.AudioFacts{MTimeNano: info.ModTime().UnixNano(), SizeBytes: info.Size(), Album: "Karaoke Nights"})
	if cerr := sqlDB.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{})
	if code != 0 {
		t.Fatalf("dry run = %d:\n%s", code, out)
	}
	ritWant(t, "cache hit", out, "no signal or excluded (a cappella, karaoke, sing-along): 1", "strong: 0", "candidates: 0")

	// A row that no longer matches the file is a miss: the file's plain tag stands.
	if err := os.Chtimes(audio, ritSettled, ritSettled); err != nil {
		t.Fatal(err)
	}
	out, _ = f.run(t, ScanReconcileInstrumentalTitlesCmd{})
	ritWant(t, "stale cache row", out, "strong: 1", "candidates: 1")
}

// A backup record that cannot be written fails the row: the file stays, the
// row stays settled, and the exit code is non-zero.
func TestReconcileInstrumentalTitlesBackupFailureIsAnErrorExit(t *testing.T) {
	f := newRITFixture(t, "strong", "", false)
	id, lrc := f.row(t, "", 0)
	next, nextLRC := f.row(t, "", 0)
	if err := os.Mkdir(f.backup, 0o755); err != nil { // a directory cannot be appended to
		t.Fatal(err)
	}
	out, code := f.run(t, ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: f.backup, Tail: f.tail})
	if code != 1 {
		t.Errorf("exit code = %d, want 1 when a row errors:\n%s", code, out)
	}
	ritWant(t, "failed backup", out, "errors: 1", "rows repaired: 0", "lyric files removed: 0",
		"errors: a failed row may be part-way (files removed, row not reopened); a rerun reports where each row stands",
		"failed: the --backup file could not be written; the run was stopped",
		"not finished, the counts above are partial: titlerepair: stopped after id "+id+": context canceled")
	// The first failed record stops the run: no later row appends onto a torn line.
	if ritRead(t, nextLRC) != ritLRC || f.state(t, next) != "done|musixmatch|0" {
		t.Errorf("the run went on after a failed backup record: %s", f.state(t, next))
	}
	if ritRead(t, lrc) != ritLRC || f.state(t, id) != "done|musixmatch|0" {
		t.Errorf("a lyric file was removed with no backup record: %s", f.state(t, id))
	}
	if got := ritRead(t, f.tail); got != id+"\terror\n" {
		t.Errorf("tail = %q, want the errored row", got)
	}
}

// ritFailTail is a --tail file that takes every line and cannot be synced.
type ritFailTail struct{ bytes.Buffer }

func (*ritFailTail) Sync() error  { return errors.New("sync refused") }
func (*ritFailTail) Close() error { return nil }

// A tail file that cannot be synced fails the run. Applied, the first row is
// repaired before its line fails, so the run stops there and names that row:
// its backup record is then the only one with no tail line.
func TestReconcileInstrumentalTitlesTailFailureStopsTheRun(t *testing.T) {
	f := newRITFixture(t, "strong", "", false)
	first, firstLRC := f.row(t, "", 0)
	second, secondLRC := f.row(t, "", 0)
	run := func(a ScanReconcileInstrumentalTitlesCmd) (string, int) {
		a.ConfigPath = f.cfg
		var out bytes.Buffer
		code := reconcileInstrumentalTitles(f.ctx, &out, a, func(string) (tailFile, error) { return &ritFailTail{}, nil })
		return out.String(), code
	}
	const failed = "failed: the --tail file could not be written; the run was stopped"

	// A dry run syncs once, at the end: the run finishes, and still fails.
	out, code := run(ScanReconcileInstrumentalTitlesCmd{Tail: f.tail})
	if code != 1 || strings.Contains(out, "not finished") || strings.Contains(out, "has no line") {
		t.Errorf("dry run with an unsyncable tail = %d, want 1 from the tail alone:\n%s", code, out)
	}
	ritWant(t, "dry run", out, failed, "rows that would be repaired: 2")

	out, code = run(ScanReconcileInstrumentalTitlesCmd{Yes: true, Backup: f.backup})
	if code != 1 {
		t.Errorf("apply with an unsyncable tail = %d, want 1:\n%s", code, out)
	}
	ritWant(t, "apply", out, failed, "rows repaired: 1", "row "+first+" (repaired) has no line in the --tail file",
		"not finished, the counts above are partial: titlerepair: stopped after id "+first+": context canceled")
	ritNoLeak(t, "apply stdout", out, f)
	if _, err := os.Stat(firstLRC); err == nil || f.state(t, first) != "pending|null|1" {
		t.Errorf("the first row was not repaired: %s", f.state(t, first))
	}
	if records := strings.Split(strings.TrimSpace(ritRead(t, f.backup)), "\n"); len(records) != 1 {
		t.Errorf("backup holds %d records, want the first row's only", len(records))
	}
	if ritRead(t, secondLRC) != ritLRC || f.state(t, second) != "done|musixmatch|1" {
		t.Errorf("the run went on after the tail failed: %s", f.state(t, second))
	}
}

// A row needing a manual mark is unfinished work: its note is printed and the
// exit code is 1, as for an errored row. Skips alone exit 0.
func TestTitleRepairExit(t *testing.T) {
	for _, tc := range []struct {
		res  titlerepair.Result
		code int
		note string
	}{
		{titlerepair.Result{NotProvable: 3, NoFile: 1}, 0, ""},
		{titlerepair.Result{NeedsManualMark: 1}, 1, "rows needing a manual mark"},
		{titlerepair.Result{Errors: 1}, 1, "errors: a failed row may be part-way"},
	} {
		var out bytes.Buffer
		if code := titleRepairExit(&out, tc.res); code != tc.code || !strings.HasPrefix(out.String(), tc.note) || (tc.note == "") != (out.Len() == 0) {
			t.Errorf("titleRepairExit(%+v) = %d, %q; want %d and a note starting %q", tc.res, code, out.String(), tc.code, tc.note)
		}
	}
}
