package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// Every artist, title and album below is invented.

const titleRuleWords = "plain words from a provider"

// titleRig is one pending row over real SQLite and a real LRCWriter, with the
// title rule ON and two counting lanes: the primary answers with an unsynced
// lyric, the fallback misses.
type titleRig struct {
	db                 *sql.DB
	q                  *queue.DBQueue
	lw                 *lyrics.LRCWriter
	w                  *Worker
	primary, secondary *fakeFetcher
	dir                string
	id                 int64
}

func newTitleRig(t *testing.T, track models.Track) *titleRig {
	t.Helper()
	return newTitleRigPaths(t, track, nil)
}

// newTitleRigPaths is newTitleRig with the row's output_paths chosen by the
// caller from the output directory (nil keeps the single default path).
func newTitleRigPaths(t *testing.T, track models.Track, paths func(dir string) []models.OutputPath) *titleRig {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := dbtest.OpenAt(ctx, t, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("dbtest.OpenAt: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	dir := t.TempDir()
	in := models.Inputs{Track: track, Outdir: dir, Filename: "track.lrc", SourcePath: filepath.Join(dir, "track.flac")}
	if paths != nil {
		in.OutputPaths = paths(dir)
	}
	item, err := q.Enqueue(ctx, in, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := &titleRig{
		db: sqlDB, q: q, lw: lyrics.NewLRCWriter(dir), dir: dir, id: item.ID,
		primary:   &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: titleRuleWords}}},
		secondary: &fakeFetcher{err: petitlyrics.ErrNoMatch},
	}
	r.lw.SetWordSyncCompanion(true)
	r.w = New(q, cache.New(sqlDB), r.primary, r.lw)
	r.w.SetFallbackProviders(providers.New(providers.PetitLyrics, r.secondary))
	r.w.SetProviderRecorder(q)
	r.w.SetTitleRule(true)
	return r
}

func (r *titleRig) run(t *testing.T) {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
		t.Fatal(err)
	}
	if err := r.w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

func (r *titleRig) fetches() int { return r.primary.calls + r.secondary.calls }

// row is "status outcome=.. lane=.. instrumental_result=.. cached=.. attempts=..".
func (r *titleRig) row(t *testing.T) string {
	t.Helper()
	var status, outcome, lane, result string
	var cached, attempts int
	if err := r.db.QueryRow(`SELECT status, COALESCE(outcome_type, ''), COALESCE(provider_lane, ''),
		COALESCE(CAST(instrumental_result AS TEXT), 'null'), (SELECT COUNT(*) FROM lyrics_cache),
		(SELECT COUNT(*) FROM lane_attempts WHERE queue_id = work_queue.id)
		FROM work_queue WHERE id = ?`, r.id).Scan(&status, &outcome, &lane, &result, &cached, &attempts); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return fmt.Sprintf("%s outcome=%s lane=%s instrumental_result=%s cached=%d attempts=%d", status, outcome, lane, result, cached, attempts)
}

// cols reads the row's named columns as one "a|b|c" string, NULL as "null".
func (r *titleRig) cols(t *testing.T, names ...string) string {
	t.Helper()
	exprs := make([]string, len(names))
	for i, n := range names {
		exprs[i] = "COALESCE(CAST(" + n + " AS TEXT), 'null')"
	}
	var got string
	if err := r.db.QueryRow(`SELECT `+strings.Join(exprs, ` || '|' || `)+` FROM work_queue WHERE id = ?`, r.id).Scan(&got); err != nil {
		t.Fatalf("read %v: %v", names, err)
	}
	return got
}

func (r *titleRig) path(name string) string { return filepath.Join(r.dir, name) }

func (r *titleRig) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(r.path(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

var strongTrack = models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Instrumental)", AlbumName: "Paper Kites"}

// TestTitleRule_StrongEmptyStemWritesMarker: a Strong row with nothing on disk
// gets a [source:title-rule] marker with no lane asked, nothing cached, no
// detector verdict recorded and no pacing wait owed. The second case seeds a
// cached lyric under the row's own key: the cache is not read either.
func TestTitleRule_StrongEmptyStemWritesMarker(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached lyric=%v", seeded), func(t *testing.T) {
			r := newTitleRig(t, strongTrack)
			wantCached := 0
			if seeded {
				if err := r.w.store(context.Background(), strongTrack, r.primary.song); err != nil {
					t.Fatalf("seed cache: %v", err)
				}
				wantCached = 1
			}
			r.run(t)
			if n := r.fetches(); n != 0 {
				t.Errorf("fetcher calls = %d; want 0", n)
			}
			want := fmt.Sprintf("done outcome=instrumental lane=title-rule instrumental_result=null cached=%d attempts=0", wantCached)
			if got := r.row(t); got != want {
				t.Errorf("row = %q\nwant  %q", got, want)
			}
			body := r.read(t, "track.txt")
			if !strings.Contains(body, "[source:title-rule]") || !strings.Contains(body, lyrics.InstrumentalMarker) || strings.Contains(body, titleRuleWords) {
				t.Errorf("track.txt is not a title-rule marker:\n%s", body)
			}
			// The tag and the row's lane are the same string, which is what makes
			// `scan purge-provenance --source title-rule` the bulk undo.
			if prov, isMarker, err := lyrics.ReadInstrumentalProvenance(r.path("track.txt")); err != nil || !isMarker || prov.Source != lyrics.TitleRuleLaneName {
				t.Errorf("marker provenance = %+v, marker=%v, %v; want source %q", prov, isMarker, err, lyrics.TitleRuleLaneName)
			}
			if _, err := os.Stat(r.path("track.lrc")); !os.IsNotExist(err) {
				t.Errorf("a .lrc landed: %v", err)
			}
			if r.w.lastItemContactedProvider {
				t.Error("a pacing wait is owed for a row that contacted no provider")
			}
		})
	}
}

// TestTitleRule_OffIsUnchanged: with the mode off the same row is fetched and
// settled exactly as before the rule existed.
func TestTitleRule_OffIsUnchanged(t *testing.T) {
	r := newTitleRig(t, strongTrack)
	r.w.SetTitleRule(false)
	r.run(t)
	if r.primary.calls != 1 {
		t.Errorf("primary calls = %d; want 1", r.primary.calls)
	}
	if got, want := r.row(t), "done outcome=unsynced lane=musixmatch instrumental_result=null cached=1 attempts=1"; got != want {
		t.Errorf("row = %q\nwant  %q", got, want)
	}
	if body := r.read(t, "track.txt"); !strings.Contains(body, titleRuleWords) {
		t.Errorf("track.txt = %q; want the provider's words", body)
	}
}

// TestTitleRule_StillFetches: titles the rule must NOT act on. The bare word
// and an a cappella title are the classifier's; the rest are the gate's own
// artist check on a Strong dash-suffix, where the text before the separator is
// the track's artist (a title tag carrying the artist) and the song is named
// with the qualifier. The unfetched cases are the controls: the same shape
// under another artist, and a bracketed qualifier, which no artist can stop.
func TestTitleRule_StillFetches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		track   models.Track
		fetched bool
	}{
		{"bare word", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Instrumental"}, true},
		{"a cappella", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (A Cappella)"}, true},
		{"karaoke", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Karaoke Instrumental)"}, true},
		{"artist prefix", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Tin Lantern Band - Minus One"}, true},
		{"artist prefix with a track number", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Tin Lantern Band - 03 - Minus One"}, true},
		{"track number then artist", models.Track{ArtistName: "Tin Lantern Band", TrackName: "03 Tin Lantern Band - Instrumental"}, true},
		{"artist starting with a number", models.Track{ArtistName: "3 Tin Lanterns", TrackName: "3 Tin Lanterns - Instrumental"}, true},
		{"album artist prefix", models.Track{ArtistName: "Glass Harbor", AlbumArtist: "Tin Lantern Band", TrackName: "Tin Lantern Band - Off Vocal"}, true},
		{"one artist of several", models.Track{ArtistName: "Tin Lantern Band feat. Paper Kites", TrackName: "Tin Lantern Band - Minus One"}, true},
		{"no artist to compare", models.Track{TrackName: "Glass Harbor - Minus One"}, true},
		{"article on the title only", models.Track{ArtistName: "Tin Lantern Band", TrackName: "The Tin Lantern Band - Minus One"}, true},
		{"article on the tag only", models.Track{ArtistName: "The Tin Lantern Band", TrackName: "Tin Lantern Band - Minus One"}, true},
		{"title credits a guest the tag lacks", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Tin Lantern Band feat. Paper Kites - Minus One"}, true},
		{"ampersand spelled out", models.Track{ArtistName: "Tin & Lantern", TrackName: "Tin and Lantern - Minus One"}, true},
		{"artist, title, then qualifier", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Tin Lantern Band - Glass Harbor - Instrumental"}, true},
		// The compilation shape: the performer is the track artist and the album
		// artist is someone else, so the resolved artist is not the prefix.
		{"track artist prefix on a compilation", models.Track{ArtistName: "Tin Lantern Band", AlbumArtist: "Paper Kites Collective", TrackName: "Tin Lantern Band - Minus One"}, true},
		// A placeholder is no artist: nothing was compared, so nothing settles.
		{"placeholder artist", models.Track{ArtistName: "Various Artists", TrackName: "Glass Harbor - Minus One"}, true},
		{"placeholder artist and album artist", models.Track{ArtistName: "Unknown Artist", AlbumArtist: "V.A.", TrackName: "Glass Harbor - Minus One"}, true},
		{"suffix on another title", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor - Minus One"}, false},
		{"one real artist beside a placeholder", models.Track{ArtistName: "Tin Lantern Band", AlbumArtist: "Various Artists", TrackName: "Glass Harbor - Minus One"}, false},
		// The bracket position needs no artist check at all.
		{"bracket with no artist", models.Track{TrackName: "Glass Harbor (Instrumental)"}, false},
		{"bracket on a title equal to the artist", models.Track{ArtistName: "Glass Harbor", TrackName: "Glass Harbor (Instrumental)"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTitleRig(t, tc.track)
			r.run(t)
			body := r.read(t, "track.txt")
			if tc.fetched {
				if r.primary.calls != 1 || !strings.Contains(body, titleRuleWords) {
					t.Errorf("primary calls = %d, track.txt = %q; want one fetch and the provider's words", r.primary.calls, body)
				}
				return
			}
			if r.fetches() != 0 || !strings.Contains(body, "[source:title-rule]") {
				t.Errorf("fetches = %d, track.txt = %q; want a title-rule marker and no fetch", r.fetches(), body)
			}
		})
	}
}

// TestTitleRule_NeverReplacesAFile: a Strong row whose stem already holds a
// lyric, or a marker placed by hand, keeps that file byte for byte with no lane
// asked. The writer is forced, as under `serve --update`: the rule must not
// inherit that. The row settles describing the kept file, never the rule.
func TestTitleRule_NeverReplacesAFile(t *testing.T) {
	lyric := models.Song{Track: strongTrack, Lyrics: models.Lyrics{LyricsBody: "words already on disk"}}
	manual := models.Song{Track: strongTrack, WinningLane: lyrics.ManualLaneName}
	manual.Track.Instrumental = 1
	for _, tc := range []struct {
		name    string
		seed    func(*titleRig) error
		upgrade bool
		outcome string
	}{
		{"lyric, ordinary pass", func(r *titleRig) error { return r.lw.WriteLRC(lyric, "track.lrc", r.dir) }, false, "unsynced"},
		{"lyric, upgrade trip", func(r *titleRig) error { return r.lw.WriteLRC(lyric, "track.lrc", r.dir) }, true, "unsynced"},
		{"manual marker", func(r *titleRig) error { return r.lw.WriteManualMarker(manual, "track.lrc", r.dir) }, false, "instrumental"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTitleRig(t, strongTrack)
			if err := tc.seed(r); err != nil {
				t.Fatalf("seed: %v", err)
			}
			before := r.read(t, "track.txt")
			if tc.upgrade {
				if _, err := r.db.Exec(`UPDATE work_queue SET status = 'done', outcome_type = 'unsynced',
					completed_at = '2026-08-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
					t.Fatal(err)
				}
				if flipped, err := r.q.MarkUpgradeQueued(context.Background(), []int64{r.id}, time.Now().Add(-7*24*time.Hour)); err != nil || len(flipped) != 1 {
					t.Fatalf("flip = %v, %v", flipped, err)
				}
			}
			r.lw.SetForceOverwrite(true)
			r.run(t)
			if n := r.fetches(); n != 0 {
				t.Errorf("fetcher calls = %d; want 0", n)
			}
			if after := r.read(t, "track.txt"); after != before {
				t.Errorf("track.txt changed:\n%s\nwant:\n%s", after, before)
			}
			want := fmt.Sprintf("done outcome=%s lane= instrumental_result=null cached=0 attempts=0", tc.outcome)
			if got := r.row(t); got != want {
				t.Errorf("row = %q\nwant  %q", got, want)
			}
			if !tc.upgrade {
				return
			}
			// The trip settles back onto its file record (SettleUpgradeTrip): the
			// completion time still describes the kept file and the trip counts
			// toward the sweep's escalating hold, so the row is not re-tripped at once.
			var armed, misses int
			var completed string
			if err := r.db.QueryRow(`SELECT upgrade_queued, upgrade_miss_count, completed_at FROM work_queue WHERE id = ?`, r.id).
				Scan(&armed, &misses, &completed); err != nil || armed != 0 || misses != 1 || completed != "2026-08-01T00:00:00Z" {
				t.Errorf("armed = %d, upgrade_miss_count = %d, completed_at = %q, %v; want 0, 1, the original time", armed, misses, completed, err)
			}
		})
	}
}

// TestTitleRule_WordRecheckRowMakesNoFetch: a settled .lrc flipped back for
// word timings settles with no word verdict to add, no lane asked, and its
// file untouched.
func TestTitleRule_WordRecheckRowMakesNoFetch(t *testing.T) {
	ctx := context.Background()
	r := newTitleRig(t, strongTrack)
	settled := fallthroughSong(90, "settled line")
	if err := r.lw.WriteLRC(settled, "track.lrc", r.dir); err != nil {
		t.Fatalf("seed .lrc: %v", err)
	}
	before := r.read(t, "track.lrc")
	if _, err := r.q.Dequeue(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.q.SetOutcomeType(ctx, r.id, "synced"); err != nil {
		t.Fatal(err)
	}
	if err := r.q.Complete(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	if flipped, err := r.q.MarkWordRecheckQueued(ctx, []int64{r.id}, queue.WordRecheckOptions{}, nil); err != nil || len(flipped) != 1 {
		t.Fatalf("flip = %d, %v; want 1 row", len(flipped), err)
	}
	r.primary.song = recheckSong("word line", true, models.WordAnswerServed)
	r.run(t)
	if n := r.fetches(); n != 0 {
		t.Errorf("fetcher calls = %d; want 0", n)
	}
	if after := r.read(t, "track.lrc"); after != before {
		t.Errorf("track.lrc changed:\n%s\nwant:\n%s", after, before)
	}
	var status, state string
	if err := r.db.QueryRow(`SELECT status, COALESCE(word_timing_state, '') FROM work_queue WHERE id = ?`, r.id).Scan(&status, &state); err != nil {
		t.Fatal(err)
	}
	if status != queue.StatusDone || state != queue.WordTimingAbsent {
		t.Errorf("row = %s/%q; want done, absent", status, state)
	}
}

// TestTitleRule_KaraokeSkipsDetectorFirst: under ordering = "front" the
// detector answers before any provider, and it hears a karaoke backing track
// as instrumental. With the rule on, a karaoke-titled row leaves the detector
// out and gets its lyrics. With the rule off the detector settles it, as it
// always has (the control, and the mode-off guarantee).
func TestTitleRule_KaraokeSkipsDetectorFirst(t *testing.T) {
	for _, tc := range []struct {
		name          string
		track         models.Track
		rule          bool
		detectorCalls int
		lane          string
	}{
		{"karaoke title", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Karaoke Version)"}, true, 0, "musixmatch"},
		{"karaoke album", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Paper Kites Karaoke"}, true, 0, "musixmatch"},
		{"rule off", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Karaoke Version)"}, false, 1, "detector"},
		{"ordinary title", models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor"}, true, 1, "detector"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTitleRig(t, tc.track)
			det := &fakeDetector{instrumental: true, version: "9.9.9"}
			r.w.EnableAudioDetector(det)
			r.w.SetInstrumentalDetectionDefault(true)
			r.w.SetDetectorOrdering("front")
			r.w.SetTitleRule(tc.rule)
			r.run(t)
			var lane string
			if err := r.db.QueryRow(`SELECT COALESCE(provider_lane, '') FROM work_queue WHERE id = ?`, r.id).Scan(&lane); err != nil {
				t.Fatal(err)
			}
			if len(det.calls) != tc.detectorCalls || lane != tc.lane {
				t.Errorf("detector calls = %d, lane = %q; want %d, %q", len(det.calls), lane, tc.detectorCalls, tc.lane)
			}
			if body := r.read(t, "track.txt"); (tc.lane == "musixmatch") != strings.Contains(body, titleRuleWords) {
				t.Errorf("track.txt = %q; lane %q", body, tc.lane)
			}
		})
	}
}

// TestTitleRule_KeepsAnyExistingMarker: a marker already on the stem stays byte
// for byte whoever wrote it (the detector, a provider, or a person typing the
// marker line into a file), and the row's lane is left as it was, never
// restamped title-rule: `scan purge-provenance --source title-rule` must select
// only markers the rule originated.
func TestTitleRule_KeepsAnyExistingMarker(t *testing.T) {
	marker := models.Song{Track: strongTrack}
	marker.Track.Instrumental = 1
	detector, provider := marker, marker
	detector.WinningLane, detector.DetectorVersion = lyrics.DetectorLaneName, "9.9.9"
	provider.WinningLane = "petitlyrics"
	for _, tc := range []struct {
		name, lane, tag string
		seed            func(*titleRig) error
	}{
		{"detector marker", "detector", "[dv:9.9.9]", func(r *titleRig) error { return r.lw.WriteLRC(detector, "track.lrc", r.dir) }},
		{"provider marker", "petitlyrics", "[source:petitlyrics]", func(r *titleRig) error { return r.lw.WriteLRC(provider, "track.lrc", r.dir) }},
		{"untagged marker", "", lyrics.InstrumentalMarker, func(r *titleRig) error {
			return os.WriteFile(r.path("track.txt"), []byte(lyrics.InstrumentalMarker+"\n"), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTitleRig(t, strongTrack)
			if err := tc.seed(r); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if tc.lane != "" {
				if err := r.q.SetProviderLane(context.Background(), r.id, tc.lane, ""); err != nil {
					t.Fatal(err)
				}
			}
			before := r.read(t, "track.txt")
			if !strings.Contains(before, tc.tag) {
				t.Fatalf("seeded marker lacks %q:\n%s", tc.tag, before)
			}
			r.run(t)
			if n := r.fetches(); n != 0 {
				t.Errorf("fetcher calls = %d; want 0", n)
			}
			if after := r.read(t, "track.txt"); after != before {
				t.Errorf("track.txt changed:\n%s\nwant:\n%s", after, before)
			}
			want := fmt.Sprintf("done outcome=instrumental lane=%s instrumental_result=null cached=0 attempts=0", tc.lane)
			if got := r.row(t); got != want {
				t.Errorf("row = %q\nwant  %q", got, want)
			}
		})
	}
}

// TestTitleRule_SettleReplacesTheRowsEarlierRecord: a row that an earlier pass
// left describing another lane's file (its licensor, a word verdict, a sync
// tier) describes only the marker once the rule settles it.
func TestTitleRule_SettleReplacesTheRowsEarlierRecord(t *testing.T) {
	r := newTitleRig(t, strongTrack)
	if _, err := r.db.Exec(`UPDATE work_queue SET provider_lane = 'innertube', upstream = 'an-earlier-licensor',
		word_timing_state = 'absent', sync_tier = 'line' WHERE id = ?`, r.id); err != nil {
		t.Fatal(err)
	}
	r.run(t)
	if got, want := r.cols(t, "status", "provider_lane", "upstream", "word_timing_state", "sync_tier"), "done|title-rule|null|null|null"; got != want {
		t.Errorf("status|lane|upstream|word_timing_state|sync_tier = %q; want %q", got, want)
	}
	// The completion provenance names the build that wrote the marker.
	if got := r.cols(t, "writer_version"); got == "null" || got == "" {
		t.Errorf("writer_version = %q; want the writing build", got)
	}
}

// TestTitleRule_MissingOutputDirectory: a missing output directory is never
// counted as a marker written. With every directory gone the row fails before
// any write; with one gone it fails unless that entry is provably stale; and a
// stale entry beside a kept file still settles as kept, not as the rule's.
func TestTitleRule_MissingOutputDirectory(t *testing.T) {
	two := func(dir string) []models.OutputPath {
		return []models.OutputPath{{Outdir: dir, Filename: "track.lrc"}, {Outdir: filepath.Join(dir, "gone"), Filename: "track.lrc"}}
	}
	gone := func(dir string) []models.OutputPath {
		return []models.OutputPath{{Outdir: filepath.Join(dir, "gone"), Filename: "track.lrc"}}
	}
	lyric := models.Song{Track: strongTrack, Lyrics: models.Lyrics{LyricsBody: "words already on disk"}}
	for _, tc := range []struct {
		name       string
		paths      func(string) []models.OutputPath
		stale      bool
		seedLyric  bool
		want       string // status|provider_lane|outcome_type|attempts
		wantMarker bool
		wantWrite  bool // last_error names a marker write, not the pre-write refusal
	}{
		{"every directory gone", gone, true, false, "failed|null|null|1", false, false},
		{"one gone and provably stale", two, true, false, "done|title-rule|instrumental|0", true, false},
		{"one gone and not stale", two, false, false, "failed|null|null|1", true, true},
		{"one gone and stale beside a kept lyric", two, true, true, "done|null|unsynced|0", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTitleRigPaths(t, strongTrack, tc.paths)
			r.w.SetOutputHealer(&fakeHealer{stale: tc.stale, probes: map[string]int{}})
			if tc.seedLyric {
				if err := r.lw.WriteLRC(lyric, "track.lrc", r.dir); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			r.run(t)
			if n := r.fetches(); n != 0 {
				t.Errorf("fetcher calls = %d; want 0", n)
			}
			if got := r.cols(t, "status", "provider_lane", "outcome_type", "attempts"); got != tc.want {
				t.Errorf("status|lane|outcome|attempts = %q; want %q", got, tc.want)
			}
			// The row with nowhere to write is refused before any write is tried,
			// with the error the ordinary path records for it.
			if lastErr := r.cols(t, "last_error"); strings.Contains(lastErr, "title-rule marker") != tc.wantWrite ||
				strings.HasPrefix(tc.want, "failed") != strings.Contains(lastErr, "output dir does not exist") {
				t.Errorf("last_error = %q; want a marker write named: %v", lastErr, tc.wantWrite)
			}
			body, err := os.ReadFile(r.path("track.txt"))
			if isMarker := err == nil && strings.Contains(string(body), "[source:title-rule]"); isMarker != tc.wantMarker {
				t.Errorf("title-rule marker on disk = %v (%v); want %v", isMarker, err, tc.wantMarker)
			}
		})
	}
}

// TestTitleRule_WriteFailureFailsTheRow: a marker that cannot be written is a
// failed attempt like any other write failure. The row is not settled, carries
// no lane or outcome, and the lanes are still not asked.
func TestTitleRule_WriteFailureFailsTheRow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	r := newTitleRig(t, strongTrack)
	if err := os.Chmod(r.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(r.dir, 0o700) })
	r.run(t)
	if n := r.fetches(); n != 0 {
		t.Errorf("fetcher calls = %d; want 0", n)
	}
	if got, want := r.cols(t, "status", "provider_lane", "outcome_type", "attempts"), "failed|null|null|1"; got != want {
		t.Errorf("status|lane|outcome|attempts = %q; want %q", got, want)
	}
	if got := r.cols(t, "last_error"); !strings.Contains(got, "title-rule marker") {
		t.Errorf("last_error = %q; want the marker write failure", got)
	}
}

// TestTitleRule_WordRecheckRowUnderWordSyncOff: with word sync off a queued
// recheck row is not the rule's to settle. It is released untouched, with no
// verdict stamped and no fetch, exactly as the recheck path releases it.
func TestTitleRule_WordRecheckRowUnderWordSyncOff(t *testing.T) {
	ctx := context.Background()
	r := newTitleRig(t, strongTrack)
	if err := r.lw.WriteLRC(fallthroughSong(90, "settled line"), "track.lrc", r.dir); err != nil {
		t.Fatalf("seed .lrc: %v", err)
	}
	if _, err := r.q.Dequeue(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.q.SetOutcomeType(ctx, r.id, "synced"); err != nil {
		t.Fatal(err)
	}
	if err := r.q.Complete(ctx, r.id); err != nil {
		t.Fatal(err)
	}
	if flipped, err := r.q.MarkWordRecheckQueued(ctx, []int64{r.id}, queue.WordRecheckOptions{}, nil); err != nil || len(flipped) != 1 {
		t.Fatalf("flip = %d, %v; want 1 row", len(flipped), err)
	}
	r.lw.SetWordSyncCompanion(false)
	r.run(t)
	if n := r.fetches(); n != 0 {
		t.Errorf("fetcher calls = %d; want 0", n)
	}
	if got, want := r.cols(t, "status", "word_timing_state"), "done|null"; got != want {
		t.Errorf("status|word_timing_state = %q; want %q (released with no verdict)", got, want)
	}
}

// TestTitleRule_KaraokeDetectorSkipIsFrontOnly: the karaoke skip exists for
// ordering = "front" alone. Under the default ordering the detector answers
// only after every provider missed, and a karaoke-titled row still reaches it.
// Under "front" the same row is deferred as an ordinary miss, never marked.
func TestTitleRule_KaraokeDetectorSkipIsFrontOnly(t *testing.T) {
	karaoke := models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Karaoke Version)"}
	for _, tc := range []struct {
		ordering      string
		detectorCalls int
		want          string // status|provider_lane|outcome_type
	}{
		{"demoted", 1, "done|detector|instrumental"},
		{"front", 0, "deferred|null|null"},
	} {
		t.Run(tc.ordering, func(t *testing.T) {
			r := newTitleRig(t, karaoke)
			r.primary.err = musixmatch.ErrNotFound // every provider misses
			det := &fakeDetector{instrumental: true, version: "9.9.9"}
			r.w.EnableAudioDetector(det)
			r.w.SetInstrumentalDetectionDefault(true)
			r.w.SetDetectorOrdering(tc.ordering)
			r.run(t)
			if len(det.calls) != tc.detectorCalls {
				t.Errorf("detector calls = %d; want %d", len(det.calls), tc.detectorCalls)
			}
			if got := r.cols(t, "status", "provider_lane", "outcome_type"); got != tc.want {
				t.Errorf("status|lane|outcome = %q; want %q", got, tc.want)
			}
			if r.primary.calls != 1 {
				t.Errorf("primary calls = %d; want 1", r.primary.calls)
			}
		})
	}
}

// TestTitleRule_AnyOccupiedOutputWritesNothing: the rule acts only on a track
// with nothing on disk, and a row with several outputs is one track. A lyric
// at ANY output keeps every output as it is: no marker lands beside it at an
// empty one, and the row settles describing the kept file with its lane left
// alone, so `purge-provenance --source title-rule` has nothing of this row's to
// delete. The order of the outputs does not matter, and a row whose outputs
// are all empty still gets its marker at each.
func TestTitleRule_AnyOccupiedOutputWritesNothing(t *testing.T) {
	two := func(dir string) []models.OutputPath {
		return []models.OutputPath{{Outdir: filepath.Join(dir, "a"), Filename: "track.lrc"}, {Outdir: filepath.Join(dir, "b"), Filename: "track.lrc"}}
	}
	lyric := models.Song{Track: strongTrack, Lyrics: models.Lyrics{LyricsBody: "words already on disk"}}
	for _, tc := range []struct {
		name, occupied string
		want           string // status|provider_lane|outcome_type|attempts
	}{
		{"lyric at the first output", "a", "done|null|unsynced|0"},
		{"lyric at the last output", "b", "done|null|unsynced|0"},
		{"every output empty", "", "done|title-rule|instrumental|0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTitleRigPaths(t, strongTrack, two)
			for _, sub := range []string{"a", "b"} {
				if err := os.Mkdir(r.path(sub), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			var before string
			if tc.occupied != "" {
				if err := r.lw.WriteLRC(lyric, "track.lrc", r.path(tc.occupied)); err != nil {
					t.Fatalf("seed: %v", err)
				}
				before = r.read(t, filepath.Join(tc.occupied, "track.txt"))
			}
			r.run(t)
			if n := r.fetches(); n != 0 {
				t.Errorf("fetcher calls = %d; want 0", n)
			}
			if got := r.cols(t, "status", "provider_lane", "outcome_type", "attempts"); got != tc.want {
				t.Errorf("status|lane|outcome|attempts = %q; want %q", got, tc.want)
			}
			for _, sub := range []string{"a", "b"} {
				entries, err := os.ReadDir(r.path(sub))
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case tc.occupied == "":
					if got := r.read(t, filepath.Join(sub, "track.txt")); !strings.Contains(got, "[source:title-rule]") {
						t.Errorf("%s: marker = %q; want the title-rule marker", sub, got)
					}
				case sub == tc.occupied:
					if after := r.read(t, filepath.Join(sub, "track.txt")); after != before || len(entries) != 1 {
						t.Errorf("%s: kept lyric changed or gained a neighbor (%d entries):\n%s", sub, len(entries), after)
					}
				case len(entries) != 0:
					t.Errorf("%s: %d file(s) written beside a kept lyric at the other output; want none", sub, len(entries))
				}
			}
		})
	}
}

// TestTitleRule_WordRecheckRowWithNoFile pins what the recheck fast path relies
// on when the settled .lrc has since vanished: it still asks no lane and writes
// nothing (a word recheck could only ever land a word-synced lyric, the one
// thing the rule exists not to fetch for this title), and the row is not
// stranded by it. A scan that finds the sidecar gone reopens the parked recheck
// row as an ordinary fetch (queue.reopenWordRecheckForScan), and the rule's
// ordinary branch then writes the marker, still with no fetch.
func TestTitleRule_WordRecheckRowWithNoFile(t *testing.T) {
	ctx := context.Background()
	park := func(t *testing.T) *titleRig {
		t.Helper()
		r := newTitleRig(t, strongTrack)
		if _, err := r.q.Dequeue(ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.q.SetOutcomeType(ctx, r.id, "synced"); err != nil {
			t.Fatal(err)
		}
		if err := r.q.Complete(ctx, r.id); err != nil {
			t.Fatal(err)
		}
		if flipped, err := r.q.MarkWordRecheckQueued(ctx, []int64{r.id}, queue.WordRecheckOptions{}, nil); err != nil || len(flipped) != 1 {
			t.Fatalf("flip = %d, %v; want 1 row", len(flipped), err)
		}
		r.primary.song = recheckSong("word line", true, models.WordAnswerServed)
		return r
	}
	t.Run("the worker reaches it first", func(t *testing.T) {
		r := park(t)
		r.run(t)
		if entries, err := os.ReadDir(r.dir); err != nil || len(entries) != 0 {
			t.Errorf("output directory holds %d file(s), %v; want nothing written", len(entries), err)
		}
		if got := r.cols(t, "status", "word_timing_state", "outcome_type", "provider_lane"); got != "done|absent|synced|null" {
			t.Errorf("status|word state|outcome|lane = %q; want done|absent|synced|null", got)
		}
		if n := r.fetches(); n != 0 {
			t.Errorf("fetcher calls = %d; want 0", n)
		}
	})
	t.Run("a scan reaches it first", func(t *testing.T) {
		r := park(t)
		in := models.Inputs{Track: strongTrack, Outdir: r.dir, Filename: "track.lrc", SourcePath: r.path("track.flac"), FromScan: true}
		if _, err := r.q.Enqueue(ctx, in, queue.PriorityScan); err != nil {
			t.Fatalf("scan enqueue: %v", err)
		}
		if got := r.cols(t, "status", "word_timing_state", "outcome_type"); got != "pending|null|null" {
			t.Fatalf("status|word state|outcome = %q; want the row reopened as an ordinary fetch", got)
		}
		r.run(t)
		if got := r.read(t, "track.txt"); !strings.Contains(got, "[source:title-rule]") {
			t.Errorf("marker = %q; want the title-rule marker", got)
		}
		if got := r.cols(t, "status", "provider_lane", "outcome_type"); got != "done|title-rule|instrumental" {
			t.Errorf("status|lane|outcome = %q; want done|title-rule|instrumental", got)
		}
		if n := r.fetches(); n != 0 {
			t.Errorf("fetcher calls = %d; want 0", n)
		}
	})
}
