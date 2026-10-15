package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
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
			s := scheduler(sqlDB, scanner.ScanOptions{}, nil, false, cacheRepo, nil, "", 0, titleRulesFor(cfg))
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

// TestEditionRuleFor_ModeGate: mode off gives no rule (every row keeps the
// empty edition); any other mode names a plain title on an instrumental
// release an edition copy, and nothing else (#1562).
func TestEditionRuleFor_ModeGate(t *testing.T) {
	var cfg config.Config
	cfg.InstrumentalTitle.Mode = config.InstrumentalTitleOff
	if editionRuleFor(cfg) != nil || titleRulesFor(cfg) != nil {
		t.Fatal("mode off must yield no edition rule and no title rules")
	}
	for _, mode := range []config.InstrumentalTitleMode{config.InstrumentalTitleStrong, config.InstrumentalTitleAll} {
		cfg.InstrumentalTitle.Mode = mode
		rule := editionRuleFor(cfg)
		if rule == nil {
			t.Fatalf("mode %q: nil edition rule", mode)
		}
		for _, tc := range []struct {
			track models.Track
			want  string
		}{
			{models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Low Tide (Instrumental)"}, queue.EditionInstrumental},
			{models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Low Tide"}, queue.EditionNone},
			// The title decides first: a Strong title keeps its own verdict and row.
			{models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Instrumental)", AlbumName: "Low Tide (Instrumental)"}, queue.EditionNone},
			// A bare trailing word is not a release marker.
			{models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Low Tide Instrumentals"}, queue.EditionNone},
		} {
			if got := rule(tc.track); got != tc.want {
				t.Errorf("mode %q: rule(%q on %q) = %q; want %q", mode, tc.track.TrackName, tc.track.AlbumName, got, tc.want)
			}
		}
	}
}

// TestScheduler_EditionRuleWired: the scheduler's own queue carries the
// edition rule, so a scan of a sung copy and its instrumental-release twin
// makes two rows under mode strong and one under mode off (#1562).
func TestScheduler_EditionRuleWired(t *testing.T) {
	for _, tc := range []struct {
		mode     config.InstrumentalTitleMode
		wantRows string
	}{
		{config.InstrumentalTitleStrong, ",instrumental"},
		{config.InstrumentalTitleOff, ""},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			ctx := context.Background()
			sqlDB, err := dbtest.OpenAt(ctx, t, filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			lib, err := library.New(sqlDB).Add(ctx, "/music", "Music", models.LibrarySettings{})
			if err != nil {
				t.Fatal(err)
			}
			var results []models.ScanResult
			for _, album := range []string{"Low Tide", "Low Tide (Instrumental)"} {
				dir := "/music/" + album
				results = append(results, models.ScanResult{
					FilePath: dir + "/a.mp3", Outdir: dir, Filename: "a.lrc", Status: scan.StatusPending,
					Track: models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: album},
				})
			}
			if err := scan.New(sqlDB).Upsert(ctx, lib.ID, results, scan.UpsertOptions{}); err != nil {
				t.Fatal(err)
			}
			var cfg config.Config
			cfg.InstrumentalTitle.Mode = tc.mode
			s := scheduler(sqlDB, scanner.ScanOptions{}, nil, false, nil, nil, "", 0, titleRulesFor(cfg))
			if err := s.OnScanComplete(ctx, models.Library{ID: lib.ID}, nil, lib.Path, scan.TriggerScheduler); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := sqlDB.QueryRowContext(ctx, `SELECT COALESCE(GROUP_CONCAT(edition, ','), '') FROM (SELECT edition FROM work_queue ORDER BY edition)`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.wantRows {
				t.Fatalf("mode %q: row editions = %q; want %q", tc.mode, got, tc.wantRows)
			}
		})
	}
}

// The serve queue (the webhook handler's) carries the edition rule (#1562),
// or a Download webhook files an instrumental-release copy under its sung
// twin's row. runServe hands workQ to seedLaneControlsFn; the held port then
// stops it at the bind.
func TestRunServe_ServeQueueCarriesEditionRule(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", filepath.Join(t.TempDir(), "test.key"))
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")
	sqlDB, err := dbtest.OpenAt(context.Background(), t, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "[db]\npath = " + tomlString(dbPath) + "\n\n[providers]\nprimary = \"musixmatch\"\n\n[server]\naddr = " + tomlString(held.Addr().String()) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := "unreached"
	seedLaneControlsFn = func(ctx context.Context, q servedTrackLookup, _ ...providers.LyricsProvider) {
		if dq, ok := q.(*queue.DBQueue); ok {
			got, _ = dq.EditionOf(ctx, models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor", AlbumName: "Low Tide (Instrumental)"}, "")
		}
	}
	t.Cleanup(func() { seedLaneControlsFn = seedLaneControls })
	_ = runServe(context.Background(), &bytes.Buffer{}, ServeCmd{ConfigPath: cfgPath},
		func(string) musixmatch.Fetcher { return fakeFetcher{} },
		func(...string) lyrics.Writer { return fakeWriter{} })
	if got != queue.EditionInstrumental {
		t.Errorf("serve queue edition = %q; want %q (default mode)", got, queue.EditionInstrumental)
	}
}
