package commands

import (
	"context"
	"log/slog"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
)

// controlSeeder is a lane client that adjudicates a sustained miss run by
// re-fetching a known-good control track (petitlyrics, #767) and can take that
// control from durable evidence at startup (#1195).
type controlSeeder interface {
	SeedKnownGood(models.Track)
}

// The real petitlyrics client must keep satisfying controlSeeder. seedLaneControls
// discovers seeders by a runtime type assertion, so a renamed or re-signatured
// SeedKnownGood would otherwise compile cleanly and silently seed nothing (#1195
// review F3).
var _ controlSeeder = (*petitlyrics.Client)(nil)

// servedTrackLookup is the work_queue read seedLaneControls needs; satisfied by
// *queue.DBQueue.
type servedTrackLookup interface {
	LatestServedTrack(ctx context.Context, lane string) (models.Track, bool, error)
}

// seedLaneControlsFn is the serve call site's indirection, so a test can prove
// runServe reaches it with the lanes it built.
var seedLaneControlsFn = seedLaneControls

// seedLaneControls hands every lane that keeps a liveness control the most
// recent track the database records it serving (#1195).
//
// Without it a restart wipes the control, and the lane's first long miss run is
// judged by the bare count -- the #767 false positive -- which on a nightly
// restart latched the petitlyrics lane off every night. With no recorded win
// (a genuinely fresh install, #607's case) the lane stays unseeded and the
// count fallback still applies.
//
// A lookup failure is NOT fatal: it logs at Warn and leaves that lane unseeded,
// which is exactly the pre-#1195 behavior. Neither path logs the track; it is
// library metadata.
func seedLaneControls(ctx context.Context, q servedTrackLookup, lanes ...providers.LyricsProvider) {
	for _, p := range lanes {
		if p == nil {
			continue
		}
		var inner any = p
		if w, ok := p.(interface{ Unwrap() providers.Fetcher }); ok {
			inner = w.Unwrap()
		}
		seeder, ok := inner.(controlSeeder)
		if !ok {
			continue
		}
		track, found, err := q.LatestServedTrack(ctx, p.Name())
		if err != nil {
			slog.Warn("could not seed lane liveness control from the queue; the lane starts without one",
				"provider", p.Name(), "error", err)
			continue
		}
		if !found {
			continue
		}
		seeder.SeedKnownGood(track)
		slog.Debug("seeded lane liveness control from a prior win", "provider", p.Name())
	}
}
