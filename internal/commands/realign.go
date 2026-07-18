package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/realign"
)

// RealignCmd re-attaches orphaned lyric sidecars (.lrc/.txt left behind when an
// audio file was renamed) to their audio via the four-tier confidence resolver in
// internal/realign: exact (provenance ISRC/MBID match), heuristic (single-candidate
// filesystem pairing gated by a name-similarity guard), ambiguous, and conflict.
// Dry-run unless --yes. It only ever changes a sidecar's stem, never its extension,
// so a synced .lrc or an instrumental .txt marker keeps its type. This command is a
// thin adapter: the resolver and apply logic live in internal/realign, shared with
// serve mode's reactive realign (#450).
type RealignCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id)" default:""`
	Yes        bool   `arg:"--yes" help:"actually rename sidecars (without it, prints what would change)"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of applied moves (default: <db-dir>/realign-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// runRealign loads config + libraries, computes the realign plan for each library
// via internal/realign, and renders/applies it. It preserves the CLI's dry-run
// default, --yes apply, JSONL backup, and per-library scoping.
func runRealign(ctx context.Context, out io.Writer, args RealignCmd) int {
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

	repo := library.New(sqlDB)
	var libs []models.Library
	if strings.TrimSpace(args.Library) != "" {
		lib, rerr := resolveLibrary(ctx, repo, args.Library)
		if rerr != nil {
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return 1
		}
		libs = []models.Library{lib}
	} else {
		libs, err = repo.List(ctx)
		if err != nil {
			slog.Error("failed to list libraries", "error", err)
			return 1
		}
	}
	if len(libs) == 0 {
		_, _ = fmt.Fprintln(out, "no libraries configured; add one with 'canticle library add'")
		return 0
	}

	rc := cfg.Realign
	suffix := ""
	if !args.Yes {
		suffix = " [dry run; pass --yes to apply]"
	}
	_, _ = fmt.Fprintf(out, "realign: %d librar%s; require_provenance=%t cross_directory=%t identity_keys=[%s] min_confidence=%g%s\n",
		len(libs), plural(len(libs), "y", "ies"), rc.RequireProvenance, rc.CrossDirectory,
		strings.Join(realign.NormalizeIdentityKeys(rc.IdentityKeys), ","), rc.MinConfidence, suffix)

	realigner := realign.New(repo, rc)
	var combined realign.Result
	for _, lib := range libs {
		if _, ok := pathutil.ResolveWithinRoot(lib.Path, lib.Path); !ok {
			// Root missing or unresolvable: report and skip rather than fail the run.
			_, _ = fmt.Fprintf(out, "skip library %q (id=%d): root %q is not accessible\n", lib.Name, lib.ID, lib.Path)
			continue
		}
		res, rerr := realigner.PlanLibrary(lib)
		if rerr != nil {
			slog.Warn("realign: plan failed; skipping library", "library", lib.Name, "root", lib.Path, "error", rerr)
			continue
		}
		combined.Moves = append(combined.Moves, res.Moves...)
		combined.Skips = append(combined.Skips, res.Skips...)
		combined.DirsChecked += res.DirsChecked
		combined.OrphansSeen += res.OrphansSeen
	}

	return renderRealign(out, args, cfg, realigner, combined)
}

// renderRealign prints planned skips and moves, applies eligible moves under --yes
// (via realign.Apply, with a backup-first clobber-safe rename), and prints the
// summary. Output is identical to the pre-extraction CLI.
func renderRealign(out io.Writer, args RealignCmd, cfg config.Config, realigner *realign.Realigner, res realign.Result) int {
	// Report ambiguous/conflict skips first (deterministic order).
	skips := res.Skips
	sort.Slice(skips, func(i, j int) bool {
		if skips[i].Kind != skips[j].Kind {
			return skips[i].Kind < skips[j].Kind
		}
		return skips[i].Path < skips[j].Path
	})
	var ambiguousN, conflictN int
	for _, s := range skips {
		switch s.Kind {
		case "ambiguous":
			ambiguousN++
		case "conflict":
			conflictN++
		}
		_, _ = fmt.Fprintf(out, "%s: %s (%s); skipped\n", s.Kind, s.Path, s.Reason)
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("realign-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}

	var exactPlanned, heuristicPlanned int
	for _, mv := range res.Moves {
		switch mv.Method {
		case "exact":
			exactPlanned++
		case "heuristic":
			heuristicPlanned++
		}
	}

	if !args.Yes {
		// Dry run: report what would move and what is gated, no filesystem change.
		gatedSkipped := 0
		for _, mv := range res.Moves {
			if !mv.Eligible {
				gatedSkipped++
				_, _ = fmt.Fprintf(out, "skip [%s]: %s -> %s (%s)\n", mv.Method, mv.Orphan, mv.Target, mv.GateReason)
				continue
			}
			_, _ = fmt.Fprintf(out, "would move [%s]: %s -> %s\n", mv.Method, mv.Orphan, mv.Target)
		}
		_, _ = fmt.Fprintf(out, "realign summary: dirs=%d orphans=%d planned=%d (exact=%d heuristic=%d) gated-skipped=%d ambiguous=%d conflict=%d%s\n",
			res.DirsChecked, res.OrphansSeen, exactPlanned+heuristicPlanned-gatedSkipped, exactPlanned, heuristicPlanned, gatedSkipped, ambiguousN, conflictN, suffixDryRun(args.Yes))
		return 0
	}

	// Apply: the CLI permits heuristic moves that survived require_provenance gating.
	applied, aerr := realigner.Apply(res.Moves, backupPath, realign.Policy{AllowHeuristic: true})
	if aerr != nil {
		slog.Error("realign: apply failed", "error", aerr)
		return 1
	}
	var exactApplied, heuristicApplied, gatedSkipped, errCount int
	for _, a := range applied {
		switch {
		case a.GatedSkipped:
			gatedSkipped++
			_, _ = fmt.Fprintf(out, "skip [%s]: %s -> %s (%s)\n", a.Move.Method, a.Move.Orphan, a.Move.Target, a.Move.GateReason)
		case a.Err != nil:
			errCount++
			slog.Warn("realign: move failed; leaving sidecar in place", "orphan", a.Move.Orphan, "target", a.Move.Target, "error", a.Err)
		default:
			switch a.Move.Method {
			case "exact":
				exactApplied++
			case "heuristic":
				heuristicApplied++
			}
			_, _ = fmt.Fprintf(out, "moved [%s]: %s -> %s\n", a.Move.Method, a.Move.Orphan, a.Move.Target)
		}
	}
	appliedN := exactApplied + heuristicApplied
	_, _ = fmt.Fprintf(out, "realign done: dirs=%d orphans=%d applied=%d (exact=%d heuristic=%d) gated-skipped=%d ambiguous=%d conflict=%d errors=%d\n",
		res.DirsChecked, res.OrphansSeen, appliedN, exactApplied, heuristicApplied, gatedSkipped, ambiguousN, conflictN, errCount)
	if appliedN > 0 {
		_, _ = fmt.Fprintf(out, "backup of applied moves written to %s\n", backupPath)
	}
	if errCount > 0 {
		return 1
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func suffixDryRun(yes bool) string {
	if yes {
		return ""
	}
	return " [dry run; pass --yes to apply]"
}
