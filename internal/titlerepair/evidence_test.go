package titlerepair

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/providers"
)

// Every artist, title and lyric line below is invented.

const (
	shelfArtist = "Tin Lantern Band"
	lrcWords    = "[00:01.00]Zorp the blent\n[00:05.00]Frabble on\n"
	lrcBody     = "[source:musixmatch]\n" + lrcWords
	plainWords  = "Zorp the blent\nFrabble on\n"
	year        = 365 * 24 * time.Hour
)

// settled is the completed_at of every shelf row.
var settled = time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)

// shelf is one library root ("music") with one album directory, over a real
// database kept outside it.
type shelf struct {
	d         *sql.DB
	base, dir string
	n         int
}

// track is one shelf row: stem+".lrc" and stem+".txt" are its lyric files.
type track struct {
	id          int64
	title, stem string
}

func newShelf(t *testing.T) *shelf {
	t.Helper()
	// Resolved, as Mark resolves an output directory before it names a file.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &shelf{d: dbtest.Open(t), base: base, dir: filepath.Join(base, "music", "album")}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *shelf) opts(apply bool, report func(lyricblock.Backup) error) Options {
	return Options{Roots: []string{filepath.Join(s.base, "music")}, Apply: apply, Report: report}
}

// row inserts a row a provider lane settled with a synced lyric, Strong by its
// title, then applies set (a SQL SET list) when it is not empty.
func (s *shelf) row(t *testing.T, set string) track {
	t.Helper()
	s.n++
	tr := track{title: fmt.Sprintf("Glass Harbor %d (Instrumental)", s.n), stem: filepath.Join(s.dir, fmt.Sprint(s.n))}
	if err := s.d.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename, status, outcome_type, provider_lane, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, 'done', 'synced', ?, ?) RETURNING id`, shelfArtist, tr.title, normalize.NormalizeKey(shelfArtist),
		normalize.NormalizeKey(tr.title), s.dir, fmt.Sprintf("%d.flac", s.n), providers.Musixmatch, settled.Format(time.RFC3339)).Scan(&tr.id); err != nil {
		t.Fatal(err)
	}
	if set != "" {
		s.exec(t, `UPDATE work_queue SET `+set+` WHERE id = ?`, tr.id)
	}
	return tr
}

func (s *shelf) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := s.d.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// get returns the one text value query selects.
func (s *shelf) get(t *testing.T, query string, args ...any) string {
	t.Helper()
	var v string
	if err := s.d.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// rowState is "status|lane|outcome|blocks under the row's keys".
func (s *shelf) rowState(t *testing.T, id int64) string {
	t.Helper()
	return s.get(t, `SELECT status || '|' || COALESCE(provider_lane, 'null') || '|' || COALESCE(outcome_type, 'null') || '|' ||
		(SELECT COUNT(*) FROM lyric_blocks b WHERE b.artist_key = w.artist_key AND b.title_key = w.title_key) FROM work_queue w WHERE id = ?`, id)
}

// put writes a lyric file whose mtime is off from the rows' completed_at.
func put(t *testing.T, path, body string, off time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, settled.Add(off), settled.Add(off)); err != nil {
		t.Fatal(err)
	}
}

// cacheWords stores plainWords as the cached lyric of artist and title.
func (s *shelf) cacheWords(t *testing.T, artist, title string) {
	t.Helper()
	song := models.Song{Track: models.Track{ArtistName: artist, TrackName: title}, Lyrics: models.Lyrics{LyricsBody: plainWords}}
	entry, err := lyrics.EncodeCachedSong(song)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.New(s.d).Store(context.Background(), artist, title, 240, entry); err != nil {
		t.Fatal(err)
	}
}

// block blocks the shelf's words under the row's keys.
func (s *shelf) block(t *testing.T, tr track) {
	t.Helper()
	if _, err := lyricblock.NewStore(s.d, nil).Add(context.Background(), s.d, lyricblock.Block{
		ArtistKey: shelfArtist, TitleKey: tr.title, Fingerprint: lyricblock.Fingerprint(lrcBody),
	}); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// state is everything a dry run or a refusal must leave alone: every file under
// the shelf (name, mode, mtime, bytes) and every column of every queue row,
// block and cache entry.
func (s *shelf) state(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(s.base, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		fmt.Fprintf(&b, "%s %v %d %q\n", p, fi.Mode(), fi.ModTime().UnixNano(), body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"work_queue", "lyric_blocks", "lyrics_cache"} {
		rows, err := s.d.Query(`SELECT * FROM ` + table + ` ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, table, vals)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// A dry run reads and vets and changes nothing. The apply then backs the exact
// bytes up, removes the file, blocks the words under the row's keys, drops the
// cache entry (stored under the album artist, as the worker stores it) and
// reopens the row. The file's mtime is a year off: the cache entry alone proves it.
func TestDryRunChangesNothingThenApplyRepairs(t *testing.T) {
	const albumArtist = "Copper Finch Trio"
	s := newShelf(t)
	tr := s.row(t, `album_artist = '`+albumArtist+`'`)
	put(t, tr.stem+".lrc", lrcBody, -year)
	s.cacheWords(t, albumArtist, tr.title)
	before := s.state(t)
	res, seen := run(t, s.d, s.opts(false, func(lyricblock.Backup) error { t.Error("Report called in a dry run"); return nil }))
	if res.WouldRepair != 1 || res.BackedUp != 1 || res.Repaired+res.Removed+res.NewBlocks+res.CacheInvalidated+res.Reopened != 0 ||
		seen[tr.id] != DispositionWouldRepair {
		t.Errorf("dry run: %+v, disposition %q; want one would-repair with one file", res, seen[tr.id])
	}
	if s.state(t) != before {
		t.Error("the dry run changed a file, a row, a block or a cache entry")
	}

	backup, err := os.Create(filepath.Join(t.TempDir(), "backup.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	o := s.opts(true, func(b lyricblock.Backup) error { return lyricblock.AppendBackup(backup, b) })
	var files []string
	o.OnRow = func(r Row) { files = r.Files }
	res, seen = run(t, s.d, o)
	if res.Repaired != 1 || res.WouldRepair != 0 || res.BackedUp != 1 || res.Removed != 1 || res.NewBlocks != 1 ||
		res.CacheInvalidated != 1 || res.Reopened != 1 || seen[tr.id] != DispositionRepaired {
		t.Errorf("apply: %+v, disposition %q; want one row repaired", res, seen[tr.id])
	}
	raw, err := os.ReadFile(backup.Name())
	if err != nil {
		t.Fatal(err)
	}
	var rec lyricblock.Backup
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("the backup is not one JSON record: %v", err)
	}
	if string(rec.Content) != lrcBody || rec.Path != tr.stem+".lrc" || rec.WorkItemID != tr.id || rec.Op != "reconcile-instrumental-titles" {
		t.Errorf("backup record = %+v, want the file's exact bytes under this repair's op", rec)
	}
	if !slices.Equal(files, []string{tr.stem + ".lrc"}) {
		t.Errorf("Row.Files = %v, want the one lyric file", files)
	}
	if got := s.rowState(t, tr.id) + s.get(t, `SELECT '|' || COUNT(*) FROM lyrics_cache`); exists(tr.stem+".lrc") || got != "pending|null|null|1|0" {
		t.Errorf("after apply: file exists %v, row %q; want it gone and pending|null|null|1|0", exists(tr.stem+".lrc"), got)
	}
}

// Each skip refuses its row and changes nothing, under Apply, and the run goes
// on to the next row. Apply with no Report is refused before any row is read.
// Three minutes either side is outside the window. A file touched long after the
// settle is refused though its words are cached or blocked; another title's cache proves nothing.
func TestSkipCountersChangeNothing(t *testing.T) {
	s := newShelf(t)
	elsewhere := t.TempDir()
	var want []string
	add := func(disposition, set string, files func(tr track)) {
		tr := s.row(t, set)
		files(tr)
		want = append(want, disposition)
	}
	inWindow := func(body string) func(track) {
		return func(tr track) { put(t, tr.stem+".lrc", body, time.Minute) }
	}
	add(DispositionSymlink, "", func(tr track) {
		put(t, filepath.Join(s.dir, "target"), lrcBody, 0)
		if err := os.Symlink(filepath.Join(s.dir, "target"), tr.stem+".lrc"); err != nil {
			t.Fatal(err)
		}
	})
	add(DispositionOutsideRoots, `outdir = '`+elsewhere+`'`, func(tr track) {
		put(t, filepath.Join(elsewhere, filepath.Base(tr.stem)+".lrc"), lrcBody, 0)
	})
	add(DispositionSourceDisagrees, "", inWindow("[source:petitlyrics]\n"+lrcWords))
	add(DispositionNotProvable, "", func(tr track) { put(t, tr.stem+".lrc", lrcBody, 3*time.Minute) })
	add(DispositionNotProvable, "", func(tr track) { put(t, tr.stem+".lrc", lrcBody, -3*time.Minute) })
	add(DispositionNotProvable, "", func(tr track) {
		s.cacheWords(t, shelfArtist, tr.title)
		put(t, tr.stem+".lrc", lrcBody, year)
	})
	add(DispositionNotProvable, "", func(tr track) {
		s.block(t, tr)
		put(t, tr.stem+".lrc", lrcBody, year)
	})
	add(DispositionNotProvable, "", func(tr track) {
		s.cacheWords(t, shelfArtist, "Paper Orchard (Instrumental)")
		put(t, tr.stem+".lrc", lrcBody, -year)
	})
	add(DispositionNotProvable, `completed_at = 'last spring'`, inWindow(lrcBody))
	add(DispositionStemOccupied, "", func(tr track) {
		put(t, tr.stem+".lrc", lrcBody, 0)
		put(t, tr.stem+".txt", "[by:canticle]\n[source:manual]\n♪ Instrumental ♪\n", 0)
	})
	add(DispositionNoFile, "", func(track) {})
	add(DispositionNoFile, "", inWindow("[source:musixmatch]\n[00:00.00]♪ Instrumental ♪\n"))
	before := s.state(t)
	if res, err := New(s.d, nil).Run(context.Background(), s.opts(true, nil)); err == nil || res.Examined != 0 {
		t.Errorf("Apply with no Report: %+v, %v; want an error before any row", res, err)
	}
	res, seen := run(t, s.d, s.opts(true, func(lyricblock.Backup) error { t.Error("Report called for a refused row"); return nil }))
	for i, d := range want {
		if got := seen[int64(i+1)]; got != d {
			t.Errorf("row %d: disposition %q, want %q", i+1, got, d)
		}
	}
	if res.Symlink != 1 || res.OutsideRoots != 1 || res.SourceDisagrees != 1 || res.NotProvable != 6 || res.StemOccupied != 1 ||
		res.NoFile != 2 || res.Candidates != len(want) {
		t.Errorf("counts %+v, want one of each skip, six not provable and two with no file", res)
	}
	if s.state(t) != before {
		t.Error("a refused row had a file, a column, a block or a cache entry changed")
	}
}

// Each proof alone is enough: an mtime just after the settle (with a [source:]
// tag in another case), one just before it on an untagged .txt, and words
// already blocked under the row's keys, which is how a repair that crashed
// after Mark's first transaction resumes.
func TestEachProofAloneSuffices(t *testing.T) {
	s := newShelf(t)
	after, earlier, blocked := s.row(t, ""), s.row(t, `outcome_type = 'unsynced'`), s.row(t, "")
	put(t, after.stem+".lrc", "[source: MusixMatch ]\n"+lrcWords, 90*time.Second)
	put(t, earlier.stem+".txt", plainWords, -90*time.Second)
	put(t, blocked.stem+".lrc", lrcBody, -year)
	s.block(t, blocked)
	res, _ := run(t, s.d, s.opts(true, func(lyricblock.Backup) error { return nil }))
	if res.Repaired != 3 || res.Removed != 3 || res.NewBlocks != 2 || res.Reopened != 3 {
		t.Errorf("%+v; want three rows repaired, two of them newly blocked", res)
	}
	for _, p := range []string{after.stem + ".lrc", earlier.stem + ".txt", blocked.stem + ".lrc"} {
		if exists(p) {
			t.Errorf("%s was not removed", filepath.Base(p))
		}
	}
}

// A backup that cannot be written leaves its row exactly as it was and counts
// an error; the run goes on and repairs the next row. The log names the row by
// id and carries nothing of its path or title.
func TestFailingReportChangesNothingAndTheRunContinues(t *testing.T) {
	s := newShelf(t)
	first, second := s.row(t, ""), s.row(t, "")
	put(t, first.stem+".lrc", lrcBody, 0)
	put(t, second.stem+".lrc", lrcBody, 0)
	// The first line of the state is the first row's file: mode, mtime and bytes.
	firstRow, firstFile := s.rowState(t, first.id), strings.SplitN(s.state(t), "\n", 2)[0]
	var logged bytes.Buffer
	res, seen := runLog(t, s.d, s.opts(true, func(b lyricblock.Backup) error {
		if b.WorkItemID == first.id {
			return errors.New("disk full")
		}
		return nil
	}), slog.New(slog.NewTextHandler(&logged, nil)))
	if res.Errors != 1 || res.Repaired != 1 || seen[first.id] != DispositionError || seen[second.id] != DispositionRepaired {
		t.Errorf("%+v, dispositions %v; want the first row an error and the second repaired", res, seen)
	}
	if s.rowState(t, first.id) != firstRow || !strings.HasPrefix(s.state(t), firstFile+"\n") || exists(second.stem+".lrc") {
		t.Error("the row whose backup failed was changed, or the next row was not repaired")
	}
	if got := logged.String(); !strings.Contains(got, fmt.Sprintf("row failed; a rerun reports where it stands\" id=%d", first.id)) ||
		strings.Contains(got, s.base) || strings.Contains(got, "Glass Harbor") || strings.Contains(got, "disk full") {
		t.Errorf("log %q; want the failed row's id and no path, title or cause", got)
	}
}

// A row a worker claims, or an operator edits or marks, after the page was read
// is refused by Mark and counted with the rows found so at selection.
func TestActTimeInFlightAndHandMarked(t *testing.T) {
	s := newShelf(t)
	rows := []track{s.row(t, ""), s.row(t, ""), s.row(t, ""), s.row(t, "")}
	for _, tr := range rows {
		put(t, tr.stem+".lrc", lrcBody, 0)
	}
	o := s.opts(true, func(lyricblock.Backup) error { return nil })
	o.OnRow = func(r Row) {
		if r.ID == rows[0].id {
			s.exec(t, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, rows[1].id)
			s.exec(t, `UPDATE work_queue SET lyric_edited_at = '2026-01-02T03:04:05Z' WHERE id = ?`, rows[2].id)
			s.exec(t, `UPDATE work_queue SET manual_instrumental_at = '2026-01-02T03:04:05Z' WHERE id = ?`, rows[3].id)
		}
	}
	res, seen := run(t, s.d, o)
	if res.Repaired != 1 || res.InFlight != 1 || res.HandMarked != 2 || res.Candidates != 1 ||
		seen[rows[1].id] != DispositionInFlight || seen[rows[2].id] != DispositionHandMarked || seen[rows[3].id] != DispositionHandMarked {
		t.Errorf("%+v, dispositions %v; want one repaired, one in flight, two hand-marked", res, seen)
	}
	for _, tr := range rows[1:] {
		if !exists(tr.stem+".lrc") || !strings.HasSuffix(s.rowState(t, tr.id), "|musixmatch|synced|0") {
			t.Errorf("row %d was changed: %s", tr.id, s.rowState(t, tr.id))
		}
	}
}

// A file that appears after the first vet, in the window, is vetted, backed up,
// counted and removed with the first. A row hand-marked while its late file is
// backed up cannot be reopened: an error, still a candidate, not a hand-marked skip.
func TestLateFileIsCountedAndAMidRepairHandMarkIsAnError(t *testing.T) {
	s := newShelf(t)
	clean, marked := s.row(t, ""), s.row(t, "")
	put(t, clean.stem+".lrc", lrcBody, 0)
	put(t, marked.stem+".lrc", lrcBody, 0)
	res, seen := run(t, s.d, s.opts(true, func(b lyricblock.Backup) error {
		if tr := map[int64]track{clean.id: clean, marked.id: marked}[b.WorkItemID]; !exists(tr.stem + ".txt") {
			put(t, tr.stem+".txt", plainWords, 0)
		} else if tr.id == marked.id {
			s.exec(t, `UPDATE work_queue SET manual_instrumental_at = '2026-01-02T03:04:05Z' WHERE id = ?`, tr.id)
		}
		return nil
	}))
	if res.Repaired != 1 || res.BackedUp != 2 || res.Removed != 2 || res.Errors != 1 || res.HandMarked != 0 || res.Candidates != 2 ||
		seen[clean.id] != DispositionRepaired || seen[marked.id] != DispositionError {
		t.Errorf("%+v, dispositions %v; want one row repaired with two files backed up and one error", res, seen)
	}
}

// A file that appears after the first vet, touched long after the settle, fails
// Mark's late vet although its words were blocked a moment before: the row stays
// done and blocked with every file in place. It is counted on its own, not as a
// skip, and a rerun refuses it at the first vet without touching it.
func TestLateFileNeedsManualMark(t *testing.T) {
	s := newShelf(t)
	tr := s.row(t, "")
	put(t, tr.stem+".lrc", lrcBody, 0)
	o := s.opts(true, func(lyricblock.Backup) error {
		put(t, tr.stem+".txt", plainWords, year)
		return nil
	})
	res, seen := run(t, s.d, o)
	if res.NeedsManualMark != 1 || res.NotProvable != 0 || res.Repaired != 0 || seen[tr.id] != DispositionNeedsManualMark {
		t.Errorf("%+v, disposition %q; want one row needing a manual mark", res, seen[tr.id])
	}
	left := func() bool {
		return exists(tr.stem+".lrc") && exists(tr.stem+".txt") && s.rowState(t, tr.id) == "done|musixmatch|synced|1"
	}
	if !left() {
		t.Errorf("after the late vet: row %s; want it done, blocked once, both files in place", s.rowState(t, tr.id))
	}
	if res, _ := run(t, s.d, o); res.NotProvable != 1 || res.NeedsManualMark != 0 || !left() {
		t.Errorf("rerun: %+v, row %s; want the row refused as not provable and left alone", res, s.rowState(t, tr.id))
	}
}
