package worker

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

// upstream reads the row's upstream column, NULL as empty.
func (r *cacheLaneRig) upstream(t *testing.T) string {
	t.Helper()
	var up string
	if err := r.db.QueryRow(`SELECT COALESCE(upstream, '') FROM work_queue WHERE id = ?`, r.id).Scan(&up); err != nil {
		t.Fatalf("read upstream: %v", err)
	}
	return up
}

// TestRunOnce_StampsUpstreamLikeTheTag (#1297): a completion from a lane whose
// result names a licensor stores it, and the stored value is the [upstream:]
// tag the sidecar got for the same completion; a result with none stores NULL.
func TestRunOnce_StampsUpstreamLikeTheTag(t *testing.T) {
	for _, tc := range []struct{ name, upstream string }{
		{"names a licensor", "lyricfind"},
		{"names none", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			song := fallthroughSong(90, "words")
			song.Upstream = tc.upstream
			rig, w := newCacheLaneRig(t, &fakeFetcher{song: song})
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			body, err := os.ReadFile(rig.lrc)
			if err != nil {
				t.Fatalf("read sidecar: %v", err)
			}
			tag := ""
			if m := regexp.MustCompile(`\[upstream:([^\]]*)\]`).FindSubmatch(body); m != nil {
				tag = string(m[1])
			}
			if got := rig.upstream(t); got != tc.upstream || tag != got {
				t.Errorf("row upstream %q, sidecar tag %q; want both %q\n%s", got, tag, tc.upstream, body)
			}
		})
	}
}

// TestRunOnce_CacheHitAndDetectorClearUpstream: neither a cache hit (an entry
// carries no upstream) nor a detector settle may keep an upstream an earlier
// attempt left on the row.
func TestRunOnce_CacheHitAndDetectorClearUpstream(t *testing.T) {
	t.Run("cache hit", func(t *testing.T) {
		song := fallthroughSong(90, "words")
		song.Upstream = "lyricfind"
		primary := &fakeFetcher{song: song}
		rig, w := newCacheLaneRig(t, primary)
		ctx := context.Background()
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce (fetch): %v", err)
		}
		if got := rig.upstream(t); got != "lyricfind" {
			t.Fatalf("upstream after fetch = %q; want lyricfind", got)
		}
		if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, rig.id); err != nil {
			t.Fatal(err)
		}
		primary.err = errors.New("provider must not be asked on a cache hit")
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce (hit): %v", err)
		}
		if lane, _ := rig.laneAndFetched(t); lane == "" || rig.upstream(t) != "" {
			t.Errorf("lane %q upstream %q; want a lane and NULL upstream", lane, rig.upstream(t))
		}
	})
	t.Run("detector", func(t *testing.T) {
		rig, w := newCacheLaneRig(t, &fakeFetcher{err: musixmatch.ErrNotFound})
		w.SetFallbackProviders()
		w.EnableAudioDetector(&fakeDetector{instrumental: true, version: "9.9.9"})
		w.SetInstrumentalDetectionDefault(true)
		if _, err := rig.db.Exec(`UPDATE work_queue SET upstream = 'lyricfind' WHERE id = ?`, rig.id); err != nil {
			t.Fatal(err)
		}
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if lane, _ := rig.laneAndFetched(t); lane != "detector" || rig.upstream(t) != "" {
			t.Errorf("lane %q upstream %q; want detector and NULL upstream", lane, rig.upstream(t))
		}
	})
}

// TestWorker_KeptWriteLeavesUpstream: a refused downgrade leaves upstream as it
// leaves provider_lane, since both still describe the kept file.
func TestWorker_KeptWriteLeavesUpstream(t *testing.T) {
	song := fallthroughSong(90, "plain")
	song.Subtitles.Lines = nil
	song.Lyrics.LyricsBody = "plain words"
	song.Upstream = "refused-licensor"
	rig, w := newRecheckRig(t, &fakeFetcher{song: song}, nil, true)
	if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending', provider_lane = 'innertube', upstream = 'lyricfind' WHERE id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var status, lane, up string
	if err := rig.db.QueryRow(`SELECT status, COALESCE(provider_lane,''), COALESCE(upstream,'') FROM work_queue WHERE id = ?`, rig.id).Scan(&status, &lane, &up); err != nil {
		t.Fatal(err)
	}
	if status != queue.StatusDone || lane != "innertube" || up != "lyricfind" {
		t.Errorf("row = %s/%q/%q; want done/innertube/lyricfind untouched", status, lane, up)
	}
}

// TestRunOnce_RejectedResultOwnsLaneAndUpstream (#1297 F1): the verify-failure
// and guard-reject exits stamp the REJECTED result's lane, and the licensor
// must move with it. A row an earlier attempt left on innertube/lyricfind, then
// rejected on a musixmatch result that names no licensor, must not read
// musixmatch/lyricfind.
func TestRunOnce_RejectedResultOwnsLaneAndUpstream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *Worker)
		song  func() models.Song
	}{
		{"guard reject", func(w *Worker) { w.EnableGuard(rejectAllGuard{reason: "script"}) }, func() models.Song {
			return fallthroughSong(90, "words")
		}},
		{"verify failure", func(w *Worker) {
			w.EnableVerification(&fakeVerifier{results: []verificationResult{{accepted: false}}}, 1)
		}, func() models.Song {
			song := fallthroughSong(90, "words")
			song.Track.TrackName = "Entirely Different Title"
			return song
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, w := newCacheLaneRig(t, &fakeFetcher{song: tc.song()})
			tc.setup(w)
			if _, err := rig.db.Exec(`UPDATE work_queue SET provider_lane = 'innertube', upstream = 'lyricfind' WHERE id = ?`, rig.id); err != nil {
				t.Fatal(err)
			}
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			lane, _ := rig.laneAndFetched(t)
			if lane == "" || lane == "innertube" || rig.upstream(t) != "" {
				t.Errorf("lane %q upstream %q; want the rejected result's lane and NULL upstream", lane, rig.upstream(t))
			}
		})
	}
}

// TestRunOnce_UpstreamFollowsLaneNotFile (#1297 F2): the row records the
// result's licensor even where the file carries no [upstream:] tag block (an
// unsynced .txt) or nothing is written at all (a categorical result).
func TestRunOnce_UpstreamFollowsLaneNotFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		song func() models.Song
	}{
		{"unsynced txt", func() models.Song {
			song := fallthroughSong(90, "words")
			song.Subtitles.Lines = nil
			song.Lyrics.LyricsBody = "plain words"
			return song
		}},
		{"categorical", func() models.Song { return fallthroughSong(9000, "words") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			song := tc.song()
			song.Upstream = "lyricfind"
			rig, w := newCacheLaneRig(t, &fakeFetcher{song: song})
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if got := rig.upstream(t); got != "lyricfind" {
				t.Errorf("upstream = %q; want lyricfind (it follows provider_lane, not the file)", got)
			}
		})
	}
}
