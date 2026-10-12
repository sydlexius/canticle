package scanner_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/scanfail"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// Tests for the scanner's tag-cache identity read (#1574). Cache rows are
// written by the production audiometa.Store.Record from real ReadAudioFacts
// output and wired with the production WithIdentityStore option.
//
// "The file was not read" is proven by content, not by a counter: the audio
// bytes are replaced with same-size garbage and the mtime restored, so a scan
// that opened the file would fail its tag read and emit no row.

const (
	hitISRC = "ZZXXX2600001"
	hitMBID = "11111111-2222-4333-8444-555555555555"
)

// storeAdapter mirrors the adapter internal/commands injects.
type storeAdapter struct{ store *audiometa.Store }

func (a storeAdapter) Identity(ctx context.Context, path string, mtimeNano, size int64) (scanner.CachedIdentity, bool, error) {
	id, found, err := a.store.Identity(ctx, path, mtimeNano, size)
	return scanner.CachedIdentity(id), found, err
}

// errStore is an identity store whose every lookup fails with mk(path).
type errStore struct{ mk func(path string) error }

func (e errStore) Identity(_ context.Context, path string, _, _ int64) (scanner.CachedIdentity, bool, error) {
	return scanner.CachedIdentity{}, false, e.mk(path)
}

// hitEnv is one library root, one real database, and a scanner wired to the
// tag cache, the duration cache and (optionally) the metadata-failure store.
type hitEnv struct {
	t    *testing.T
	ctx  context.Context
	root string
	db   *sql.DB
	sc   *scanner.Scanner
}

func newHitEnv(t *testing.T, withFailures bool) hitEnv {
	t.Helper()
	db := dbtest.Open(t)
	opts := []scanner.Option{
		scanner.WithDurationStore(audiodur.New(db, scanner.DurationReaderVersion)),
		scanner.WithIdentityStore(storeAdapter{audiometa.New(db, scanner.DurationReaderVersion)}),
	}
	if withFailures {
		opts = append(opts, scanner.WithMetadataFailureStore(scanfail.New(db)))
	}
	return hitEnv{t: t, ctx: context.Background(), root: t.TempDir(), db: db, sc: scanner.NewScanner(opts...)}
}

// flac writes name under the root with a 187s duration and the given title.
func (e hitEnv) flac(name, title string) string {
	e.t.Helper()
	if err := testutil.WriteFLACFileWithComments(e.root, name, 44100, 44100*187, map[string]string{
		"ARTIST": "Marlow Finch", "TITLE": title, "ISRC": hitISRC, "MUSICBRAINZ_TRACKID": hitMBID,
	}); err != nil {
		e.t.Fatal(err)
	}
	return filepath.Join(e.root, name)
}

// seed records path's tags in audio_metadata under the canonical key, as
// `scan index-metadata` does.
func (e hitEnv) seed(path string) {
	e.t.Helper()
	facts, err := scanner.ReadAudioFacts(path)
	if err != nil {
		e.t.Fatalf("ReadAudioFacts: %v", err)
	}
	absRoot, canonRoot := pathutil.CanonicalRoot(e.root)
	key := pathutil.RebaseUnderCanonicalRoot(absRoot, canonRoot, path)
	if err := audiometa.New(e.db, scanner.DurationReaderVersion).Record(e.ctx, key, facts); err != nil {
		e.t.Fatalf("Record: %v", err)
	}
}

func (e hitEnv) exec(query string) {
	e.t.Helper()
	if _, err := e.db.ExecContext(e.ctx, query); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
}

// durations renders audio_durations as "seconds,seconds,...".
func (e hitEnv) durations() string {
	e.t.Helper()
	var got sql.NullString
	if err := e.db.QueryRowContext(e.ctx, `SELECT group_concat(duration_seconds) FROM audio_durations`).Scan(&got); err != nil {
		e.t.Fatal(err)
	}
	return got.String
}

// scan returns each row as "base name|artist|title|isrc|mbid|seconds", plus
// the scanner's one aggregate cache log line.
func (e hitEnv) scan(sc *scanner.Scanner, opts scanner.ScanOptions) (rows []string, logLine string) {
	e.t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	opts.MaxDepth = 1
	res, err := sc.ScanLibrary(e.ctx, e.root, opts)
	if err != nil {
		e.t.Fatalf("ScanLibrary: %v", err)
	}
	for _, r := range res {
		tr := r.Track
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%s|%d", filepath.Base(r.FilePath), tr.ArtistName, tr.TrackName, tr.ISRC, tr.RecordingMBID, tr.TrackLength))
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `msg="scan identity cache"`) {
			logLine = line
		}
	}
	return rows, logLine
}

// corruptInPlace overwrites path with same-size garbage and restores its
// mtime, then proves the (mtime, size) stamp survived. A file read after this
// cannot yield tags.
func corruptInPlace(t *testing.T, path string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(os.WriteFile(path, bytes.Repeat([]byte{'Z'}, int(before.Size())), 0o600),
		os.Chtimes(path, time.Now(), before.ModTime())); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("stamp did not survive the overwrite (err %v): %v -> %v", err, before, after)
	}
	if _, err := scanner.ReadAudioFacts(path); err == nil {
		t.Fatal("corrupted file still reads as tagged audio; the no-read proof would be vacuous")
	}
}

// A hit is served without opening the file. With recording enrichment off it
// carries no ISRC, recording ID or duration and banks nothing; with it on it
// carries all three and banks the cached duration once, never rewriting it.
func TestIdentityCacheHit_EnrichmentSwitch(t *testing.T) {
	e := newHitEnv(t, false)
	path := e.flac("a.flac", "Quiet Harbor")
	e.seed(path)
	corruptInPlace(t, path)

	rows, _ := e.scan(e.sc, scanner.ScanOptions{})
	if want := []string{"a.flac|Marlow Finch|Quiet Harbor|||0"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("enrichment off: rows = %q, want %q", rows, want)
	}
	if d := e.durations(); d != "" {
		t.Errorf("enrichment off banked duration(s) %q", d)
	}

	rows, line := e.scan(e.sc, scanner.ScanOptions{EnrichRecording: true})
	if want := []string{"a.flac|Marlow Finch|Quiet Harbor|" + hitISRC + "|" + hitMBID + "|187"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("enrichment on: rows = %q, want %q", rows, want)
	}
	if d := e.durations(); d != "187" {
		t.Errorf("enrichment on banked %q, want the cached 187", d)
	}
	if !strings.Contains(line, "level=DEBUG") || !strings.Contains(line, "hits=1 misses=0 lookup_errors=0 duration_bank_errors=0") ||
		strings.Contains(line, "flac") || strings.Contains(line, "Marlow") {
		t.Errorf("cache log line = %q, want DEBUG, hits=1 only, and no library detail", line)
	}

	// An already-banked duration is not written again: a marked row survives.
	e.exec(`UPDATE audio_durations SET duration_seconds = 999`)
	e.scan(e.sc, scanner.ScanOptions{EnrichRecording: true})
	if d := e.durations(); d != "999" {
		t.Errorf("a rescan rewrote the banked duration: %q, want the marked 999", d)
	}

	// A hit whose duration cannot be banked is still served, and counted.
	e.exec(`DROP TABLE audio_durations`)
	rows, line = e.scan(e.sc, scanner.ScanOptions{EnrichRecording: true})
	if len(rows) != 1 || !strings.Contains(line, "level=WARN") || !strings.Contains(line, "hits=1 misses=0 lookup_errors=0 duration_bank_errors=1") {
		t.Errorf("failed bank: %d row(s), log %q; want the row and WARN with duration_bank_errors=1", len(rows), line)
	}
}

// A row that is not current for the file is a miss and the file is read: a
// changed mtime, a changed size, or a row another tag reader wrote.
func TestIdentityCacheMiss_RereadsTheFile(t *testing.T) {
	for _, tc := range []struct {
		name, title string // title the file is rewritten with after seeding
		keepMtime   bool
		sql, want   string
	}{
		{"same stamp is served", "Noisy Harbor", true, "", "Quiet Harbor"}, // the hit the rest must not be
		{"mtime", "Noisy Harbor", false, "", "Noisy Harbor"},               // same size, new mtime
		{"size", "A Much Longer Harbor Title", true, "", "A Much Longer Harbor Title"},
		{"other tag reader", "Noisy Harbor", true, `UPDATE audio_metadata SET tag_reader_version = tag_reader_version + 1`, "Noisy Harbor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHitEnv(t, false)
			path := e.flac("a.flac", "Quiet Harbor")
			e.seed(path)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			e.flac("a.flac", tc.title)
			mtime := before.ModTime()
			if !tc.keepMtime {
				mtime = mtime.Add(2 * time.Hour)
			}
			if err := os.Chtimes(path, time.Now(), mtime); err != nil {
				t.Fatal(err)
			}
			if tc.sql != "" {
				e.exec(tc.sql)
			}

			rows, _ := e.scan(e.sc, scanner.ScanOptions{})
			if want := []string{"a.flac|Marlow Finch|" + tc.want + "|||0"}; !reflect.DeepEqual(rows, want) {
				t.Errorf("rows = %q, want %q", rows, want)
			}
		})
	}
}

// An embedded-lyrics mode bypasses the cache: the file is read and its
// embedded lyrics are respected or extracted, so it yields no row. Off, the
// same cache row is served.
func TestIdentityCache_EmbeddedLyricsModeBypasses(t *testing.T) {
	for mode, want := range map[string]int{"respect": 0, "extract": 0, "off": 1} {
		e := newHitEnv(t, false)
		if err := testutil.WriteAudioFile(e.root, "a.mp3", "Odile Varga", "Salt Road", "Low Tide", "an invented embedded line"); err != nil {
			t.Fatal(err)
		}
		e.seed(filepath.Join(e.root, "a.mp3"))

		rows, line := e.scan(e.sc, scanner.ScanOptions{EmbeddedLyrics: mode})
		if len(rows) != want || !strings.Contains(line, fmt.Sprintf("hits=%d misses=0 ", want)) {
			t.Errorf("%s mode: %d row(s), log %q; want %d row(s) and as many hits (0: the file was read)", mode, len(rows), line, want)
		}
	}
}

// A symlinked audio file hits on its TARGET's stamp, and a file whose stat
// fails is a counted miss that falls through to the read.
func TestIdentityCache_SymlinkedFileHitsAndStatFailureIsAMiss(t *testing.T) {
	e := newHitEnv(t, false)
	outside := t.TempDir()
	// A long target name, so the link's own size differs from the file's.
	if err := testutil.WriteFLACFileWithComments(outside, "linked-target.flac", 44100, 44100, map[string]string{"ARTIST": "Odile Varga", "TITLE": "Salt Road"}); err != nil {
		t.Fatal(err)
	}
	target, link := filepath.Join(outside, "linked-target.flac"), filepath.Join(e.root, "b.flac")
	// z.flac dangles: its stat fails.
	if err := errors.Join(os.Symlink(target, link), os.Symlink(filepath.Join(outside, "gone.flac"), filepath.Join(e.root, "z.flac"))); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	e.seed(link)
	corruptInPlace(t, target)

	rows, line := e.scan(e.sc, scanner.ScanOptions{})
	if want := []string{"b.flac|Odile Varga|Salt Road|||0"}; !reflect.DeepEqual(rows, want) || !strings.Contains(line, "hits=1 misses=1 ") {
		t.Errorf("rows = %q, log %q; want %q with hits=1 misses=1", rows, line, want)
	}
}

// A cache lookup error falls back to reading the file: the track is kept with
// the row a read builds, no failure is recorded for it, and the error is
// reported as a count plus a cause that cannot name a file.
func TestIdentityCache_LookupErrorFallsBackToTheFile(t *testing.T) {
	sentinel := errors.New("invented store outage")
	for _, tc := range []struct {
		name  string
		mk    func(path string) error // nil: the real store over a dropped table
		cause string
	}{
		{"real store", nil, `first_error="SQL logic error: no such table: audio_metadata`},
		{"wrapped cause", func(p string) error { return fmt.Errorf("identity %q: %w", p, sentinel) }, `first_error="invented store outage"`},
		{"path error", func(p string) error {
			return fmt.Errorf("identity: %w", &fs.PathError{Op: "open", Path: p, Err: syscall.ENOENT})
		}, "first_error=syscall.Errno"},
		{"cause names the file", func(p string) error { return errors.New("cannot read " + filepath.Base(p)) }, "first_error=*errors.errorString"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHitEnv(t, true)
			e.seed(e.flac("a.flac", "Quiet Harbor"))
			e.seed(e.flac("b.flac", "Salt Road"))
			tail := "|" + hitISRC + "|" + hitMBID + "|187"
			want := []string{"a.flac|Marlow Finch|Quiet Harbor" + tail, "b.flac|Marlow Finch|Salt Road" + tail}

			sc := e.sc
			if tc.mk != nil {
				sc = scanner.NewScanner(scanner.WithIdentityStore(errStore{tc.mk}))
			} else {
				e.exec(`DROP TABLE audio_metadata`)
			}
			rows, line := e.scan(sc, scanner.ScanOptions{EnrichRecording: true})
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("rows with a failing cache = %q, want both files read: %q", rows, want)
			}
			var failures int
			if err := e.db.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM scanner_metadata_failures`).Scan(&failures); err != nil || failures != 0 {
				t.Errorf("%d metadata-failure row(s) (err %v) written for a cache lookup error", failures, err)
			}
			if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "hits=0 misses=0 lookup_errors=2") || !strings.Contains(line, tc.cause) {
				t.Errorf("cache log line = %q, want WARN, lookup_errors=2 and %s", line, tc.cause)
			}
			for _, private := range []string{e.root, "flac", "Marlow", "Quiet", "Harbor", "Salt"} {
				if strings.Contains(line, private) {
					t.Errorf("cache log line carries %q: %s", private, line)
				}
			}
		})
	}
}

// The metadata-failure skip still decides first: a file it covers stays
// skipped even though a current cache row exists for it.
func TestIdentityCache_MetadataFailureSkipWins(t *testing.T) {
	e := newHitEnv(t, true)
	skipped := e.flac("a.flac", "Quiet Harbor")
	served := e.flac("b.flac", "Salt Road")
	e.seed(skipped)
	e.seed(served)
	corruptInPlace(t, served)
	info, err := os.Lstat(skipped)
	if err != nil {
		t.Fatal(err)
	}
	if err := scanfail.New(e.db).RecordFailure(e.ctx, skipped, info.ModTime().UnixNano(), info.Size(), errors.New("invented failure")); err != nil {
		t.Fatal(err)
	}

	// The control is b.flac: the same kind of cache row, and it is served.
	rows, line := e.scan(e.sc, scanner.ScanOptions{})
	if want := []string{"b.flac|Marlow Finch|Salt Road|||0"}; !reflect.DeepEqual(rows, want) || !strings.Contains(line, "hits=1 misses=0 ") {
		t.Errorf("rows = %q, log %q; want only %q, served from the cache", rows, line, want)
	}
}
