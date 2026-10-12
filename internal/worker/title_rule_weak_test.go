package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/orchestrator"
)

// Every artist, title and album below is invented.

var (
	weakTrack     = models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Piano Version)", AlbumName: "Paper Kites"}
	weakAlbum     = models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Paper Kites (Instrumental)"}
	bareWordTrack = models.Track{ArtistName: "Tin Lantern Band", TrackName: "Instrumental", AlbumName: "Paper Kites"}
)

// newWeakRig is the title rig under instrumental_title.mode = strong_weak with
// a detector that answers verdict, at the default (demoted) ordering.
func newWeakRig(t *testing.T, track models.Track, verdict bool) (*titleRig, *fakeDetector) {
	t.Helper()
	return newWeakRigPaths(t, track, verdict, nil)
}

func newWeakRigPaths(t *testing.T, track models.Track, verdict bool, paths func(string) []models.OutputPath) (*titleRig, *fakeDetector) {
	t.Helper()
	r := newTitleRigPaths(t, track, paths)
	det := &fakeDetector{result: detector.Result{Instrumental: verdict, Version: "9.9.9", Reusable: true}}
	r.w.EnableAudioDetector(det)
	r.w.SetInstrumentalDetectionDefault(true)
	r.w.SetTitleRuleWeak(true)
	return r, det
}

// TestWeakTitle_DetectorAgrees: the detector is asked before the cache and the
// providers, and its yes settles the row through the detector's own settle: a
// marker, lane "detector", the verdict recorded, no provider asked and no
// pacing wait owed. A cached lyric under the row's key is not read.
func TestWeakTitle_DetectorAgrees(t *testing.T) {
	for _, tc := range []struct {
		name   string
		track  models.Track
		seeded bool
	}{
		{"weak title", weakTrack, false},
		{"weak title, cached lyric", weakTrack, true},
		{"album signal", weakAlbum, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, det := newWeakRig(t, tc.track, true)
			wantCached := 0
			if tc.seeded {
				if err := r.w.store(context.Background(), tc.track, r.primary.song); err != nil {
					t.Fatalf("seed cache: %v", err)
				}
				wantCached = 1
			}
			called := func() { t.Error("a lyric provider was asked") }
			r.primary.onCall, r.secondary.onCall = called, called
			r.run(t)
			if len(det.calls) != 1 {
				t.Errorf("detector calls = %d; want 1", len(det.calls))
			}
			want := fmt.Sprintf("done outcome=instrumental lane=detector instrumental_result=1 cached=%d attempts=1", wantCached)
			if got := r.row(t); got != want {
				t.Errorf("row = %q\nwant  %q", got, want)
			}
			body := r.read(t, "track.txt")
			if !strings.Contains(body, lyrics.InstrumentalMarker) || strings.Contains(body, titleRuleWords) || strings.Contains(body, "title-rule") {
				t.Errorf("track.txt is not the detector's marker:\n%s", body)
			}
			if got := r.cols(t, "detector_version"); got != "9.9.9" {
				t.Errorf("detector_version = %q; want 9.9.9", got)
			}
			if r.w.lastItemContactedProvider {
				t.Error("a pacing wait is owed for a row that contacted no provider")
			}
		})
	}
}

// TestWeakTitle_DetectorDisagrees: a no from the detector leaves the row on
// today's path (a lyric where a provider has one, a deferred miss where none
// does) and the detector is run ONCE for the row, wherever the ordering would
// have asked it again. The bare word "Instrumental" is a song named with the
// word: with a vocal verdict it gets its lyrics.
func TestWeakTitle_DetectorDisagrees(t *testing.T) {
	for _, tc := range []struct {
		name     string
		track    models.Track
		ordering string
		miss     bool
		want     string // status|provider_lane|outcome_type
	}{
		{"demoted, lyric", weakTrack, "demoted", false, "done|musixmatch|unsynced"},
		{"demoted, every provider misses", weakTrack, "demoted", true, "deferred|null|null"},
		{"front, lyric", weakTrack, "front", false, "done|musixmatch|unsynced"},
		{"bare word", bareWordTrack, "demoted", false, "done|musixmatch|unsynced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := instrumentaltitle.Classify(tc.track.TrackName, tc.track.AlbumName); v.Tier != instrumentaltitle.Weak {
				t.Fatalf("Classify = %+v; the case needs a Weak title", v)
			}
			r, det := newWeakRig(t, tc.track, false)
			r.w.SetDetectorOrdering(tc.ordering)
			if tc.miss {
				r.primary.err = musixmatch.ErrNotFound
			}
			r.run(t)
			if len(det.calls) != 1 {
				t.Errorf("detector calls = %d; want exactly 1", len(det.calls))
			}
			if got := r.cols(t, "status", "provider_lane", "outcome_type"); got != tc.want {
				t.Errorf("status|lane|outcome = %q; want %q", got, tc.want)
			}
			if r.primary.calls != 1 {
				t.Errorf("primary calls = %d; want 1", r.primary.calls)
			}
			// The one live verdict is stored for the row's later passes.
			if got := r.cols(t, "instrumental_result", "detector_version"); tc.miss && got != "0|9.9.9" {
				t.Errorf("instrumental_result|detector_version = %q; want 0|9.9.9", got)
			}
			if !tc.miss {
				if body := r.read(t, "track.txt"); !strings.Contains(body, titleRuleWords) {
					t.Errorf("track.txt = %q; want the provider's words", body)
				}
			}
		})
	}
}

// TestWeakTitle_FrontAsksOnce: under ordering = "front" the detector already
// answers first, and the Weak path adds no second call to its yes either.
func TestWeakTitle_FrontAsksOnce(t *testing.T) {
	r, det := newWeakRig(t, weakTrack, true)
	r.w.SetDetectorOrdering("front")
	r.run(t)
	if len(det.calls) != 1 || r.fetches() != 0 {
		t.Errorf("detector calls = %d, fetches = %d; want 1, 0", len(det.calls), r.fetches())
	}
	if got, want := r.cols(t, "status", "provider_lane", "outcome_type"), "done|detector|instrumental"; got != want {
		t.Errorf("status|lane|outcome = %q; want %q", got, want)
	}
}

// TestWeakTitle_Unchanged: every row the Weak path must leave alone, each with
// a detector that WOULD say instrumental. The detector is not asked early (at
// the default ordering a provider's lyric means it is not asked at all) and
// the row gets what it gets today. A stem that already holds a lyric or a
// marker is among them: the Weak path acts only on a track with nothing on disk.
func TestWeakTitle_Unchanged(t *testing.T) {
	lyric := models.Song{Track: weakTrack, Lyrics: models.Lyrics{LyricsBody: "words already on disk"}}
	manual := models.Song{Track: weakTrack, WinningLane: lyrics.ManualLaneName}
	manual.Track.Instrumental = 1
	second := func(dir string) []models.OutputPath {
		return []models.OutputPath{{Outdir: dir, Filename: "track.lrc"}, {Outdir: filepath.Join(dir, "alt"), Filename: "track.lrc"}}
	}
	tin := "Tin Lantern Band"
	for _, tc := range []struct {
		name  string
		track models.Track
		setup func(*testing.T, *titleRig)
		want  string // status|provider_lane|outcome_type
		paths func(string) []models.OutputPath
	}{
		{name: "ordinary title", track: models.Track{ArtistName: tin, TrackName: "Glass Harbor"}, want: "done|musixmatch|unsynced"},
		{name: "a cappella title", track: models.Track{ArtistName: tin, TrackName: "Glass Harbor (A Cappella)"}, want: "done|musixmatch|unsynced"},
		{name: "karaoke title", track: models.Track{ArtistName: tin, TrackName: "Glass Harbor (Karaoke Version)"}, want: "done|musixmatch|unsynced"},
		// Strong, but declined by the artist check: not Weak either.
		{name: "strong title, declined", track: models.Track{ArtistName: tin, TrackName: tin + " - Minus One"}, want: "done|musixmatch|unsynced"},
		// Every output is probed. The lyric there is the one the provider has, so
		// the forced re-write leaves it byte-identical, where a detector settle
		// would have replaced it with a marker.
		{name: "lyric at the second output", track: weakTrack, paths: second, setup: func(t *testing.T, r *titleRig) {
			if err := os.Mkdir(r.path("alt"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := r.lw.WriteLRC(r.primary.song, "track.lrc", r.path("alt")); err != nil {
				t.Fatalf("seed: %v", err)
			}
			r.lw.SetForceOverwrite(true)
		}, want: "done|musixmatch|unsynced"},
		// A probe that fails (the directory is gone) is a no, not a yes.
		{name: "second output directory gone", track: weakTrack, paths: second, want: "failed|null|null"},
		{name: "mode strong", track: weakTrack, setup: func(_ *testing.T, r *titleRig) { r.w.SetTitleRuleWeak(false) }, want: "done|musixmatch|unsynced"},
		{name: "parallel dispatch", track: weakTrack, setup: func(_ *testing.T, r *titleRig) {
			r.w.SetRaceWait(time.Millisecond)
			r.w.SetProvidersMode(orchestrator.ModeParallel)
			// No lane is held under parallel today. Hold a real one, built as
			// rebuildOrchestrator builds it, so the mode check itself is pinned
			// for the day #528 holds the lane there.
			r.w.detectorLane = orchestrator.NewDetectorLane(r.w.audioDetector, r.w.detectorBreaker, r.w.lane.Pacer())
		}, want: "done|musixmatch|unsynced"},
		{name: "no detector", track: weakTrack, setup: func(_ *testing.T, r *titleRig) { r.w.EnableAudioDetector(nil) }, want: "done|musixmatch|unsynced"},
		{name: "open breaker", track: weakTrack, setup: func(_ *testing.T, r *titleRig) { r.w.detectorBreaker.Trip() }, want: "done|musixmatch|unsynced"},
		{name: "detection off for the item", track: weakTrack, setup: func(_ *testing.T, r *titleRig) { r.w.SetInstrumentalDetectionDefault(false) }, want: "done|musixmatch|unsynced"},
		{name: "strong title", track: strongTrack, want: "done|title-rule|instrumental"},
		{name: "lyric on disk", track: weakTrack, setup: func(t *testing.T, r *titleRig) {
			if err := r.lw.WriteLRC(lyric, "track.lrc", r.dir); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// Forced, as under `serve --update`: a detector settle would then
			// replace the lyric with a marker, so it must not be reached.
			r.lw.SetForceOverwrite(true)
		}, want: "done|musixmatch|unsynced"},
		{name: "manual marker on disk", track: weakTrack, setup: func(t *testing.T, r *titleRig) {
			if err := r.lw.WriteManualMarker(manual, "track.lrc", r.dir); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}},
		{name: "upgrade trip, file gone", track: weakTrack, setup: func(t *testing.T, r *titleRig) {
			if _, err := r.db.Exec(`UPDATE work_queue SET status = 'done', outcome_type = 'unsynced',
				completed_at = '2026-08-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
				t.Fatal(err)
			}
			if flipped, err := r.q.MarkUpgradeQueued(context.Background(), []int64{r.id}, time.Now().Add(-7*24*time.Hour)); err != nil || len(flipped) != 1 {
				t.Fatalf("flip = %v, %v", flipped, err)
			}
		}, want: "done|musixmatch|unsynced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, det := newWeakRigPaths(t, tc.track, true, tc.paths)
			if tc.setup != nil {
				tc.setup(t, r)
			}
			altFile := filepath.Join(r.dir, "alt", "track.txt")
			before, _ := os.ReadFile(altFile)
			r.run(t)
			if len(det.calls) != 0 {
				t.Errorf("detector calls = %d; want 0", len(det.calls))
			}
			if after, _ := os.ReadFile(altFile); string(after) != string(before) {
				t.Errorf("the second output's lyric changed:\n%s\nwas:\n%s", after, before)
			}
			got := r.cols(t, "status", "provider_lane", "outcome_type")
			if tc.want != "" && got != tc.want {
				t.Errorf("status|lane|outcome = %q; want %q", got, tc.want)
			}
			if strings.Contains(got, "detector") {
				t.Errorf("status|lane|outcome = %q; the detector settled the row", got)
			}
			if tc.track.TrackName == strongTrack.TrackName {
				return
			}
			if r.primary.calls != 1 {
				t.Errorf("primary calls = %d; want 1", r.primary.calls)
			}
		})
	}
}

// TestWeakTitle_DetectorErrorIsNotAVerdict: a failed detector call settles
// nothing. The row gets its lyric, and the outage has opened the detector's
// breaker, so the dispatch does not call it again.
func TestWeakTitle_DetectorErrorIsNotAVerdict(t *testing.T) {
	r, det := newWeakRig(t, weakTrack, true)
	det.err = errors.New("sidecar returned 500")
	r.run(t)
	if len(det.calls) != 1 {
		t.Errorf("detector calls = %d; want 1", len(det.calls))
	}
	if got, want := r.cols(t, "status", "provider_lane", "outcome_type", "instrumental_result"), "done|musixmatch|unsynced|null"; got != want {
		t.Errorf("status|lane|outcome|result = %q; want %q", got, want)
	}
	if body := r.read(t, "track.txt"); !strings.Contains(body, titleRuleWords) {
		t.Errorf("track.txt = %q; want the provider's words", body)
	}
}

// TestWeakTitle_DetectorNotReadyOrDown: a sidecar still booting is dialed ONCE
// per pass, and a row no provider answers is released with no miss charged on
// every pass of the lane's boot grace. An outage on the early ask is still
// named on the deferred row. Both are what the same row gets with the tier off.
func TestWeakTitle_DetectorNotReadyOrDown(t *testing.T) {
	for _, weak := range []bool{true, false} {
		r, det := newWeakRig(t, weakTrack, true)
		r.w.SetTitleRuleWeak(weak)
		det.err = fmt.Errorf("dial refused: %w", detector.ErrClassifierNotReady)
		r.primary.err = musixmatch.ErrNotFound
		for pass := 1; pass <= 5; pass++ {
			r.run(t)
			if len(det.calls) != pass {
				t.Errorf("weak=%v pass %d: detector dials = %d; want %d (one per pass)", weak, pass, len(det.calls), pass)
			}
			if got := r.cols(t, "status", "miss_count"); got != "pending|0" {
				t.Errorf("weak=%v pass %d: status|miss_count = %q; want pending|0", weak, pass, got)
			}
		}

		r, det = newWeakRig(t, weakTrack, true)
		r.w.SetTitleRuleWeak(weak)
		det.err = errors.New("sidecar returned 500")
		r.primary.err = musixmatch.ErrNotFound
		r.run(t)
		got := r.cols(t, "status", "miss_count", "last_error")
		if len(det.calls) != 1 || !strings.HasPrefix(got, "deferred|1|") || !strings.Contains(got, "sidecar returned 500") {
			t.Errorf("weak=%v outage: dials = %d, status|miss_count|last_error = %q; want 1 dial, a deferred miss naming the detector", weak, len(det.calls), got)
		}
	}
}

// TestWeakTitle_WriterLacksOccupant: without the read-only probe the stem rule
// cannot be enforced, so the row takes the ordinary path and says so loudly.
func TestWeakTitle_WriterLacksOccupant(t *testing.T) {
	r, det := newWeakRig(t, weakTrack, true)
	r.w.writer = &capturingWriter{}
	recs := captureLogs(t)
	r.run(t)
	rec := findLog(*recs, slog.LevelError, "writer lacks Occupant")
	if rec == nil || len(rec.attrs) != 1 || rec.attrs["id"].Int64() != r.id {
		t.Errorf("error log = %+v; want one naming the row id and nothing else", rec)
	}
	if len(det.calls) != 0 || r.primary.calls != 1 {
		t.Errorf("detector calls = %d, primary calls = %d; want 0, 1", len(det.calls), r.primary.calls)
	}
}
