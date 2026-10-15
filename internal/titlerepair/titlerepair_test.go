package titlerepair

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/dbtest"
	it "github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/worker"
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
		res.Strong != res.Unreadable+res.InFlight+res.HandMarked+res.ArtistPrefixed+res.ByAlbum+res.Candidates ||
		res.Strong != res.StrongByTitle+res.StrongByAlbum ||
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
		Examined: 11, NoSignal: 4, Weak: 3, Strong: 4, StrongByTitle: 4, Candidates: 4, OutsideRoots: 4, // no root is named
		StrongBy: map[StrongKey]int{
			{it.FamilyInstrumental, it.PositionBracket}: 3,
			{it.FamilyOffVocal, it.PositionSuffix}:      1,
		},
		DetectorInstrumental: 2, DetectorNotInstrumental: 1, DetectorUnscored: 1,
		WeakDetectorInstrumental: 1, WeakDetectorNotInstrumental: 1, WeakDetectorUnscored: 1,
		WeakAlbumInstrumental: 1, // the bare album word: the release rule declines it
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

// A plain title on an instrumental release is Strong by album, counted from the
// row's stored album and artists: no audio file is read for it. It is counted
// apart from the Strong rows by title, the two sum to Strong, and it leaves the
// Weak rows. It reaches the cull only as the release's own row: edition
// instrumental (0), or in a dry run a row the split would re-key (1), and with
// the audio file's album still naming the release (9 withdraws it, 10 is
// unreadable). Under Apply an empty edition is a row the split left shared. A
// held row is still counted in flight first.
func TestStrongByAlbumIsCountedApart(t *testing.T) {
	const release = "Low Tide (Instrumental)"
	d := dbtest.Open(t)
	ids := seed(t, d,
		wq{title: "Quiet Shelf", album: release},
		wq{title: "Quiet Shelf", album: "Low Tide - Instrumentals", artist: "Copper Finch Trio"},
		wq{title: "Quiet Shelf", album: release, status: "processing"},
		wq{title: "Quiet Shelf (Vocal)", album: release},                              // the title guard
		wq{title: "Quiet Shelf", album: "Low Tide Instrumentals"},                     // the bare trailing word
		wq{title: "Quiet Shelf", album: release, artist: "Copper Finch feat. Wren"},   // a guest credit
		wq{title: "Quiet Shelf", album: "Low Tide - Instrumental", artist: "Various"}, // dash form, no real artist
		wq{title: "Quiet Shelf (Piano Version)", album: "Low Tide"},                   // weak by its title alone
		wq{},                                     // strong by its title
		wq{title: "Quiet Shelf", album: release}, // its file names the sung album
		wq{title: "Quiet Shelf", album: release}, // its file cannot be read
	)
	for _, i := range []int{0, 9, 10} {
		if _, err := d.Exec(`UPDATE work_queue SET edition = 'instrumental' WHERE id = ?`, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	var read []string
	fileAlbum := func(path string) (string, error) {
		read = append(read, path)
		switch path {
		case "/shelf/9.flac":
			return "Low Tide", nil
		case "/shelf/10.flac":
			return "", errors.New("read failed")
		}
		return "", nil
	}
	res, seen := run(t, d, Options{FileAlbum: fileAlbum})
	if got := strings.Join(read, " "); got != "/shelf/0.flac /shelf/1.flac /shelf/8.flac /shelf/9.flac /shelf/10.flac" {
		t.Errorf("audio files read: %s; want the by-title row's and the release rows' the cull may reach", got)
	}
	if res.Strong != 6 || res.StrongByTitle != 1 || res.StrongByAlbum != 5 || res.ByAlbum != 1 || res.InFlight != 1 || res.Unreadable != 1 ||
		res.Candidates != 3 || res.Weak != 5 || res.WeakAlbumInstrumental != 4 {
		t.Errorf("strong %d (by title %d, by album %d), by-album skips %d, in flight %d, unreadable %d, candidates %d, weak %d, weak album %d; want 6 (1, 5), 1, 1, 1, 3, 5, 4",
			res.Strong, res.StrongByTitle, res.StrongByAlbum, res.ByAlbum, res.InFlight, res.Unreadable, res.Candidates, res.Weak, res.WeakAlbumInstrumental)
	}
	if got := res.StrongBy[StrongKey{it.FamilyInstrumental, it.PositionRelease}]; got != 5 {
		t.Errorf("StrongBy release = %d, want 5", got)
	}
	// No root is named, so the cull refuses each candidate.
	want := map[int64]string{ids[0]: DispositionOutsideRoots, ids[1]: DispositionOutsideRoots, ids[2]: DispositionInFlight,
		ids[8]: DispositionOutsideRoots, ids[9]: DispositionByAlbum, ids[10]: DispositionUnreadable}
	if !maps.Equal(seen, want) {
		t.Errorf("dry run dispositions = %v, want %v", seen, want)
	}
	_, seen = run(t, d, Options{FileAlbum: fileAlbum, Apply: true, Report: func(lyricblock.Backup) error { return nil }})
	if want[ids[1]] = DispositionByAlbum; !maps.Equal(seen, want) {
		t.Errorf("apply dispositions = %v, want %v", seen, want)
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

// sungLane answers every track with the shelf's words, as a provider would for
// the sung copy.
type sungLane struct{}

func (sungLane) FindLyrics(_ context.Context, tr models.Track) (models.Song, error) {
	return models.Song{Track: tr, Lyrics: models.Lyrics{LyricsBody: plainWords}}, nil
}

// The album cull end to end (#1564). A legacy row settled with the sung words
// for the release copy, linked to both copies, its words cached. The split
// re-keys it; the cull backs the file up, removes it, drops the cache entry and
// reopens the row, adding no block, for a file that appears mid-cull too.
// Then a scan and one worker pass give the sung copy its own row and write its
// words: nothing blocks them. A second pass hands the reopened row to the
// worker's title gate (#1562), which settles it on the album verdict and
// writes the title-rule marker beside the release copy.
func TestAlbumCullAddsNoBlockAndFreesTheSungTwin(t *testing.T) {
	const title, release = "Quiet Shelf", "Low Tide (Instrumental)"
	ctx := context.Background()
	s := newShelf(t)
	music, sungDir := filepath.Join(s.base, "music"), filepath.Join(s.base, "music", "sung")
	if err := os.MkdirAll(sungDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.exec(t, `INSERT INTO libraries (id, path, name) VALUES (1, ?, 'shelf')`, music)
	keys := []any{normalize.NormalizeKey(shelfArtist), normalize.NormalizeKey(title)}
	row := s.get(t, `INSERT INTO work_queue (artist, title, album, artist_key, title_key, outdir, filename, source_path, status, outcome_type, provider_lane, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, 'a.flac', ?, 'done', 'synced', ?, ?) RETURNING id`, shelfArtist, title, release, keys[0], keys[1], s.dir,
		filepath.Join(s.dir, "a.flac"), providers.Musixmatch, settled.Format(time.RFC3339))
	for _, c := range []struct{ album, dir string }{{release, s.dir}, {"Low Tide", sungDir}} {
		sr := s.get(t, `INSERT INTO scan_results (library_id, file_path, artist, title, album, artist_key, title_key, outdir, filename, status)
			VALUES (1, ?, ?, ?, ?, ?, ?, ?, 'a.flac', 'processing') RETURNING id`, filepath.Join(c.dir, "a.flac"), shelfArtist, title, c.album, keys[0], keys[1], c.dir)
		s.exec(t, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, row, sr)
	}
	lrc := filepath.Join(s.dir, "a.lrc")
	put(t, lrc, lrcBody, 0)
	put(t, filepath.Join(s.dir, "a.flac"), "", 0) // the audio file: a release row is culled only while it exists
	s.cacheWords(t, shelfArtist, title)

	if sres, err := New(s.d, nil).Split(ctx, SplitOptions{Apply: true, Report: func(SplitRecord) error { return nil }}); err != nil || sres.ReKeyed != 1 || sres.Pending != 1 {
		t.Fatalf("split: %+v, %v; want the row re-keyed and the sung copy pending", sres, err)
	}
	var backed []lyricblock.Backup
	late := filepath.Join(s.dir, "a.txt") // other words, in the window
	res, _ := run(t, s.d, s.opts(true, func(b lyricblock.Backup) error {
		if backed = append(backed, b); !exists(late) {
			put(t, late, "Quorble zint\n", 0)
		}
		return nil
	}))
	if res.Repaired != 1 || res.Removed != 2 || res.NewBlocks != 0 || res.CacheInvalidated != 1 || res.Reopened != 1 {
		t.Errorf("cull: %+v; want one row repaired, two files removed and no block", res)
	}
	if len(backed) != 2 || backed[0].Path != lrc || string(backed[0].Content) != lrcBody || backed[1].Path != late {
		t.Errorf("backup = %+v; want the removed files' bytes", backed)
	}
	if got := s.get(t, `SELECT status || '|' || edition || '|' || (SELECT COUNT(*) FROM lyric_blocks) || '|' || (SELECT COUNT(*) FROM lyrics_cache)
		FROM work_queue WHERE id = ?`, row); exists(lrc) || exists(late) || got != "pending|instrumental|0|0" {
		t.Errorf("after the cull: file exists %v, row %s; want it gone and pending|instrumental|0|0", exists(lrc), got)
	}

	q := queue.NewDBQueue(s.d)
	q.SetEditionRule(queue.ReleaseEdition) // as serve and the scheduler set it under a title mode other than off
	e := scan.Enqueuer{Results: scan.New(s.d), Cache: cache.New(s.d), Queue: q, Priority: 5, Blocks: lyricblock.NewStore(s.d, nil)}
	if enq, hits, err := e.EnqueuePending(ctx, models.Library{ID: 1}); err != nil || enq != 1 || hits != 0 {
		t.Fatalf("scan: enqueued %d, cache hits %d, %v; want the sung copy enqueued", enq, hits, err)
	}
	// The sung copy's row outranks the reopened one.
	w := worker.New(q, cache.New(s.d), sungLane{}, lyrics.NewLRCWriter(music))
	w.SetTitleRule(true)
	w.SetBlockChecker(lyricblock.NewStore(s.d, nil))
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(sungDir, "a.txt")); err != nil || !strings.Contains(string(b), "Zorp the blent") {
		t.Errorf("the sung copy's lyric = %q, %v; want its words written", b, err)
	}
	if got := s.get(t, `SELECT group_concat(edition || ':' || status || ':' || COALESCE(outcome_type, 'null'), ',') FROM (SELECT * FROM work_queue ORDER BY id)`); got != "instrumental:pending:null,:done:unsynced" {
		t.Errorf("rows = %s; want the release row waiting and the sung row settled with its words", got)
	}

	s.exec(t, `UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, row)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (release row): %v", err)
	}
	if b, err := os.ReadFile(late); err != nil || exists(lrc) || !strings.Contains(string(b), "[source:title-rule]") || !strings.Contains(string(b), lyrics.InstrumentalMarker) {
		t.Errorf("the release copy's sidecar = %q, %v; want the title-rule marker", b, err)
	}
	if got := s.get(t, `SELECT status || '|' || (SELECT COUNT(*) FROM lyric_blocks) FROM work_queue WHERE id = ?`, row); got != "done|0" {
		t.Errorf("release row = %s; want it settled by the gate with no block", got)
	}
}

// A cull of a row strong by album interrupted between the unlink and the
// reopen leaves it settled, recording a lyric file with none on disk and no
// block to prove it: a rerun finishes it (#1564). The control, strong by its
// own title in the same state with no block, is still no file. So is a release
// row whose audio file is gone too (a deleted track), with no reader to fail:
// it is skipped as unreadable, never reopened for a marker.
func TestInterruptedAlbumCullFinishesOnARerun(t *testing.T) {
	s := newShelf(t)
	const release = `album = 'Low Tide (Instrumental)', edition = 'instrumental'`
	album := s.row(t, `title = 'Quiet Shelf', title_key = 'quiet shelf', `+release)
	control := s.row(t, "")
	gone := s.row(t, `title = 'Still Shelf', title_key = 'still shelf', `+release)
	s.exec(t, `UPDATE work_queue SET source_path = ? WHERE id = ?`, filepath.Join(s.dir, "gone.flac"), gone.id)
	res, seen := run(t, s.d, s.opts(true, func(lyricblock.Backup) error { return nil }))
	if seen[album.id] != DispositionRepaired || seen[control.id] != DispositionNoFile || seen[gone.id] != DispositionUnreadable ||
		res.Reopened != 1 || res.Removed != 0 || res.NewBlocks != 0 || res.Unreadable != 1 {
		t.Errorf("rerun: %+v, %v; want the album row finished, the control no file and the deleted track unreadable", res, seen)
	}
	if got := s.rowState(t, album.id); got != "pending|null|null|0" {
		t.Errorf("album row = %s, want pending|null|null|0", got)
	}
	if got := s.rowState(t, gone.id); got != "done|musixmatch|synced|0" {
		t.Errorf("deleted track's row = %s, want it left settled", got)
	}
}
