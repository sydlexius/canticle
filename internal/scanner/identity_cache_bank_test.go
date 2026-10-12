package scanner_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// Tests for the tag-cache row a scan banks from its own read (#1575). They
// reuse the #1574 fixtures in identity_cache_hit_test.go: a real database, the
// production audiometa.Store behind the adapter, real audio files.

var enrich = scanner.ScanOptions{EnrichRecording: true}

// recordStore is the real store with its Record replaced: nil err drops the
// write (the scanner as it was before #1575), non-nil err fails it.
type recordStore struct {
	storeAdapter
	err func(path string) error
}

func (r recordStore) Record(_ context.Context, path string, _ scanner.AudioFacts) error {
	if r.err == nil {
		return nil
	}
	return r.err(path)
}

// countOpens makes sc count every audio file it opens.
func countOpens(sc *scanner.Scanner) *int {
	n := new(int)
	scanner.SetAudioOpener(sc, func(p string) (*os.File, error) {
		*n++
		return os.Open(p) //nolint:gosec // reason: test fixture path under t.TempDir
	})
	return n
}

// metaRows renders every audio_metadata column except the wall-clock
// indexed_at, one string per row, ordered by path.
func (e hitEnv) metaRows() []string {
	e.t.Helper()
	rows, err := e.db.QueryContext(e.ctx, `SELECT * FROM audio_metadata ORDER BY file_path`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			e.t.Fatal(err)
		}
		var b strings.Builder
		for i, c := range cols {
			if c != "indexed_at" {
				fmt.Fprintf(&b, "%s=%v|", c, vals[i])
			}
		}
		out = append(out, b.String())
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// One scan of a never-indexed file opens it once and leaves a row the store
// reports current. The next scan is then served from that row: the file's
// bytes are garbage by then, the scan row is identical, the file is not
// opened, and nothing is written.
func TestScanBank_ColdScanBanksTheRowTheNextScanIsServedFrom(t *testing.T) {
	e := newHitEnv(t, true)
	path := e.flac("a.flac", "Quiet Harbor")
	opens := countOpens(e.sc)

	cold, line := e.scan(e.sc, enrich)
	if want := []string{"a.flac|Marlow Finch|Quiet Harbor|" + hitISRC + "|" + hitMBID + "|187"}; !reflect.DeepEqual(cold, want) {
		t.Fatalf("cold rows = %q, want %q", cold, want)
	}
	if *opens != 1 || !strings.Contains(line, "hits=0 misses=1 ") || !strings.Contains(line, "recorded=1 record_errors=0") {
		t.Errorf("cold scan: %d open(s), log %q; want 1 open, one miss, recorded=1", *opens, line)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	absRoot, canonRoot := pathutil.CanonicalRoot(e.root)
	id, found, err := audiometa.New(e.db, scanner.DurationReaderVersion).Identity(e.ctx,
		pathutil.RebaseUnderCanonicalRoot(absRoot, canonRoot, path), info.ModTime().UnixNano(), info.Size())
	if err != nil || !found || id.Title != "Quiet Harbor" || id.TrackLength != 187 || id.ISRC != hitISRC {
		t.Fatalf("cache row after one scan: %+v found=%v err=%v; want a current row for the file", id, found, err)
	}

	corruptInPlace(t, path)
	e.exec(`UPDATE audio_metadata SET indexed_at = 'marked'`)
	warm, line := e.scan(e.sc, enrich)
	if !reflect.DeepEqual(warm, cold) {
		t.Errorf("warm rows = %q, want the cold scan's %q", warm, cold)
	}
	if *opens != 1 || !strings.Contains(line, "hits=1 misses=0 ") || !strings.Contains(line, "recorded=0 record_errors=0") {
		t.Errorf("warm scan: %d open(s) in total, log %q; want still 1, a hit, nothing recorded", *opens, line)
	}
	var marked int
	if err := e.db.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM audio_metadata WHERE indexed_at = 'marked'`).Scan(&marked); err != nil || marked != 1 {
		t.Errorf("an unchanged warm scan rewrote the cache row (marked rows %d, err %v)", marked, err)
	}
}

// The row a scan banks is, column for column, the row `scan index-metadata`
// writes for the same file: store.Record of scanner.ReadAudioFacts. The MP3
// fixture has no parseable duration, so index-metadata records it and the scan
// deliberately does not (see bankFacts); it is the one row allowed to differ.
func TestScanBank_RowEqualsTheIndexMetadataRow(t *testing.T) {
	scanned, indexed := newHitEnv(t, false), newHitEnv(t, false)
	indexed.root = scanned.root
	if err := errors.Join(
		testutil.WriteFLACFileWithComments(scanned.root, "a.flac", 44100, 44100*187, map[string]string{
			"ARTIST": "Marlow Finch", "ALBUMARTIST": "The Tin Larks", "TITLE": "Quiet Harbor", "ALBUM": "Low Tide",
			"COMPOSER": "Odile Varga", "GENRE": "Invented", "DATE": "2026", "TRACKNUMBER": "3", "TRACKTOTAL": "9",
			"DISCNUMBER": "1", "DISCTOTAL": "2", "ISRC": hitISRC, "MUSICBRAINZ_TRACKID": hitMBID,
		}),
		os.WriteFile(filepath.Join(scanned.root, "b.flac"), scanner.BuildFLACWithRawComments(t, []string{
			"ARTIST=Marlow Finch", "ARTIST=Odile Varga", "ALBUMARTIST=Marlow Finch", "ALBUMARTIST=The Tin Larks", "TITLE=Salt Road",
		}), 0o600),
		testutil.WriteFLACFile(scanned.root, "c.flac", 44100, 44100*61),
		testutil.WriteAudioFile(scanned.root, "d.mp3", "Odile Varga", "Untitled Shore", "Low Tide", ""),
	); err != nil {
		t.Fatal(err)
	}

	if rows, _ := scanned.scan(scanned.sc, enrich); len(rows) != 4 {
		t.Fatalf("scan returned %d row(s), want 4", len(rows))
	}
	for _, name := range []string{"a.flac", "b.flac", "c.flac", "d.mp3"} {
		indexed.seed(filepath.Join(indexed.root, name))
	}
	got, want := scanned.metaRows(), indexed.metaRows()
	if len(want) != 4 || !reflect.DeepEqual(got, want[:3]) {
		t.Fatalf("banked rows differ from the index-metadata rows for the three FLAC files:\nscan:  %q\nindex: %q", got, want)
	}
	// Guard the comparison: the rows are populated (a multi-value artist among
	// them), stamped current, and the unbanked fourth is the MP3.
	for i, sub := range []string{"composer=Odile Varga|", "artist=Marlow Finch; Odile Varga|", "duration_seconds=61|", "file_type=MP3|duration_seconds=0|"} {
		if !strings.Contains(want[i], sub) || !strings.Contains(want[i], fmt.Sprintf("tag_reader_version=%d|", scanner.TagReaderVersion)) {
			t.Errorf("fixture row %d lacks %q or the current stamp; the comparison would be weak: %q", i, sub, want[i])
		}
	}
	if !strings.Contains(want[0], "track_no=3|track_total=9|disc_no=1|disc_total=2|") || !strings.Contains(want[0], "duration_seconds=187|") {
		t.Errorf("fully tagged row is not fully populated: %q", want[0])
	}
}

// The cases in which a read must NOT be banked. Each keeps the track and
// writes no row; the control proves the same file is banked otherwise.
func TestScanBank_ReadsThatAreNotBanked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  scanner.ScanOptions
		setup func(sc *scanner.Scanner)
		want  int // audio_metadata rows afterwards
	}{
		{"control", enrich, nil, 1},
		{"enrichment off", scanner.ScanOptions{}, nil, 0},
		{"embedded-lyrics mode makes no lookup", scanner.ScanOptions{EnrichRecording: true, EmbeddedLyrics: "respect"}, nil, 0},
		{"duration parse failed", enrich, func(sc *scanner.Scanner) {
			scanner.SetDurationProbe(sc, func(string) (int, error) { return 0, errors.New("invented parse failure") })
		}, 0},
		{"no duration parser is banked like index-metadata", enrich, func(sc *scanner.Scanner) {
			scanner.SetDurationProbe(sc, func(string) (int, error) { return 0, scanner.ErrNoDurationParser })
		}, 1},
		{"file changed between the lookup stat and the read", enrich, func(sc *scanner.Scanner) {
			scanner.SetAudioOpener(sc, func(p string) (*os.File, error) {
				if err := os.Chtimes(p, time.Now(), time.Now().Add(-48*time.Hour)); err != nil {
					return nil, err
				}
				return os.Open(p) //nolint:gosec // reason: test fixture path under t.TempDir
			})
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHitEnv(t, false)
			e.flac("a.flac", "Quiet Harbor")
			if tc.setup != nil {
				tc.setup(e.sc)
			}
			rows, line := e.scan(e.sc, tc.opts)
			if len(rows) != 1 || !strings.Contains(rows[0], "a.flac|Marlow Finch|Quiet Harbor|") {
				t.Errorf("rows = %q, want the track kept", rows)
			}
			if got := len(e.metaRows()); got != tc.want || !strings.Contains(line, fmt.Sprintf("recorded=%d record_errors=0", tc.want)) {
				t.Errorf("%d cache row(s), log %q; want %d and the same recorded count", got, line, tc.want)
			}
		})
	}
}

// brokenRead fails every read, as a disk fault under the re-read would.
type brokenRead struct{ io.ReadSeeker }

func (brokenRead) Read([]byte) (int, error) { return 0, errors.New("invented input/output error") }

// A scan whose Vorbis multi-value re-read hits an I/O error queues the track
// with the single-value fallback artist, as before #1575, and does NOT bank it.
// The next clean scan of the unchanged file reads both artists and banks the row.
func TestScanBank_FailedMultiValueReadIsNotBanked(t *testing.T) {
	e := newHitEnv(t, false)
	kv := []string{"TITLE=Quiet Harbor", "ARTIST=Ada Reed", "ARTIST=Marlow Finch"}
	if err := os.WriteFile(filepath.Join(e.root, "a.flac"), scanner.BuildFLACWithRawComments(t, kv), 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(when, artist string, banked int) {
		t.Helper()
		rows, line := e.scan(e.sc, enrich)
		wantLog := fmt.Sprintf("recorded=%d record_errors=0 multivalue_skips=%d", banked, 1-banked)
		if !reflect.DeepEqual(rows, []string{"a.flac|" + artist + "|Quiet Harbor|||30"}) || len(e.metaRows()) != banked || !strings.Contains(line, wantLog) {
			t.Fatalf("%s: rows %q, %d cache row(s), log %q; want artist %q, %d row(s), %q", when, rows, len(e.metaRows()), line, artist, banked, wantLog)
		}
	}
	scanner.SetVorbisReader(e.sc, func(f *os.File) io.ReadSeeker { return brokenRead{f} })
	check("failed re-read", "Marlow Finch", 0)
	scanner.SetVorbisReader(e.sc, nil)
	check("clean re-read", "Ada Reed; Marlow Finch", 1)
}

// A failed cache write is counted and reported without naming a file, and the
// track is kept with the row a read builds. A write cut short by a canceled
// context is shutdown: not counted, not a warning.
func TestScanBank_WriteFailureIsCountedAndKeepsTheTrack(t *testing.T) {
	outage := errors.New("invented write outage")
	for _, tc := range []struct {
		name string
		err  func(path string) error // nil: the real store, its insert aborted by a trigger
		want []string
	}{
		{"real store", nil, []string{"level=WARN", "recorded=0 record_errors=2", "invented write outage"}},
		{"error quoting the path", func(p string) error { return fmt.Errorf("record %q: %w", p, outage) },
			[]string{"level=WARN", "recorded=0 record_errors=2", `first_error="invented write outage"`}},
		{"canceled", func(p string) error { return fmt.Errorf("record %q: %w", p, context.Canceled) },
			[]string{"level=DEBUG", "recorded=0 record_errors=0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHitEnv(t, true)
			e.flac("a.flac", "Quiet Harbor")
			e.flac("b.flac", "Salt Road")
			sc := e.sc
			if tc.err != nil {
				sc = scanner.NewScanner(scanner.WithIdentityStore(recordStore{
					storeAdapter{audiometa.New(e.db, scanner.DurationReaderVersion)}, tc.err}))
			} else {
				e.exec(`CREATE TRIGGER invented_outage BEFORE INSERT ON audio_metadata
				        BEGIN SELECT RAISE(ABORT, 'invented write outage'); END`)
			}
			tail := "|" + hitISRC + "|" + hitMBID + "|187"
			want := []string{"a.flac|Marlow Finch|Quiet Harbor" + tail, "b.flac|Marlow Finch|Salt Road" + tail}
			rows, line := e.scan(sc, enrich)
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("rows with a failing cache write = %q, want both tracks: %q", rows, want)
			}
			for _, s := range tc.want {
				if !strings.Contains(line, s) {
					t.Errorf("cache log line = %q, want it to contain %q", line, s)
				}
			}
			for _, private := range []string{e.root, filepath.Base(filepath.Dir(e.root)), "flac", "Marlow", "Quiet", "Harbor", "Salt"} {
				if strings.Contains(line, private) {
					t.Errorf("cache log line carries %q: %s", private, line)
				}
			}
			if n := len(e.metaRows()); n != 0 {
				t.Errorf("%d cache row(s) after failed writes, want 0", n)
			}
		})
	}
}

// THE MEASUREMENT recorded in session-management.md for #1575: audio files
// opened by three consecutive scans of an unchanged four-file library that
// `scan index-metadata` never visited. "before" drops the cache write, which
// is the scanner as it stood before this change; "after" is production wiring.
func TestScanBank_OpenCountsBeforeAndAfter(t *testing.T) {
	for _, tc := range []struct {
		name string
		bank bool
		want []int
	}{
		{"before", false, []int{4, 4, 4}},
		{"after", true, []int{4, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHitEnv(t, false)
			for i, title := range []string{"Quiet Harbor", "Salt Road", "Untitled Shore", "Low Tide"} {
				e.flac(fmt.Sprintf("%d.flac", i), title)
			}
			var store scanner.IdentityStore = storeAdapter{audiometa.New(e.db, scanner.DurationReaderVersion)}
			if !tc.bank {
				store = recordStore{storeAdapter: store.(storeAdapter)}
			}
			sc := scanner.NewScanner(scanner.WithIdentityStore(store),
				scanner.WithDurationStore(audiodur.New(e.db, scanner.DurationReaderVersion)))
			opens, got, last := countOpens(sc), []int{}, 0
			for range 3 {
				if rows, _ := e.scan(sc, enrich); len(rows) != 4 {
					t.Fatalf("scan returned %d row(s), want 4", len(rows))
				}
				got, last = append(got, *opens-last), *opens
			}
			t.Logf("%s: audio files opened per scan = %v", tc.name, got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("opens per scan = %v, want %v", got, tc.want)
			}
		})
	}
}
