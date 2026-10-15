package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
)

// An instrumental-edition row settles on its album verdict (#1562). Every
// artist, title and album below is invented.

const (
	editionArtist = "Tin Lantern Band"
	editionTitle  = "Glass Harbor"
	sungAlbum     = "Low Tide"
	instAlbum     = "Low Tide (Instrumental)"
	sungWords     = "sung harbor words from a provider"
)

// editionRig is a library holding a sung copy (root/sung) and an
// instrumental-release copy (root/inst) of one song, scanned through the real
// scan repository and enqueuer into a real queue, and settled by a worker with
// a real LRCWriter. rule is instrumental_title.mode != off: the queue's edition
// rule, the enqueuer's Strong-title door and the worker's title rule together.
type editionRig struct {
	db    *sql.DB
	q     *queue.DBQueue
	repo  *scan.Repo
	cache *cache.CacheRepo
	lw    *lyrics.LRCWriter
	w     *Worker
	fetch *fakeFetcher
	lib   models.Library
	root  string
	rule  bool
	gen   int // the enqueuer's provider generation; 0 never suppresses
}

func newEditionRig(t *testing.T, rule bool) *editionRig {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := dbtest.OpenAt(ctx, t, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("dbtest.OpenAt: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	root := t.TempDir()
	for _, d := range []string{"sung", "inst"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := library.New(sqlDB).Add(ctx, root, "M", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	if rule {
		q.SetEditionRule(queue.ReleaseEdition) // commands.editionRuleFor under a mode other than off
	}
	r := &editionRig{
		db: sqlDB, q: q, repo: scan.New(sqlDB), cache: cache.New(sqlDB), lw: lyrics.NewLRCWriter(root), lib: lib, root: root, rule: rule,
		fetch: &fakeFetcher{song: models.Song{
			Track:  models.Track{ArtistName: editionArtist, TrackName: editionTitle},
			Lyrics: models.Lyrics{LyricsBody: sungWords},
		}},
	}
	r.w = New(q, r.cache, r.fetch, r.lw)
	r.w.SetTitleRule(rule)
	return r
}

func (r *editionRig) dir(copyName string) string { return filepath.Join(r.root, copyName) }

// scanCopy indexes one copy as pending and runs the scan enqueuer over the
// library, returning the enqueuer's (enqueued, cacheHits).
func (r *editionRig) scanCopy(t *testing.T, copyName string) (int, int) {
	t.Helper()
	return r.scanCopyAs(t, copyName, scan.StatusPending)
}

// scanCopyAs is scanCopy with the indexed status chosen: the scanner indexes
// a file whose sidecar is already on disk as done.
func (r *editionRig) scanCopyAs(t *testing.T, copyName, status string) (int, int) {
	t.Helper()
	ctx := context.Background()
	dir := r.dir(copyName)
	if err := r.repo.Upsert(ctx, r.lib.ID, []models.ScanResult{{
		FilePath: filepath.Join(dir, "track.flac"), Outdir: dir, Filename: "track.lrc", Status: status,
		Track: models.Track{ArtistName: editionArtist, TrackName: editionTitle, AlbumName: map[string]string{"inst": instAlbum, "sung": sungAlbum}[copyName]},
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("upsert %s: %v", copyName, err)
	}
	e := scan.Enqueuer{Results: r.repo, Cache: r.cache, Queue: r.q, Priority: queue.PriorityScan,
		Timing: scan.TimingVerdicts{Reader: r.q}, ProvidersVersion: r.gen}
	if r.rule {
		e.TitleInstrumental = func(tr models.Track) bool {
			return instrumentaltitle.Classify(tr.TrackName, tr.AlbumName).Tier == instrumentaltitle.Strong
		}
	}
	n, hits, err := e.EnqueuePending(ctx, r.lib)
	if err != nil {
		t.Fatalf("EnqueuePending %s: %v", copyName, err)
	}
	return n, hits
}

// drain runs the worker until the queue is empty.
func (r *editionRig) drain(t *testing.T) {
	t.Helper()
	for range 10 {
		if _, err := r.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
		err := r.w.RunOnce(context.Background())
		if errors.Is(err, errQueueEmpty) {
			return
		}
		if err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	t.Fatal("queue did not drain")
}

// file reads copyName's sidecar; "" when it does not exist.
func (r *editionRig) file(t *testing.T, copyName string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir(copyName), "track.txt"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// rows is "edition|status|lane" per row, ordered by edition, comma-joined.
func (r *editionRig) rows(t *testing.T) string {
	t.Helper()
	var got string
	if err := r.db.QueryRow(`SELECT COALESCE(GROUP_CONCAT(v, ','), '') FROM (SELECT edition || '|' || status || '|' ||
		COALESCE(provider_lane, '') AS v FROM work_queue ORDER BY edition, id)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// instFetches counts provider calls made for the instrumental copy's album.
func (r *editionRig) instFetches() int {
	n := 0
	for _, tr := range r.fetch.tracks {
		if tr.AlbumName == instAlbum {
			n++
		}
	}
	return n
}

// cachedSung reports (cache rows, rows holding the sung words).
func (r *editionRig) cachedSung(t *testing.T) (int, int) {
	t.Helper()
	var all, sung int
	if err := r.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(lyrics LIKE ?), 0) FROM lyrics_cache`, "%"+sungWords+"%").Scan(&all, &sung); err != nil {
		t.Fatal(err)
	}
	return all, sung
}

func isTitleRuleMarker(body string) bool {
	return strings.Contains(body, "[source:title-rule]") && strings.Contains(body, lyrics.InstrumentalMarker) && !strings.Contains(body, sungWords)
}

// TestEditionGate_BothScanOrders: whichever copy is scanned first, and whether
// or not the first is settled before the second is scanned, the instrumental
// copy gets a title-rule marker on its own row with no fetch, the sung copy
// gets its lyrics, and the sung copy's cache entry survives. "settled
// between" with the instrumental copy first is the case of a sung copy scanned
// after its instrumental twin settled; with the sung copy first it is the
// cache door: the sung entry is cached by then and must not settle the
// instrumental copy.
func TestEditionGate_BothScanOrders(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second string
		settleBetween bool
	}{
		{"sung first, settled between", "sung", "inst", true},
		{"instrumental first, settled between", "inst", "sung", true},
		{"sung first, drained after both", "sung", "inst", false},
		{"instrumental first, drained after both", "inst", "sung", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEditionRig(t, true)
			r.scanCopy(t, tc.first)
			if tc.settleBetween {
				r.drain(t)
			}
			if n, hits := r.scanCopy(t, tc.second); n != 1 || hits != 0 {
				t.Errorf("second scan enqueued %d, cache hits %d; want 1, 0", n, hits)
			}
			r.drain(t)
			if body := r.file(t, "inst"); !isTitleRuleMarker(body) {
				t.Errorf("instrumental copy's sidecar is not a title-rule marker:\n%s", body)
			}
			if body := r.file(t, "sung"); !strings.Contains(body, sungWords) {
				t.Errorf("sung copy's sidecar = %q; want the provider's words", body)
			}
			if n := r.instFetches(); n != 0 {
				t.Errorf("provider calls for the instrumental copy = %d; want 0", n)
			}
			if r.fetch.calls != 1 {
				t.Errorf("provider calls = %d; want 1 (the sung copy)", r.fetch.calls)
			}
			if all, sung := r.cachedSung(t); all != 1 || sung != 1 {
				t.Errorf("lyrics_cache rows = %d, holding the sung words = %d; want 1, 1", all, sung)
			}
			if got, want := r.rows(t), "|done|musixmatch,instrumental|done|title-rule"; got != want {
				t.Errorf("rows = %v; want %v", got, want)
			}
		})
	}
}

// TestEditionGate_LegacyRowStillFetches: a row with an empty edition made
// before the rule was set, whose album names an instrumental release, is
// never acted on by the album verdict: the worker fetches it as it always
// has, and a rescan of its file still meets the cache door as before.
func TestEditionGate_LegacyRowStillFetches(t *testing.T) {
	ctx := context.Background()
	r := newEditionRig(t, true)
	dir := r.dir("inst")
	legacy := models.Inputs{
		Track:  models.Track{ArtistName: editionArtist, TrackName: editionTitle, AlbumName: instAlbum},
		Outdir: dir, Filename: "track.lrc", SourcePath: filepath.Join(dir, "track.flac"), LibraryID: r.lib.ID,
	}
	if _, err := queue.NewDBQueue(r.db).Enqueue(ctx, legacy, queue.PriorityScan); err != nil { // no rule: edition ''
		t.Fatal(err)
	}
	r.drain(t)
	if r.instFetches() != 1 {
		t.Errorf("provider calls for the legacy row = %d; want 1", r.instFetches())
	}
	if body := r.file(t, "inst"); !strings.Contains(body, sungWords) {
		t.Errorf("legacy row's sidecar = %q; want the provider's words", body)
	}
	if got, want := r.rows(t), "|done|musixmatch"; got != want {
		t.Errorf("rows = %v; want %v", got, want)
	}
	// The file is rescanned with its sidecar gone: the row naming it keeps the
	// empty edition, so the door reads the cache exactly as before.
	if err := os.Remove(filepath.Join(dir, "track.txt")); err != nil {
		t.Fatal(err)
	}
	if n, hits := r.scanCopy(t, "inst"); n != 0 || hits != 1 {
		t.Errorf("rescan enqueued %d, cache hits %d; want 0, 1 (a cache hit, as before #1562)", n, hits)
	}
}

// TestEditionGate_ModeOff: with the mode off both copies fold into one row,
// which is fetched once, and nothing is settled by title or album.
func TestEditionGate_ModeOff(t *testing.T) {
	r := newEditionRig(t, false)
	r.scanCopy(t, "sung")
	r.scanCopy(t, "inst")
	r.drain(t)
	if got, want := r.rows(t), "|done|musixmatch"; got != want {
		t.Errorf("rows = %v; want %v (one row)", got, want)
	}
	if r.fetch.calls != 1 {
		t.Errorf("provider calls = %d; want 1", r.fetch.calls)
	}
	for _, c := range []string{"sung", "inst"} {
		if body := r.file(t, c); strings.Contains(body, "title-rule") {
			t.Errorf("%s copy got a title-rule marker under mode off:\n%s", c, body)
		}
	}
}

// TestEditionGate_UpgradeTripMakesNoFetch: an upgrade trip of the marker row
// settles back through the gate: no fetch, the marker byte for byte, the row
// done under its title-rule lane.
func TestEditionGate_UpgradeTripMakesNoFetch(t *testing.T) {
	r := newEditionRig(t, true)
	r.scanCopy(t, "inst")
	r.drain(t)
	before := r.file(t, "inst")
	if !isTitleRuleMarker(before) {
		t.Fatalf("setup: no marker:\n%s", before)
	}
	// The sweep's own flip, forced: status, priority and the trip flag.
	if _, err := r.db.Exec(`UPDATE work_queue SET status = 'pending', upgrade_queued = 1, attempts = 0
		WHERE edition = 'instrumental'`); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	if r.fetch.calls != 0 {
		t.Errorf("provider calls = %d; want 0", r.fetch.calls)
	}
	if after := r.file(t, "inst"); after != before {
		t.Errorf("marker changed:\n%s\nwant:\n%s", after, before)
	}
	if got, want := r.rows(t), "instrumental|done|title-rule"; got != want {
		t.Errorf("rows = %v; want %v", got, want)
	}
	if all, _ := r.cachedSung(t); all != 0 {
		t.Errorf("lyrics_cache rows = %d; want 0", all)
	}
}

// TestEditionGate_HandFilesStay: a lyric or a marker placed by hand beside
// either copy stays byte for byte, and the instrumental copy is never fetched.
// Beside the instrumental copy the file lands after the scan, so the edition
// row meets it at the gate. Beside the sung copy it is there before the scan,
// so that copy is indexed settled, as the scanner indexes a file with a
// sidecar, and only the edition row is worked.
func TestEditionGate_HandFilesStay(t *testing.T) {
	manual := models.Song{Track: models.Track{ArtistName: editionArtist, TrackName: editionTitle, Instrumental: 1}, WinningLane: lyrics.ManualLaneName}
	handMark := func(r *editionRig, dir string) error { return r.lw.WriteManualMarker(manual, "track.lrc", dir) }
	for _, tc := range []struct {
		name, copyName string
		seed           func(r *editionRig, dir string) error
	}{
		{"hand lyric beside the instrumental copy", "inst", handLyric},
		{"hand mark beside the instrumental copy", "inst", handMark},
		{"hand lyric beside the sung copy", "sung", handLyric},
		{"hand mark beside the sung copy", "sung", handMark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEditionRig(t, true)
			if tc.copyName == "sung" {
				if err := tc.seed(r, r.dir("sung")); err != nil {
					t.Fatalf("seed: %v", err)
				}
				r.scanCopyAs(t, "sung", scan.StatusDone)
				r.scanCopy(t, "inst")
			} else {
				r.scanCopy(t, "sung")
				r.scanCopy(t, "inst")
				if err := tc.seed(r, r.dir("inst")); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			before := r.file(t, tc.copyName)
			r.drain(t)
			if after := r.file(t, tc.copyName); after != before {
				t.Errorf("hand file changed:\n%s\nwant:\n%s", after, before)
			}
			if n := r.instFetches(); n != 0 {
				t.Errorf("provider calls for the instrumental copy = %d; want 0", n)
			}
			if tc.copyName == "sung" && !isTitleRuleMarker(r.file(t, "inst")) {
				t.Errorf("instrumental copy's sidecar is not a title-rule marker:\n%s", r.file(t, "inst"))
			}
		})
	}
}

func handLyric(_ *editionRig, dir string) error {
	return os.WriteFile(filepath.Join(dir, "track.txt"), []byte("words typed by hand\n"), 0o600)
}

// TestEditionGate_RetaggedEditionRowSkipsTheCache: an edition row whose file
// album no longer names an instrumental release at fetch time is not settled
// by the album verdict. It is fetched, and still never reads or writes the
// cache: the sung twin's entry is neither served to it nor replaced.
func TestEditionGate_RetaggedEditionRowSkipsTheCache(t *testing.T) {
	r := newEditionRig(t, true)
	r.scanCopy(t, "sung")
	r.drain(t)
	r.scanCopy(t, "inst")
	if _, err := r.db.Exec(`UPDATE work_queue SET album = ? WHERE edition = 'instrumental'`, sungAlbum); err != nil {
		t.Fatal(err)
	}
	r.fetch.song.Lyrics.LyricsBody = "edition words from a provider"
	r.drain(t)
	if r.fetch.calls != 2 {
		t.Errorf("provider calls = %d; want 2 (the edition row reads no cache)", r.fetch.calls)
	}
	if body := r.file(t, "inst"); !strings.Contains(body, "edition words") {
		t.Errorf("edition row's sidecar = %q; want the fetched words", body)
	}
	if all, sung := r.cachedSung(t); all != 1 || sung != 1 {
		t.Errorf("lyrics_cache rows = %d, holding the sung words = %d; want 1, 1 (the edition row stores nothing)", all, sung)
	}
}

// TestEditionGate_SungVerdictDoesNotSuppress: a categorical timing verdict on
// the sung twin's row is that recording's, so it never keeps the edition copy
// from its own row. The sung copy itself is still suppressed (the control).
func TestEditionGate_SungVerdictDoesNotSuppress(t *testing.T) {
	r := newEditionRig(t, true)
	r.gen = 5
	r.scanCopy(t, "sung")
	r.drain(t)
	if _, err := r.db.Exec(`UPDATE work_queue SET timing_outcome = 'categorical', providers_version = 5 WHERE edition = ''`); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.scanCopy(t, "inst"); n != 1 {
		t.Errorf("edition copy enqueued %d; want 1", n)
	}
	if err := os.Remove(filepath.Join(r.dir("sung"), "track.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`UPDATE scan_results SET status = 'pending' WHERE file_path = ?`, filepath.Join(r.dir("sung"), "track.flac")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cache.Invalidate(context.Background(), editionArtist, editionTitle); err != nil {
		t.Fatal(err)
	}
	if n, hits := r.scanCopy(t, "sung"); n != 0 || hits != 0 {
		t.Errorf("control: sung copy enqueued %d, cache hits %d under its own categorical verdict; want 0, 0", n, hits)
	}
}

// TestEditionGate_WordRecheckStoresNothing: an edition row's word recheck
// writes its word result to the file and nothing to the cache.
func TestEditionGate_WordRecheckStoresNothing(t *testing.T) {
	primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
	rig, w := newRecheckRig(t, primary, nil, false)
	if _, err := rig.db.Exec(`UPDATE work_queue SET edition = ? WHERE id = ?`, queue.EditionInstrumental, rig.id); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got, _ := os.ReadFile(rig.lrc); !strings.Contains(string(got), "word line") {
		t.Fatalf(".lrc = %s; want the word result's cues", got)
	}
	if song, ok := rig.cached(t); ok {
		t.Fatalf("cache = %+v; want nothing stored by an edition row", song)
	}
}

// The safety rule's other half: the row was enqueued as an instrumental
// edition, but the file's album read at fetch time no longer names the
// release, so the album verdict must not settle it (#1562).
func TestEditionGate_FetchTimeAlbumDecides(t *testing.T) {
	r := newEditionRig(t, true)
	r.scanCopy(t, "inst")
	r.w.SetRecordingEnrichmentDefault(true)
	r.w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{AlbumName: sungAlbum}}).read)
	r.drain(t)
	if r.fetch.calls != 1 {
		t.Errorf("provider calls = %d; want 1 (the file album no longer passes)", r.fetch.calls)
	}
	if body := r.file(t, "inst"); strings.Contains(body, "title-rule") {
		t.Errorf("edition row settled on a stale album:\n%s", body)
	}
}
