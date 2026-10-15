package worker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/titlerepair"
)

// knownDurations is a scan.DurationLookup that knows every file's duration,
// as production's banked durations do for a scanned file (#952).
type knownDurations struct{ calls int }

func (d *knownDurations) Lookup(context.Context, string, int64, int64) (int, bool, error) {
	d.calls++
	return 200, true, nil
}

// splitLegacyRow builds the legacy shared row (sung copy scanned first, both
// copies folded into one row with an empty edition, settled and cached), then
// turns the mode on and runs the repair's split, which re-keys the row to the
// instrumental edition and sets the sung copy back to pending.
func splitLegacyRow(t *testing.T) *editionRig {
	t.Helper()
	r := newEditionRig(t, false)
	r.scanCopy(t, "sung")
	r.scanCopy(t, "inst")
	r.drain(t)
	if got, want := r.rows(t), "|done|musixmatch"; got != want {
		t.Fatalf("setup rows = %v; want %v", got, want)
	}
	if all, sung := r.cachedSung(t); all != 1 || sung != 1 {
		t.Fatalf("setup cache rows = %d, sung = %d; want 1, 1", all, sung)
	}
	r.rule = true
	r.q.SetEditionRule(queue.ReleaseEdition)
	r.w.SetTitleRule(true)
	res, err := titlerepair.New(r.db, nil).Split(context.Background(), titlerepair.SplitOptions{
		Apply: true, Report: func(titlerepair.SplitRecord) error { return nil },
	})
	if err != nil || res.ReKeyed != 1 || res.Pending != 1 {
		t.Fatalf("split = %+v, %v; want one row re-keyed and one twin pending", res, err)
	}
	return r
}

// TestSplitTwinGetsItsOwnRow (#1604): a legacy row shared by an instrumental
// release copy and its sung twin is split while the sung words are cached.
// The next scan must give the sung copy its own row, which the worker settles
// from the cache with no provider call, instead of reading the cache entry as
// a hit that leaves the copy done with no row and no file. It holds whether or
// not the file's duration is known (production banks it, so known is the
// normal case). A copy of the same title by another artist, scanned in the
// same pass after the twin, stays a cache hit. Names are invented.
func TestSplitTwinGetsItsOwnRow(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "duration unknown", true: "duration known"}[known], func(t *testing.T) {
			ctx := context.Background()
			r := splitLegacyRow(t)
			durations := &knownDurations{}
			if known {
				for _, d := range []string{"sung", "inst"} {
					if err := os.WriteFile(filepath.Join(r.dir(d), "track.flac"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				r.durations = durations
			}
			// The control: same title, another artist, words cached, no row.
			const otherArtist = "Paper Kite Choir"
			other := filepath.Join(r.root, "other")
			if err := os.Mkdir(other, 0o750); err != nil {
				t.Fatal(err)
			}
			raw, err := lyrics.EncodeCachedSong(models.Song{Track: models.Track{ArtistName: otherArtist, TrackName: editionTitle},
				Lyrics: models.Lyrics{LyricsBody: "other words"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.cache.Store(ctx, otherArtist, editionTitle, 0, raw); err != nil {
				t.Fatal(err)
			}
			if err := r.repo.Upsert(ctx, r.lib.ID, []models.ScanResult{{
				FilePath: filepath.Join(other, "track.flac"), Outdir: other, Filename: "track.lrc", Status: scan.StatusPending,
				Track: models.Track{ArtistName: otherArtist, TrackName: editionTitle, AlbumName: sungAlbum},
			}}, scan.UpsertOptions{}); err != nil {
				t.Fatal(err)
			}

			recs := captureLogs(t)
			if n, hits := r.enqueue(t); n != 1 || hits != 1 {
				t.Errorf("scan after the split enqueued %d, cache hits %d; want 1 (the twin), 1 (the other artist)", n, hits)
			}
			if known && durations.calls == 0 {
				t.Error("the known file duration was never looked up")
			}
			if rec := findLog(*recs, slog.LevelInfo, "past a cache hit"); rec == nil || rec.attrs["twins"].Int64() != 1 {
				t.Errorf("twins log = %+v; want twins=1", rec)
			}
			var linked int
			if err := r.db.QueryRow(`SELECT COUNT(*) FROM work_queue_scan_results j
				JOIN work_queue w ON w.id = j.work_queue_id JOIN scan_results s ON s.id = j.scan_result_id
				WHERE w.edition = '' AND s.file_path = ?`, filepath.Join(r.dir("sung"), "track.flac")).Scan(&linked); err != nil {
				t.Fatal(err)
			}
			if linked != 1 {
				t.Errorf("sung copy links to %d rows with an empty edition; want 1", linked)
			}
			r.drain(t)
			if body := r.file(t, "sung"); !strings.Contains(body, sungWords) {
				t.Errorf("sung copy's sidecar = %q; want the cached words", body)
			}
			if r.fetch.calls != 1 {
				t.Errorf("provider calls = %d; want 1 (the legacy row's only fetch)", r.fetch.calls)
			}
			if got, want := r.rows(t), "|done|musixmatch,instrumental|done|musixmatch"; got != want {
				t.Errorf("rows = %v; want %v", got, want)
			}

			// Once the sung row exists, the door reads the cache exactly as before:
			// the copy is offered again with its sidecar gone and is a cache hit.
			if err := os.Remove(filepath.Join(r.dir("sung"), "track.txt")); err != nil {
				t.Fatal(err)
			}
			if _, err := r.db.Exec(`UPDATE scan_results SET status = 'pending' WHERE file_path = ?`,
				filepath.Join(r.dir("sung"), "track.flac")); err != nil {
				t.Fatal(err)
			}
			if n, hits := r.enqueue(t); n != 0 || hits != 1 {
				t.Errorf("rescan enqueued %d, cache hits %d; want 0, 1", n, hits)
			}
		})
	}
}

// TestSplitModeOffReleaseCopyMakesNoSungRow (#1604): after a split, with the
// mode set back to off, the release copy offered again before its sung twin
// must not take the empty-edition row for itself. It is a cache hit, as with
// the mode off before the split, and the sung copy still gets that row.
func TestSplitModeOffReleaseCopyMakesNoSungRow(t *testing.T) {
	r := splitLegacyRow(t)
	r.rule = false
	r.q.SetEditionRule(nil)
	r.w.SetTitleRule(false)
	instPath := filepath.Join(r.dir("inst"), "track.flac")
	if _, err := r.db.Exec(`UPDATE scan_results SET status = 'pending' WHERE file_path = ?`, instPath); err != nil {
		t.Fatal(err)
	}
	// The sung copy is held back so the release copy is offered alone first.
	sungPath := filepath.Join(r.dir("sung"), "track.flac")
	if _, err := r.db.Exec(`UPDATE scan_results SET status = 'done' WHERE file_path = ?`, sungPath); err != nil {
		t.Fatal(err)
	}
	if n, hits := r.enqueue(t); n != 0 || hits != 1 {
		t.Errorf("release copy's scan enqueued %d, cache hits %d; want 0, 1", n, hits)
	}
	if _, err := r.db.Exec(`UPDATE scan_results SET status = 'pending' WHERE file_path = ?`, sungPath); err != nil {
		t.Fatal(err)
	}
	if n, hits := r.enqueue(t); n != 1 || hits != 0 {
		t.Errorf("sung copy's scan enqueued %d, cache hits %d; want 1, 0", n, hits)
	}
	var srcs string
	if err := r.db.QueryRow(`SELECT GROUP_CONCAT(edition || '@' || source_path, ',') FROM
		(SELECT edition, source_path FROM work_queue ORDER BY edition)`).Scan(&srcs); err != nil {
		t.Fatal(err)
	}
	if want := "@" + sungPath + ",instrumental@" + instPath; srcs != want {
		t.Errorf("rows = %v; want %v", srcs, want)
	}
}

// TestSplitTwinProbeErrorStopsTheScan: when the queue cannot say whether the
// song has a row of the empty edition, a cached entry is not taken as a hit:
// the scan stops with the error and the copy stays pending for the next one.
func TestSplitTwinProbeErrorStopsTheScan(t *testing.T) {
	ctx := context.Background()
	r := newEditionRig(t, false)
	song := models.Song{Track: models.Track{ArtistName: editionArtist, TrackName: editionTitle},
		Lyrics: models.Lyrics{LyricsBody: sungWords}}
	raw, err := lyrics.EncodeCachedSong(song)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.Store(ctx, editionArtist, editionTitle, 0, raw); err != nil {
		t.Fatal(err)
	}
	dir := r.dir("sung")
	if err := r.repo.Upsert(ctx, r.lib.ID, []models.ScanResult{{
		FilePath: filepath.Join(dir, "track.flac"), Outdir: dir, Filename: "track.lrc", Status: scan.StatusPending,
		Track: models.Track{ArtistName: editionArtist, TrackName: editionTitle, AlbumName: sungAlbum},
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatal(err)
	}
	closed := dbtest.Open(t)
	_ = closed.Close()
	e := scan.Enqueuer{Results: r.repo, Cache: r.cache, Queue: queue.NewDBQueue(closed), Priority: queue.PriorityScan}
	if _, hits, err := e.EnqueuePending(ctx, r.lib); err == nil || !strings.Contains(err.Error(), "probe the row") || hits != 0 {
		t.Fatalf("EnqueuePending = hits %d, err %v; want the probe's error and no hit", hits, err)
	}
	var status string
	if err := r.db.QueryRow(`SELECT status FROM scan_results`).Scan(&status); err != nil || status != scan.StatusPending {
		t.Errorf("scan result status = %q, %v; want pending", status, err)
	}
}
