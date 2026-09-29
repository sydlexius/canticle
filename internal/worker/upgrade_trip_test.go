package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// upgradeRig is an upgrade trip end to end (#553) over REAL SQLite and a REAL
// writer: a settled unsynced row whose track.txt is on disk (a 100s audio
// file, a distinct album artist), flipped by the sweep's own MarkUpgradeQueued.
type upgradeRig struct {
	db    *sql.DB
	q     *queue.DBQueue
	lw    *lyrics.LRCWriter
	w     *Worker
	fetch *fakeFetcher
	txt   string
	id    int64
}

const upgradeOldWords = "old plain words"

func newUpgradeRig(t *testing.T, fetch *fakeFetcher) *upgradeRig {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	dir := t.TempDir()
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:  models.Track{ArtistName: "Track Artist feat. X", AlbumArtist: "Album Artist", TrackName: "Song"},
		Outdir: dir, Filename: "track.lrc", SourcePath: "/library/track.flac",
	}, queue.PriorityScan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE work_queue SET status = 'done', outcome_type = 'unsynced', miss_count = 14,
	      completed_at = '2026-08-01T00:00:00Z' WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	lw := lyrics.NewLRCWriter()
	if err := lw.WriteLRC(models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}, Lyrics: models.Lyrics{LyricsBody: upgradeOldWords}}, "track.lrc", dir); err != nil {
		t.Fatal(err)
	}
	w := New(q, cache.New(sqlDB), fetch, lw)
	w.SetProviderRecorder(q)
	w.SetMaxMissAttempts(15)
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: fallthroughFileSeconds}}).read)
	if flipped, err := q.MarkUpgradeQueued(ctx, []int64{item.ID}, time.Now().Add(-7*24*time.Hour)); err != nil || len(flipped) != 1 {
		t.Fatalf("flip = %v, %v", flipped, err)
	}
	return &upgradeRig{db: sqlDB, q: q, lw: lw, w: w, fetch: fetch, txt: filepath.Join(dir, "track.txt"), id: item.ID}
}

// run makes the row due and runs one worker pass.
func (r *upgradeRig) run(t *testing.T) {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, r.id); err != nil {
		t.Fatal(err)
	}
	r.w.consecutiveFailures = 0
	_ = r.w.RunOnce(context.Background())
}

// row is the row's file record: status, outcome, timing, lane, miss_count, armed.
func (r *upgradeRig) row(t *testing.T) string {
	t.Helper()
	var status string
	var outcome, timingOutcome, lane sql.NullString
	var misses, armed int
	if err := r.db.QueryRow(`SELECT status, outcome_type, timing_outcome, provider_lane, miss_count, upgrade_queued FROM work_queue WHERE id = ?`, r.id).
		Scan(&status, &outcome, &timingOutcome, &lane, &misses, &armed); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s outcome=%s timing=%s lane=%s misses=%d armed=%d", status, outcome.String, timingOutcome.String, lane.String, misses, armed)
}

// kept asserts the settled row still describes the untouched track.txt.
func (r *upgradeRig) kept(t *testing.T) {
	t.Helper()
	if got, want := r.row(t), "done outcome=unsynced timing= lane= misses=14 armed=0"; got != want {
		t.Fatalf("row = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(r.txt); err != nil || string(b) != upgradeOldWords {
		t.Fatalf("track.txt = %q, %v; want the old words untouched", b, err)
	}
}

// TestUpgradeTrip_EndToEnd (#553 review I-1, M-3): the trip asks a provider
// even with a stale entry cached under the worker's resolved key, and lands a
// better result; a miss settles back to done without spending miss_count.
func TestUpgradeTrip_EndToEnd(t *testing.T) {
	ctx := context.Background()
	t.Run("upgrade lands past a stale cache entry", func(t *testing.T) {
		r := newUpgradeRig(t, &fakeFetcher{song: fallthroughSong(90, "new synced lyric")})
		stale, _ := encodeSong(models.Song{Track: models.Track{ArtistName: "Album Artist", TrackName: "Song"}, Lyrics: models.Lyrics{LyricsBody: upgradeOldWords}})
		if err := cache.New(r.db).Store(ctx, normalize.ResolveArtist("Album Artist", "Track Artist feat. X"), "Song", normalize.DurationBucket(fallthroughFileSeconds), stale); err != nil {
			t.Fatal(err)
		}
		r.run(t)
		if r.fetch.calls != 1 {
			t.Fatalf("provider calls = %d, want 1 (the stale cache entry served the trip)", r.fetch.calls)
		}
		if got := r.row(t); got != "done outcome=synced timing=ok lane=musixmatch misses=14 armed=0" {
			t.Fatalf("row = %q, want the upgraded synced record", got)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(r.txt), "track.lrc")); err != nil {
			t.Fatalf("track.lrc not written: %v", err)
		}
	})
	t.Run("miss keeps the file and spends no miss", func(t *testing.T) {
		r := newUpgradeRig(t, &fakeFetcher{err: musixmatch.ErrNotFound})
		r.run(t)
		r.kept(t)
	})
}

// TestUpgradeTrip_NothingLandedKeepsRecord (#553 review I-3): a result the
// timing guard refuses or the script guard rejects lands nothing, so the row
// keeps describing the file and stays a candidate.
func TestUpgradeTrip_NothingLandedKeepsRecord(t *testing.T) {
	for _, c := range []struct {
		name  string
		song  models.Song
		guard bool
	}{
		{"categorical", fallthroughSong(400, "another recording"), false},
		{"mis-synced", fallthroughSong(120, "right song bad timing"), false},
		{"script guard", fallthroughSong(90, "foreign script"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newUpgradeRig(t, &fakeFetcher{song: c.song})
			if c.guard {
				r.w.EnableGuard(rejectAllGuard{reason: "script"})
			}
			r.run(t)
			r.kept(t)
			if got, _ := r.q.ListUpgradeCandidates(context.Background(), time.Now().Add(365*24*time.Hour), 10); len(got) != 1 {
				t.Fatalf("candidates with the hold lifted = %v, want the row", got)
			}
		})
	}
}

// TestUpgradeTrip_ForcedWriterNeverDowngrades (#553 review I-4): serve --update
// forces the writer, but a trip's instrumental marker never replaces lyrics.
func TestUpgradeTrip_ForcedWriterNeverDowngrades(t *testing.T) {
	r := newUpgradeRig(t, &fakeFetcher{song: models.Song{Track: models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1}}})
	r.lw.SetForceOverwrite(true)
	r.run(t)
	r.kept(t)
}

// TestUpgradeTrip_FailuresSettle (#553 review I-5): a verifier rejection
// settles at once; a transport failure retries, then settles on the cap.
func TestUpgradeTrip_FailuresSettle(t *testing.T) {
	t.Run("verification reject", func(t *testing.T) {
		r := newUpgradeRig(t, &fakeFetcher{song: fallthroughSong(90, "wrong words")})
		r.w.EnableVerification(&fakeVerifier{results: []verificationResult{{accepted: false}}}, 2.0)
		r.run(t)
		r.kept(t)
	})
	t.Run("transport cap", func(t *testing.T) {
		r := newUpgradeRig(t, &fakeFetcher{err: errors.New("dial tcp: connection refused")})
		for pass := 1; pass < upgradeMaxAttempts; pass++ {
			r.run(t)
			if got := r.row(t); got != "failed outcome=unsynced timing= lane= misses=14 armed=1" {
				t.Fatalf("pass %d: row = %q, want failed and still armed", pass, got)
			}
		}
		r.run(t)
		r.kept(t)
		if r.fetch.calls != upgradeMaxAttempts {
			t.Fatalf("provider calls = %d, want %d", r.fetch.calls, upgradeMaxAttempts)
		}
	})
}

// TestUpgradeTrip_FailureAfterWriteNeverSettlesOldRecord (#553 R2-M1): once a
// trip's write landed, a later failure at the attempt cap must not settle the
// row back onto the OLD file record while the new .lrc is on disk; it takes
// the ordinary fail/retry instead.
func TestUpgradeTrip_FailureAfterWriteNeverSettlesOldRecord(t *testing.T) {
	r := newUpgradeRig(t, &fakeFetcher{song: fallthroughSong(90, "new synced lyric")})
	if _, err := r.db.Exec(`UPDATE work_queue SET attempts = ? WHERE id = ?`, upgradeMaxAttempts-1, r.id); err != nil {
		t.Fatal(err)
	}
	// The cache store after the write fails: the trip bypasses the cache
	// lookup, so the missing table is reached only by the store.
	if _, err := r.db.Exec(`DROP TABLE lyrics_cache`); err != nil {
		t.Fatal(err)
	}
	r.run(t)
	if _, err := os.Stat(filepath.Join(filepath.Dir(r.txt), "track.lrc")); err != nil {
		t.Fatalf("track.lrc not written: %v", err)
	}
	if got := r.row(t); got != "failed outcome=unsynced timing= lane=musixmatch misses=14 armed=1" {
		t.Fatalf("row = %q, want failed and still armed (not settled onto the old .txt record)", got)
	}
}
