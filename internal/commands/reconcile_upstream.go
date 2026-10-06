package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/revalidate"
)

// upstreamBackupRecord is one JSONL line per filled row: the id, lane and value
// written. No path, artist or title. The prior value is always NULL, so
// restoring is "set upstream back to NULL".
type upstreamBackupRecord struct {
	ID       int64  `json:"id"`
	Lane     string `json:"lane"`
	Upstream string `json:"upstream"`
}

// Outcomes of planUpstream other than a fill.
const (
	upFill           = "fill"
	upProcessing     = "processing"
	upNoSidecar      = "no_sidecar"
	upUnreadable     = "unreadable"
	upNoTags         = "no_tags"
	upNoUpstream     = "no_upstream"
	upSourceMismatch = "source_mismatch"
)

// runReconcileUpstream is the backfill for #1298: fills work_queue.upstream for
// rows settled before the column existed, from the [upstream:] tag of their
// sidecar, only where the sidecar's [source:] equals the row's lane. Candidates
// come from the database, never a directory walk. Dry-run by default; --yes
// applies, backup first. Aggregate-only stdout. On demand only.
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
	candidates, err := q.ListUpstreamCandidates(ctx, providers.UpstreamLanes())
	if err != nil {
		slog.Error("reconcile-upstream: list candidates failed", "error", err)
		return 1
	}
	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-upstream-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backupFile *os.File
	if args.Yes {
		f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: G304 -- backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
		if ferr != nil {
			slog.Error("reconcile-upstream: open backup file failed", "path", backupPath, "error", ferr)
			return 1
		}
		backupFile = f
		lyrics.FsyncDir(filepath.Dir(backupPath))
		defer func() { _ = backupFile.Close() }()
	}

	counts := map[string]int{}
	writeFailed, applied := 0, 0
	for _, c := range candidates {
		if ctx.Err() != nil {
			return 1
		}
		upstream, outcome := planUpstream(c)
		counts[outcome]++
		if outcome != upFill || !args.Yes {
			continue
		}
		ok, err := q.SetUpstreamIfPending(ctx, c, upstream, func() error {
			return appendUpstreamBackup(backupFile, c, upstream)
		})
		switch {
		case err != nil:
			slog.Warn("reconcile-upstream: write failed; leaving row unchanged", "id", c.ID, "error", err)
			counts[outcome]--
			writeFailed++
		case !ok:
			counts[outcome]--
			counts["raced"]++
		default:
			applied++
		}
	}

	verb := "would fill"
	if args.Yes {
		verb = "filled"
	}
	_, _ = fmt.Fprintf(out, "reconcile-upstream: scanned %d row(s); %s %d (skipped: source_mismatch=%d no_sidecar=%d unreadable=%d no_tags=%d no_upstream=%d processing=%d raced=%d write_failed=%d)%s\n",
		len(candidates), verb, counts[upFill], counts[upSourceMismatch], counts[upNoSidecar], counts[upUnreadable],
		counts[upNoTags], counts[upNoUpstream], counts[upProcessing], counts["raced"], writeFailed, suffixDryRun(args.Yes))
	if applied > 0 {
		_, _ = fmt.Fprintf(out, "backup of filled rows written to %s\n", backupPath)
	}
	if writeFailed > 0 {
		return 1
	}
	return 0
}

// planUpstream decides one candidate, shared by the dry run and the apply so
// their counts cannot differ. The sidecar is DERIVED from source_path (stem +
// .lrc, else the owned .elrc, else .txt), each with the extension-case
// resolver as its miss fallback; the first regular file found is the one read.
// A symlink is never read. Returns (upstream, upFill) only when the sidecar's
// [source:] equals the row's lane and it carries an [upstream:] line; anything
// else is counted and left NULL, never guessed.
func planUpstream(c queue.UpstreamCandidate) (upstream, outcome string) {
	if c.Status == "processing" {
		return "", upProcessing
	}
	audio := strings.TrimSpace(c.AudioPath)
	if audio == "" {
		return "", upNoSidecar
	}
	stem := strings.TrimSuffix(audio, filepath.Ext(audio))
	for _, ext := range []string{".lrc", ".elrc", ".txt"} {
		path := stem + ext
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			var variant string
			var vfi os.FileInfo
			var ok bool
			if variant, vfi, ok = revalidate.ResolveSidecarCaseVariant(path); !ok {
				continue
			}
			path, fi, err = variant, vfi, nil
		}
		switch {
		case err != nil:
			return "", upUnreadable
		case !fi.Mode().IsRegular():
			return "", upUnreadable
		}
		pt, rerr := lyrics.ReadProvenanceTags(path)
		switch {
		case rerr != nil:
			return "", upUnreadable
		case pt.Source == "":
			return "", upNoTags
		case pt.Source != c.Lane:
			return "", upSourceMismatch
		case pt.Upstream == "":
			return "", upNoUpstream
		}
		return pt.Upstream, upFill
	}
	return "", upNoSidecar
}

// appendUpstreamBackup writes and fsyncs one record, write-ahead from inside
// the row's transaction.
func appendUpstreamBackup(f *os.File, c queue.UpstreamCandidate, upstream string) error {
	b, err := json.Marshal(upstreamBackupRecord{ID: c.ID, Lane: c.Lane, Upstream: upstream})
	if err != nil {
		return fmt.Errorf("marshal reconcile-upstream backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile-upstream backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync reconcile-upstream backup record: %w", err)
	}
	return nil
}
