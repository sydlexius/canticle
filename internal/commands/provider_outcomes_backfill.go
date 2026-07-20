package commands

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/sydlexius/canticle/internal/detectorbackfill"
)

// runProviderOutcomesBackfill credits historical detector settles to the
// detector lane's provider_outcomes hit counter, once per database (#548).
//
// Unlike runIdentityBackfill this is NOT run in a goroutine, and its CALL SITE
// is deliberately ahead of the worker and scheduler. Both matter, for the same
// reason: the live writer's counter increment and its provider_lane stamp are
// two separate non-transactional writes (see the KNOWN IMPRECISION note in
// detectorbackfill.BackfillProviderOutcomes), so a row caught between them
// looks uncredited to this pass and gets permanently double-credited. Running
// before any live writer exists closes that window; running synchronously but
// AFTER the worker goroutine has started does not, which is what an earlier
// revision got wrong. It is one COUNT plus one counter UPDATE against a small
// index-free predicate, so paying for it inline at startup is cheap.
//
// Best-effort and non-fatal: a failure is logged and the marker is left unset,
// so the next startup retries. The pass is atomic (counter + marker commit
// together), so a failed run credits nothing and a retry cannot double-count.
func runProviderOutcomesBackfill(ctx context.Context, sqlDB *sql.DB) {
	res, err := detectorbackfill.BackfillProviderOutcomes(ctx, sqlDB)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("provider_outcomes backfill: interrupted by shutdown; will retry on next startup")
			return
		}
		slog.Error("provider_outcomes backfill: failed; will retry on next startup", "error", err)
		return
	}
	if res.AlreadyDone {
		return
	}
	slog.Info("provider_outcomes backfill: credited historical detector settles to the detector lane",
		"credited", res.Credited,
		"note", "detector MISSES are not recoverable and are deliberately not backfilled; /metrics and the dashboard tile are expected to disagree on them")
}
