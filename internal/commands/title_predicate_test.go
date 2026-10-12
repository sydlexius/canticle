package commands

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
)

// Mode off passes no predicate (today's behavior); strong modes classify Strong only.
func TestStrongTitlePredicate_ModeGate(t *testing.T) {
	var cfg config.Config
	if strongTitlePredicate(cfg) != nil {
		t.Fatal("default (off) mode must yield a nil predicate")
	}
	cfg.InstrumentalTitle.Mode = config.InstrumentalTitleStrong
	p := strongTitlePredicate(cfg)
	if p == nil {
		t.Fatal("strong mode must yield a predicate")
	}
	if !p(models.Track{TrackName: "Glass Harbor (Instrumental)"}) {
		t.Error("Strong title not matched")
	}
	if p(models.Track{TrackName: "Glass Harbor"}) || p(models.Track{TrackName: "Glass Harbor (A Cappella)"}) {
		t.Error("non-Strong title matched")
	}
}

// TestStrongTitlePredicate_TierContract pins that the predicate fires on the
// Strong tier ONLY and judges the title with its album: a Weak verdict (album
// signal alone) is false, and a Strong title on a karaoke album is false.
func TestStrongTitlePredicate_TierContract(t *testing.T) {
	weak := models.Track{TrackName: "Glass Harbor", AlbumName: "Low Tide Instrumentals"}
	if got := instrumentaltitle.Classify(weak.TrackName, weak.AlbumName).Tier; got != instrumentaltitle.Weak {
		t.Fatalf("fixture drifted: album-only signal classifies %v; want Weak", got)
	}
	karaoke := models.Track{TrackName: "Glass Harbor (Instrumental)", AlbumName: "Low Tide Karaoke Hits"}
	if got := instrumentaltitle.Classify(karaoke.TrackName, karaoke.AlbumName).Tier; got == instrumentaltitle.Strong {
		t.Fatalf("fixture drifted: karaoke album still classifies Strong")
	}
	for _, mode := range []config.InstrumentalTitleMode{config.InstrumentalTitleStrong, config.InstrumentalTitleAll} {
		var cfg config.Config
		cfg.InstrumentalTitle.Mode = mode
		p := strongTitlePredicate(cfg)
		if p == nil {
			t.Fatalf("mode %q: nil predicate", mode)
		}
		if p(weak) {
			t.Errorf("mode %q: Weak verdict returned true", mode)
		}
		if p(karaoke) {
			t.Errorf("mode %q: Strong title on a karaoke album returned true", mode)
		}
		if !p(models.Track{TrackName: "Glass Harbor (Instrumental)", AlbumName: "Low Tide"}) {
			t.Errorf("mode %q: Strong title on a plain album returned false", mode)
		}
	}
}

// TestScheduler_StrongTitleCacheHitReadsAsMiss proves the predicate reaches the
// scan enqueuer: under mode strong a cached entry for a Strong-titled track is
// a miss (the row is enqueued); under mode off the same entry is a cache hit
// (marked done, nothing enqueued) (#1469).
func TestScheduler_StrongTitleCacheHitReadsAsMiss(t *testing.T) {
	cases := []struct {
		mode     config.InstrumentalTitleMode
		wantRows int
		wantDone bool
	}{
		{config.InstrumentalTitleStrong, 1, false},
		{config.InstrumentalTitleOff, 0, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			ctx := context.Background()
			sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			lib, err := library.New(sqlDB).Add(ctx, "/music", "Music", models.LibrarySettings{})
			if err != nil {
				t.Fatal(err)
			}
			track := models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Instrumental)"}
			if err := scan.New(sqlDB).Upsert(ctx, lib.ID, []models.ScanResult{{
				FilePath: "/music/a.mp3", Track: track, Outdir: "/music", Filename: "a.lrc", Status: scan.StatusPending,
			}}, scan.UpsertOptions{}); err != nil {
				t.Fatal(err)
			}
			enc, err := json.Marshal(models.Song{
				Track:  track,
				Lyrics: models.Lyrics{LyricsBody: "harbor words\nmore harbor words\n"},
			})
			if err != nil {
				t.Fatal(err)
			}
			cacheRepo := cache.New(sqlDB)
			if err := cacheRepo.Store(ctx, track.ArtistName, track.TrackName, 0, string(enc)); err != nil {
				t.Fatal(err)
			}

			var cfg config.Config
			cfg.InstrumentalTitle.Mode = tc.mode
			s := scheduler(sqlDB, scanner.ScanOptions{}, nil, false, cacheRepo, nil, "", 0, strongTitlePredicate(cfg))
			if err := s.OnScanComplete(ctx, models.Library{ID: lib.ID}, nil, lib.Path, scan.TriggerScheduler); err != nil {
				t.Fatal(err)
			}
			var rows int
			if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue`).Scan(&rows); err != nil || rows != tc.wantRows {
				t.Fatalf("mode %q: work_queue rows = %d (%v); want %d", tc.mode, rows, err, tc.wantRows)
			}
			var status string
			if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE file_path = ?`, "/music/a.mp3").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if gotDone := status == scan.StatusDone; gotDone != tc.wantDone {
				t.Fatalf("mode %q: scan_results status = %q; want done = %v", tc.mode, status, tc.wantDone)
			}
		})
	}
}
