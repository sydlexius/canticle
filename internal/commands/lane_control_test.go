package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/providers"
)

type recordingSeeder struct {
	fakeFetcher
	seeded []models.Track
}

func (r *recordingSeeder) SeedKnownGood(t models.Track) { r.seeded = append(r.seeded, t) }

type fakeServedLookup struct {
	tracks map[string]models.Track
	err    error
	asked  []string
}

func (f *fakeServedLookup) LatestServedTrack(_ context.Context, lane string) (models.Track, bool, error) {
	f.asked = append(f.asked, lane)
	if f.err != nil {
		return models.Track{}, false, f.err
	}
	t, ok := f.tracks[lane]
	return t, ok, nil
}

// seedLaneControls seeds only a lane whose client keeps a control, only from its
// OWN lane's evidence, and a lookup failure leaves the lane unseeded rather than
// failing startup.
func TestSeedLaneControls(t *testing.T) {
	ctx := context.Background()
	win := models.Track{ArtistName: "A", TrackName: "T"}

	seeder := &recordingSeeder{}
	lookup := &fakeServedLookup{tracks: map[string]models.Track{providers.PetitLyrics: win}}
	seedLaneControls(ctx, lookup,
		providers.New(providers.Musixmatch, fakeFetcher{}),
		providers.New(providers.PetitLyrics, seeder),
		nil,
	)
	if len(seeder.seeded) != 1 || seeder.seeded[0] != win {
		t.Errorf("seeded = %+v; want exactly the petitlyrics lane's own last win", seeder.seeded)
	}
	if len(lookup.asked) != 1 || lookup.asked[0] != providers.PetitLyrics {
		t.Errorf("lanes queried = %v; want only the lane that keeps a control", lookup.asked)
	}

	empty := &recordingSeeder{}
	seedLaneControls(ctx, &fakeServedLookup{}, providers.New(providers.PetitLyrics, empty))
	if len(empty.seeded) != 0 {
		t.Errorf("a lane with no recorded win was seeded with %+v; a fresh install must stay unseeded (#607)", empty.seeded)
	}

	failed := &recordingSeeder{}
	seedLaneControls(ctx, &fakeServedLookup{err: errors.New("boom")}, providers.New(providers.PetitLyrics, failed))
	if len(failed.seeded) != 0 {
		t.Errorf("a failed lookup seeded %+v; want the lane left unseeded", failed.seeded)
	}
}

// TestRunServe_SeedsLaneControls proves the production call site: runServe
// reaches seedLaneControlsFn with the petitlyrics lane it built and the live
// work queue, which holds the recorded win.
func TestRunServe_SeedsLaneControls(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", filepath.Join(t.TempDir(), "test.key"))

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO work_queue (artist, title, status, provider_lane, completed_at)
		VALUES ('A', 'T', 'done', 'petitlyrics', '2026-09-30T00:00:00Z')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	_ = sqlDB.Close()

	// A held port makes runServe exit 1 at the bind, which is after the seed.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "[db]\npath = " + tomlString(dbPath) + "\n\n[providers]\nprimary = \"petitlyrics\"\n\n[server]\naddr = " + tomlString(held.Addr().String()) + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var sawPetit, petitSeeds, found bool
	seedLaneControlsFn = func(ctx context.Context, q servedTrackLookup, lanes ...providers.LyricsProvider) {
		for _, p := range lanes {
			if p != nil && p.Name() == providers.PetitLyrics {
				sawPetit = true
				// The lane runServe built must unwrap to a client that can take the
				// seed; otherwise seedLaneControls skips it and this test would stay
				// green on a lane that is never seeded (#1195 review F3).
				if w, ok := p.(interface{ Unwrap() providers.Fetcher }); ok {
					_, petitSeeds = w.Unwrap().(controlSeeder)
				}
			}
		}
		_, found, _ = q.LatestServedTrack(ctx, providers.PetitLyrics)
		seedLaneControls(ctx, q, lanes...)
	}
	t.Cleanup(func() { seedLaneControlsFn = seedLaneControls })

	var out bytes.Buffer
	_ = runServe(context.Background(), &out, ServeCmd{ConfigPath: cfgPath},
		func(string) musixmatch.Fetcher { return fakeFetcher{} },
		func(...string) lyrics.Writer { return fakeWriter{} })
	if !sawPetit {
		t.Fatal("runServe never handed the petitlyrics lane to seedLaneControls; a restart would start it without a control (#1195)")
	}
	if !petitSeeds {
		t.Error("the petitlyrics lane runServe built does not unwrap to a controlSeeder, so seedLaneControls skips it (#1195)")
	}
	if !found {
		t.Error("the queue runServe passed to the seed does not see the recorded petitlyrics win")
	}
}
