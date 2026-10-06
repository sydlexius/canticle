package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// cacheLaneRig is a worker over real SQLite (queue + lyrics cache) and the real
// LRCWriter in a temp library, so a cache hit's provider_lane and the sidecar
// header it writes are both read back from production state (#1207).
type cacheLaneRig struct {
	db    *sql.DB
	cache *cache.CacheRepo
	id    int64
	lrc   string
}

func newCacheLaneRig(t *testing.T, primary *fakeFetcher) (*cacheLaneRig, *Worker) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	lib := t.TempDir()
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
		Outdir:     lib,
		Filename:   "track.lrc",
		SourcePath: filepath.Join(lib, "track.flac"),
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	rig := &cacheLaneRig{db: sqlDB, cache: cache.New(sqlDB), id: item.ID, lrc: filepath.Join(lib, "track.lrc")}
	w := New(q, rig.cache, primary, lyrics.NewLRCWriter(lib))
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{err: petitlyrics.ErrNoMatch}))
	w.SetProviderRecorder(q)
	w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: fallthroughFileSeconds}}).read)
	return rig, w
}

func (r *cacheLaneRig) laneAndFetched(t *testing.T) (lane, fetched string) {
	t.Helper()
	if err := r.db.QueryRow(`SELECT COALESCE(provider_lane, ''), COALESCE(fetched_at, '') FROM work_queue WHERE id = ?`,
		r.id).Scan(&lane, &fetched); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return lane, fetched
}

// TestRunOnce_CacheHitSettlesWithStoredLane: a row settled by a cache hit
// records the lane and fetch time of the fetch that stored the entry, on the
// row and in the sidecar's [source:]/[fetched:] tags.
func TestRunOnce_CacheHitSettlesWithStoredLane(t *testing.T) {
	primary := &fakeFetcher{song: fallthroughSong(90, "first fetch")}
	rig, w := newCacheLaneRig(t, primary)
	ctx := context.Background()
	fetchedAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	w.setClock(func() time.Time { return fetchedAt })
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (fetch): %v", err)
	}

	// Reopen the row with no lane and no sidecar; the provider now fails, so
	// only the cache can settle it.
	if err := os.Remove(rig.lrc); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending', provider_lane = NULL, fetched_at = NULL WHERE id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	primary.err = errors.New("provider must not be asked on a cache hit")
	w.setClock(func() time.Time { return fetchedAt.Add(72 * time.Hour) })
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (hit): %v", err)
	}
	if primary.calls != 1 {
		t.Fatalf("provider calls = %d; want 1 (the second pass must be a cache hit)", primary.calls)
	}
	lane, fetched := rig.laneAndFetched(t)
	if lane != providers.Musixmatch || !strings.HasPrefix(fetched, "2026-03-01") {
		t.Errorf("row lane %q fetched_at %q; want %s and the original fetch on 2026-03-01", lane, fetched, providers.Musixmatch)
	}
	body, err := os.ReadFile(rig.lrc)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	for _, tag := range []string{"[source:musixmatch]", "[fetched:" + fetchedAt.Format(time.RFC3339) + "]"} {
		if !strings.Contains(string(body), tag) {
			t.Errorf("sidecar lacks %s:\n%s", tag, body)
		}
	}
}

// TestRunOnce_LegacyCacheHitSettlesLaneless: an entry stored before #1207 (the
// bare models.Song encoding, which drops WinningLane/FetchedAt) still serves,
// and settles with no lane and no [source:]/[fetched:]; nothing is guessed.
func TestRunOnce_LegacyCacheHitSettlesLaneless(t *testing.T) {
	primary := &fakeFetcher{err: errors.New("provider must not be asked on a cache hit")}
	rig, w := newCacheLaneRig(t, primary)
	ctx := context.Background()
	legacy := fallthroughSong(90, "legacy entry")
	legacy.WinningLane, legacy.FetchedAt = providers.Musixmatch, time.Now()
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.cache.Store(ctx, "Synthetic Artist", "Synthetic Title", normalize.DurationBucket(fallthroughFileSeconds), string(raw)); err != nil {
		t.Fatalf("seed legacy entry: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("provider calls = %d; want 0 (a cache hit)", primary.calls)
	}
	if lane, fetched := rig.laneAndFetched(t); lane != "" || fetched != "" {
		t.Errorf("row lane %q fetched_at %q; want both unrecorded", lane, fetched)
	}
	body, err := os.ReadFile(rig.lrc)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !strings.Contains(string(body), "legacy entry") || strings.Contains(string(body), "[source:") || strings.Contains(string(body), "[fetched:") {
		t.Errorf("sidecar = %q; want the cached words with no [source:]/[fetched:]", body)
	}
}
