package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tierrepair"
	"github.com/sydlexius/canticle/internal/upstreamrepair"
)

// runReconcileUpstream is the backfill for #1298: fills work_queue.upstream for
// rows settled before the column existed, from the [upstream:] tag of their
// sidecar, only where the sidecar's [source:] equals the row's lane. Candidates
// come from the database, never a directory walk. Dry-run by default; --yes
// applies, backup first. Aggregate-only stdout. On demand only. Planning and
// apply live in internal/upstreamrepair; this keeps config, the database open
// and stdout.
func runReconcileUpstream(ctx context.Context, out io.Writer, args ScanReconcileUpstreamCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.OpenForBatch(ctx, cfg.DB.Path, args.Yes)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // reason: best-effort close on shutdown

	q := queue.NewDBQueue(sqlDB)
	candidates, err := upstreamrepair.List(ctx, q)
	if err != nil {
		slog.Error("reconcile-upstream: list candidates failed", "error", err)
		return 1
	}
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-upstream-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	opts := upstreamrepair.Options{Apply: args.Yes}
	if args.Yes {
		f, ferr := tierrepair.OpenBackup(backupPath)
		if ferr != nil {
			slog.Error("reconcile-upstream: open backup file failed", "path", backupPath, "error", ferr)
			return 1
		}
		defer func() { _ = f.Close() }()
		opts.Backup = f
	}

	res, err := upstreamrepair.Run(ctx, q, candidates, opts)
	if err != nil {
		// A canceled or timed-out run stays silent on stderr (as before the move); the
		// exit code is still 1.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Error("reconcile-upstream: run stopped", "error", err)
		}
		return 1
	}
	counts := res.Counts

	verb := "would fill"
	if args.Yes {
		verb = "filled"
	}
	_, _ = fmt.Fprintf(out, "reconcile-upstream: scanned %d row(s); %s %d (skipped: source_mismatch=%d no_sidecar=%d unreadable=%d no_tags=%d no_upstream=%d unknown_upstream=%d processing=%d raced=%d write_failed=%d)%s\n",
		res.Scanned, verb, counts[upstreamrepair.OutcomeFill], counts[upstreamrepair.OutcomeSourceMismatch], counts[upstreamrepair.OutcomeNoSidecar], counts[upstreamrepair.OutcomeUnreadable],
		counts[upstreamrepair.OutcomeNoTags], counts[upstreamrepair.OutcomeNoUpstream], counts[upstreamrepair.OutcomeUnknownUp], counts[upstreamrepair.OutcomeProcessing], counts[upstreamrepair.OutcomeRaced], res.WriteFailed, suffixDryRun(args.Yes))
	if res.Applied > 0 {
		_, _ = fmt.Fprintf(out, "backup of filled rows written to %s\n", backupPath)
	}
	if res.WriteFailed > 0 {
		return 1
	}
	return 0
}
