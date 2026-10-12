package titlerepair

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/dbtest"
	it "github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// strongTitle is a bracketed Strong title: actionable with any artist.
const strongTitle = "Glass Harbor (Instrumental)"

// wq is one work_queue row. The zero value of each field takes the default of
// a row the selector picks: done, synced, written by a provider lane.
type wq struct {
	title, album, artist, albumArtist string
	status, outcome, lane             string // "-" stores NULL for outcome and lane
	source                            string // "-" stores an empty path
	edited, handMarked                bool
	detector                          any // nil stores NULL
}

// seed inserts rows in one transaction and returns their ids in order. Each row
// gets its own identity key (the pair is unique) and, unless it names one, a
// source path named by its position.
func seed(t *testing.T, d *sql.DB, rows ...wq) []int64 {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	nullable := func(s, def string) any {
		if s == "-" {
			return nil
		}
		return cmp.Or(s, def)
	}
	stamp := func(on bool) any {
		if on {
			return "2026-01-02T03:04:05Z"
		}
		return nil
	}
	ids := make([]int64, 0, len(rows))
	for i, r := range rows {
		source := cmp.Or(r.source, fmt.Sprintf("/shelf/%d.flac", i))
		if source == "-" {
			source = ""
		}
		res, err := tx.Exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, album_artist, source_path,
                status, outcome_type, provider_lane, lyric_edited_at, manual_instrumental_at, instrumental_result)
             VALUES (?, ?, 'key', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cmp.Or(r.artist, "Tin Lantern Band"), cmp.Or(r.title, strongTitle), fmt.Sprintf("row-%d-%d", len(rows), i), r.album, r.albumArtist,
			source, cmp.Or(r.status, "done"), nullable(r.outcome, "synced"), nullable(r.lane, providers.Musixmatch),
			stamp(r.edited), stamp(r.handMarked), r.detector)
		if err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// run runs the repair and returns the count with the dispositions OnRow saw,
// keyed by id. A row reported twice fails the test, and so does a count that
// breaks any sum Result documents. The caller's own OnRow still runs.
func run(t *testing.T, d *sql.DB, o Options) (Result, map[int64]string) {
	t.Helper()
	return runLog(t, d, o, nil)
}

// runLog is run with the repair logging to log.
func runLog(t *testing.T, d *sql.DB, o Options, log *slog.Logger) (Result, map[int64]string) {
	t.Helper()
	seen := map[int64]string{}
	inner := o.OnRow
	o.OnRow = func(r Row) {
		if _, dup := seen[r.ID]; dup {
			t.Errorf("row %d reported twice", r.ID)
		}
		seen[r.ID] = r.Disposition
		if inner != nil {
			inner(r)
		}
	}
	res, err := New(d, log).Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Examined != res.NoSignal+res.Weak+res.Strong || len(seen) != res.Strong ||
		res.Strong != res.Unreadable+res.InFlight+res.HandMarked+res.ArtistPrefixed+res.Candidates ||
		res.Candidates != res.Symlink+res.OutsideRoots+res.StemOccupied+res.NoFile+res.SourceDisagrees+
			res.NotProvable+res.NeedsManualMark+res.Errors+res.WouldRepair+res.Repaired {
		t.Errorf("the counts do not add up: %+v, %d rows reported", res, len(seen))
	}
	return res, seen
}

// Only a done or processing row that a provider lane settled with lyrics is
// selected. A detector, hand or title-rule settle, a NULL lane, an instrumental
// or NULL outcome and a row not yet settled are never examined.
func TestSelectorPicksOnlyProviderLyricRows(t *testing.T) {
	d := dbtest.Open(t)
	ids := seed(t, d,
		wq{}, wq{outcome: "unsynced", lane: providers.PetitLyrics}, wq{lane: providers.InnerTube},
		wq{status: "pending"}, wq{status: "failed"}, wq{status: "deferred"}, wq{status: "unavailable"},
		wq{outcome: "instrumental"}, wq{outcome: "-"}, wq{outcome: "rejected"},
		wq{lane: lyrics.DetectorLaneName}, wq{lane: queue.ManualLane}, wq{lane: lyrics.TitleRuleLaneName}, wq{lane: "-"},
	)
	res, seen := run(t, d, Options{})
	if res.Examined != 3 || res.Candidates != 3 {
		t.Errorf("Examined, Candidates = %d, %d, want 3, 3", res.Examined, res.Candidates)
	}
	if got := slices.Sorted(maps.Keys(seen)); !slices.Equal(got, ids[:3]) {
		t.Errorf("selected ids %v, want %v", got, ids[:3])
	}
}

// Strong rows are counted by family and position and by the detector's stored
// verdict. Weak, a cappella and karaoke rows are counted and never candidates.
func TestClassificationCounts(t *testing.T) {
	d := dbtest.Open(t)
	seed(t, d,
		wq{detector: 1}, wq{detector: 0}, wq{},
		wq{title: "Glass Harbor - Off Vocal", detector: 1},
		wq{title: "Glass Harbor - Instrumental Demo", detector: 1}, // Weak, detector says instrumental
		wq{title: "Glass Harbor", album: "Instrumentals"},          // Weak, by album, never scored
		wq{title: "Glass Harbor (Piano Version)", detector: 0},     // Weak, detector says not instrumental
		wq{title: "Glass Harbor (A Cappella)"},                     // excluded
		wq{title: "Glass Harbor (Karaoke Instrumental)"},           // excluded
		wq{title: strongTitle, album: "Karaoke Night Vol 2"},       // excluded by album
		wq{title: "Glass Harbor"},
	)
	res, seen := run(t, d, Options{})
	want := Result{
		Examined: 11, NoSignal: 4, Weak: 3, Strong: 4, Candidates: 4, OutsideRoots: 4, // no root is named
		StrongBy: map[StrongKey]int{
			{it.FamilyInstrumental, it.PositionBracket}: 3,
			{it.FamilyOffVocal, it.PositionSuffix}:      1,
		},
		DetectorInstrumental: 2, DetectorNotInstrumental: 1, DetectorUnscored: 1,
		WeakDetectorInstrumental: 1, WeakDetectorNotInstrumental: 1, WeakDetectorUnscored: 1,
	}
	if sum := res.WeakDetectorInstrumental + res.WeakDetectorNotInstrumental + res.WeakDetectorUnscored; sum != res.Weak {
		t.Errorf("weak detector split sums to %d, want Weak %d", sum, res.Weak)
	}
	if !maps.Equal(res.StrongBy, want.StrongBy) {
		t.Errorf("StrongBy = %v, want %v", res.StrongBy, want.StrongBy)
	}
	res.StrongBy, want.StrongBy = nil, nil
	if fmt.Sprint(res) != fmt.Sprint(want) {
		t.Errorf("Result = %+v, want %+v", res, want)
	}
	if len(seen) != 4 {
		t.Errorf("OnRow saw %d rows, want the 4 Strong ones", len(seen))
	}
}

// The skips: a held row, a hand-protected row and a Strong title the rule
// declines because the text before the dash is the artist. The first match
// wins, in that order. The one candidate reaches the cull, which refuses it:
// these runs name no library root.
func TestSkips(t *testing.T) {
	const prefixed = "Tin Lantern Band - Off Vocal"
	d := dbtest.Open(t)
	ids := seed(t, d,
		wq{status: "processing"},
		wq{status: "processing", edited: true},
		wq{edited: true},
		wq{handMarked: true},
		wq{title: prefixed, edited: true},
		wq{title: prefixed},
		wq{title: prefixed, artist: "Copper Finch Trio", albumArtist: "Tin Lantern Band"},
		wq{title: prefixed, artist: "Various Artists"},
		wq{title: prefixed, artist: "Copper Finch Trio"},
	)
	res, seen := run(t, d, Options{})
	if res.Strong != 9 || res.InFlight != 2 || res.HandMarked != 3 || res.ArtistPrefixed != 3 || res.Candidates != 1 {
		t.Errorf("Strong %d: in flight %d, hand-marked %d, artist-prefixed %d, candidates %d; want 9: 2, 3, 3, 1",
			res.Strong, res.InFlight, res.HandMarked, res.ArtistPrefixed, res.Candidates)
	}
	for i, want := range []string{
		DispositionInFlight, DispositionInFlight, DispositionHandMarked, DispositionHandMarked, DispositionHandMarked,
		DispositionArtistPrefixed, DispositionArtistPrefixed, DispositionArtistPrefixed, DispositionOutsideRoots,
	} {
		if seen[ids[i]] != want {
			t.Errorf("row %d: disposition %q, want %q", i, seen[ids[i]], want)
		}
	}
}

// LibraryID scopes the selection through the junction. A row linked to a
// library through two files, and to a second library, is counted once.
func TestLibraryScope(t *testing.T) {
	d := dbtest.Open(t)
	ids := seed(t, d, wq{}, wq{}, wq{})
	for i, path := range []string{"/north", "/south"} {
		if _, err := d.Exec(`INSERT INTO libraries (path, name) VALUES (?, ?)`, path, fmt.Sprintf("Shelf %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i, link := range []struct{ row, lib int }{{0, 1}, {0, 1}, {0, 2}, {1, 2}} {
		sr, err := d.Exec(`INSERT INTO scan_results (library_id, file_path) VALUES (?, ?)`, link.lib, fmt.Sprintf("/f/%d.flac", i))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := sr.LastInsertId()
		if _, err := d.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, ids[link.row], id); err != nil {
			t.Fatal(err)
		}
	}
	for lib, want := range map[int64][]int64{1: ids[:1], 2: ids[:2]} {
		res, seen := run(t, d, Options{LibraryID: &lib})
		if got := slices.Sorted(maps.Keys(seen)); res.Examined != len(want) || !slices.Equal(got, want) {
			t.Errorf("library %d: examined %d, ids %v; want ids %v", lib, res.Examined, got, want)
		}
	}
	if res, _ := run(t, d, Options{}); res.Examined != 3 {
		t.Errorf("no library: examined %d, want 3", res.Examined)
	}
}

// The worker classifies with the audio file's album where the file has one, so
// that album withdraws a Strong verdict (rows 0, 1) or restores one the row's
// album withdrew (4, 5). Only a title that is Strong alone costs a read (2, 3),
// a failed read is a skip that outranks the others (6 to 8), and a row with no
// source path is never read (9, 10).
func TestFileAlbum(t *testing.T) {
	const plain, karaoke = "Harbor Lights", "Karaoke Night Vol 2"
	d := dbtest.Open(t)
	ids := seed(t, d, wq{album: plain}, wq{album: plain}, wq{title: "Glass Harbor - Instrumental Demo"}, wq{title: "Glass Harbor"},
		wq{album: karaoke}, wq{album: karaoke}, wq{album: plain}, wq{album: karaoke}, wq{status: "processing"},
		wq{source: "-"}, wq{source: " \t"})
	albums := map[string]string{"/shelf/0.flac": karaoke, "/shelf/4.flac": plain}
	var read []string
	res, seen := run(t, d, Options{FileAlbum: func(path string) (string, error) {
		read = append(read, path)
		if slices.Contains([]string{"/shelf/6.flac", "/shelf/7.flac", "/shelf/8.flac"}, path) {
			return "", errors.New("read failed")
		}
		return albums[path], nil
	}})
	if res.NoSignal != 3 || res.Weak != 1 || res.Strong != 7 || res.Unreadable != 3 || res.Candidates != 4 {
		t.Errorf("no signal %d, weak %d, strong %d, unreadable %d, candidates %d; want 3, 1, 7, 3, 4",
			res.NoSignal, res.Weak, res.Strong, res.Unreadable, res.Candidates)
	}
	c, u := DispositionOutsideRoots, DispositionUnreadable // no root is named, so the cull refuses a candidate
	for i, want := range []string{"", c, "", "", c, "", u, u, u, c, c} {
		if seen[ids[i]] != want {
			t.Errorf("row %d: disposition %q, want %q", i, seen[ids[i]], want)
		}
	}
	if got := strings.Join(read, " "); got != "/shelf/0.flac /shelf/1.flac /shelf/4.flac /shelf/5.flac /shelf/6.flac /shelf/7.flac /shelf/8.flac" {
		t.Errorf("FileAlbum read %q, want the seven Strong-titled rows with a path", got)
	}
}

// More rows than two pages, with held rows and unselected rows scattered among
// them and across the page boundaries: every selected row is examined exactly
// once. A page that is not in id order skips or repeats rows.
func TestPaging(t *testing.T) {
	const n = 2*pageSize + 203
	rows := make([]wq, 0, n+n/7+1)
	held := 0
	for i := range n {
		if i%7 == 0 {
			rows = append(rows, wq{status: "pending"})
		}
		if i%3 == 0 {
			held++
			rows = append(rows, wq{status: "processing"})
			continue
		}
		rows = append(rows, wq{})
	}
	d := dbtest.Open(t)
	seed(t, d, rows...)
	res, seen := run(t, d, Options{})
	if res.Examined != n || res.InFlight != held || res.Candidates != n-held || len(seen) != n {
		t.Errorf("examined %d, in flight %d, candidates %d, reported %d; want %d, %d, %d, %d",
			res.Examined, res.InFlight, res.Candidates, len(seen), n, held, n-held, n)
	}
}

// A cancel stops the run after the row in hand and names that row, not the
// page cursor. One that lands in the last row is still not a clean finish.
func TestCancelNamesTheLastRowHandled(t *testing.T) {
	d := dbtest.Open(t)
	ids := seed(t, d, wq{}, wq{}, wq{})
	for _, at := range ids[1:] {
		ctx, cancel := context.WithCancel(context.Background())
		res, err := New(d, nil).Run(ctx, Options{OnRow: func(r Row) {
			if r.ID == at {
				cancel()
			}
		}})
		if want := fmt.Sprintf("stopped after id %d:", at); !errors.Is(err, context.Canceled) || !strings.Contains(fmt.Sprint(err), want) ||
			res.Examined != int(at-ids[0])+1 {
			t.Errorf("cancel in row %d: %d examined, %v; want %q", at, res.Examined, err, want)
		}
	}
}

// The selector must walk work_queue by rowid, already in id order, not sort
// each page in a temp b-tree. This pins the plan of the real query text, with
// and without the library scope, on a never-ANALYZEd database (as production
// is), and that the order is asked for and not left to that plan.
func TestSelectorWalksRowid(t *testing.T) {
	d := dbtest.Open(t)
	lib := int64(1)
	for name, libraryID := range map[string]*int64{"all": nil, "library": &lib} {
		q, args := selector(libraryID, 0)
		if !strings.Contains(q, " ORDER BY id LIMIT ?") {
			t.Errorf("%s: the selector does not order its page by id", name)
		}
		rows, err := d.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(plan, " | "); !strings.Contains(got, "SEARCH work_queue USING INTEGER PRIMARY KEY (rowid>?)") || strings.Contains(got, "TEMP B-TREE") {
			t.Errorf("%s: plan %q, want a rowid walk of work_queue and no temp b-tree", name, got)
		}
	}
}
