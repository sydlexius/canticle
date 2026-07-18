package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lrcbackfill"
)

// runReconcileLRC walks the configured library roots and rewrites .lrc sidecars
// that stack multiple timestamps on one line into the expanded one-cue-per-line
// form, backing up each pristine original to <file>.lrc.orig. Dry-run by default;
// --yes applies and writes a JSONL record of every rewritten file. Files that are
// already clean are left untouched (needs-work gate), so the run is safely
// re-runnable.
func runReconcileLRC(ctx context.Context, out io.Writer, args ScanReconcileLRCCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	libRepo := library.New(sqlDB)
	var roots []string
	if strings.TrimSpace(args.Library) != "" {
		lib, rerr := resolveLibrary(ctx, libRepo, args.Library)
		if rerr != nil {
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return 1
		}
		roots = []string{lib.Path}
	} else {
		libs, lerr := libRepo.List(ctx)
		if lerr != nil {
			slog.Error("failed to list libraries", "error", lerr)
			return 1
		}
		for _, l := range libs {
			roots = append(roots, l.Path)
		}
	}
	if len(roots) == 0 {
		_, _ = fmt.Fprintln(out, "reconcile-lrc: no library roots configured")
		return 0
	}

	backupPath := args.Backup
	if backupPath == "" {
		// Sub-second precision so two runs in the same wall-clock second cannot
		// share (and clobber) a default backup filename.
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-lrc-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405.000000000")))
	}
	// Lazy backup: the file is created only on the first record written, so a
	// zero-rewrite run never creates (and never has to delete) it -- a --yes run
	// against an all-clean library, or against an operator-named --backup path,
	// leaves the filesystem untouched. Appends rather than truncates.
	var lb *lazyBackup
	var backupW io.Writer
	if args.Yes {
		lb = &lazyBackup{path: backupPath}
		backupW = lb
		defer func() {
			if cerr := lb.Close(); cerr != nil {
				slog.Warn("failed to close reconcile-lrc backup file", "path", backupPath, "error", cerr)
			}
		}()
	}

	summary, err := lrcbackfill.Run(lrcbackfill.Options{Roots: roots, Apply: args.Yes, Backup: backupW})
	if err != nil {
		slog.Error("reconcile-lrc failed", "error", err)
		return 1
	}

	verb := "would rewrite"
	if args.Yes {
		verb = "rewrote"
	}
	_, _ = fmt.Fprintf(out, "reconcile-lrc: %s %d stacked .lrc file(s) (%d scanned, %d already clean, %d skipped, %d blocked, %d errors)%s\n",
		verb, summary.Normalized, summary.Scanned, summary.Clean, summary.Skipped, summary.Blocked, summary.Errors, suffixDryRun(args.Yes))
	if summary.Blocked > 0 {
		// Blocked files are the only tally that demands follow-up, and the count
		// alone is useless without the paths -- which are in the WARN lines.
		_, _ = fmt.Fprintf(out, "%d file(s) still stacked but blocked by a pre-existing .orig; see the BLOCKED warnings above for paths\n", summary.Blocked)
	}
	if lb != nil && lb.opened() {
		_, _ = fmt.Fprintf(out, "backup records written to %s\n", backupPath)
	}
	if summary.Errors > 0 || summary.Blocked > 0 {
		// A partial reconciliation is not success: some files failed or were left
		// untouched, so callers/scripts must see a non-zero exit.
		//
		// Blocked counts here deliberately (#487). A blocked file is still stacked,
		// was left untouched, and its WARN says "operator action required" -- that is
		// the same partial reconciliation the errors rule already covers. Exiting 0
		// would tell a script "all clear" while telling a human "you must act", which
		// is precisely the benign/actionable conflation this issue removes, just at
		// the exit-code level. Before #487 blocked files were tallied as Skipped and
		// exited 0; promoting them to their own actionable tally without moving the
		// machine-readable signal would leave scripts blind to the one state that
		// demands attention.
		return 1
	}
	return 0
}

// lazyBackup is an io.Writer that opens (create-or-append) its file on the first
// Write, so a run that writes no records never creates the file.
type lazyBackup struct {
	path string
	f    *os.File
}

func (l *lazyBackup) Write(p []byte) (int, error) {
	if l.f == nil {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: path is operator-supplied (--backup) or derived from the configured db dir
		if err != nil {
			return 0, fmt.Errorf("open reconcile-lrc backup %q: %w", l.path, err)
		}
		l.f = f
	}
	return l.f.Write(p)
}

func (l *lazyBackup) opened() bool { return l.f != nil }

// Sync flushes the backup file so writeBackupRecord can make each record durable
// before the .lrc it protects is rewritten.
func (l *lazyBackup) Sync() error {
	if l.f != nil {
		return l.f.Sync()
	}
	return nil
}

func (l *lazyBackup) Close() error {
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}
