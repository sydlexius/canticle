// Package revalidate re-judges .lrc files that are ALREADY on disk against the
// audio they sit beside, and remediates the ones the timing predicate rejects
// (#442, part of #437).
//
// It owns no predicate and no filesystem-action machinery of its own. The
// verdict is internal/timing's, reached through the internal/lyrics disk-to-Song
// seam; the exact duration is internal/audiodur's; and every mutation goes
// through internal/realign's Apply, so the backup-first, clobber-safe, fsync'd
// JSONL trail is the SAME one the realign command writes rather than a second
// remediation stack that would drift from it.
//
// SAFETY IS THE POINT, not a feature. This command moves and deletes user files
// that a previous run of canticle put there, so:
//   - Scanning never mutates. Plan and Apply are separate calls, and the CLI's
//     default is Plan only.
//   - An unknown duration FAILS OPEN. timing.Evaluate returns UnknownDuration on
//     a zero duration and no file is ever remediated on that verdict, so a cold
//     audiodur cache is merely uninformative, never destructive.
//   - Removal is a MOVE by default. A quarantined file is recoverable by moving
//     it back; hard deletion is opt-in.
//   - Nothing identifying reaches stdout. The report is counts. Per-file paths
//     go only to an operator-requested local tail file.
package revalidate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/realign"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/timing"
)

// DurationLookup returns the exact duration in seconds for the audio file at
// path, and whether one is known. Satisfied by a closure over
// *audiodur.Store.Lookup.
//
// found=false is the fail-open signal, NOT an error: an absent or stale cache
// entry is the ordinary cold-cache case (#441), and its 0 flows to
// timing.Evaluate as UnknownDuration. An error is reserved for a broken store.
type DurationLookup func(ctx context.Context, path string, mtimeNano, size int64) (int, bool, error)

// OnFail selects what happens to a MisSynced .lrc: its words are content-correct
// (Investigation-0 on #438 found a flagged overrun is the right song's words with
// the wrong timing), so the default keeps them.
type OnFail string

const (
	// Demote writes the plain words as a .txt beside the audio, then moves the
	// .lrc aside. The default.
	Demote OnFail = "demote"
	// Delete removes the .lrc without keeping its words.
	Delete OnFail = "delete"
)

// Options configures a revalidation pass.
type Options struct {
	// Roots are the directory trees to walk.
	Roots []string
	// OnFail decides the MisSynced action. Empty means Demote.
	OnFail OnFail
	// Purge hard-deletes instead of quarantining. Opt-in and irreversible.
	Purge bool
	// QuarantineDir is the root that removed .lrc files are moved under,
	// preserving their path relative to their library root so two same-named
	// sidecars cannot collide. Required unless Purge is set.
	QuarantineDir string
	// LibraryID stamps the backup records for scoped restores.
	LibraryID int64
}

// Finding is one classified .lrc. It is the per-file record the CLI may write to
// an operator-requested tail file; it is NEVER rendered to stdout, because Path
// carries the artist and title in its directory structure.
type Finding struct {
	Path     string
	Outcome  timing.TimingOutcome
	Duration int
	Overrun  float64
	Ratio    float64
	// Action is the remediation planned for this finding, or "" when none is.
	Action string
}

// Counts is the aggregate report. It is the ONLY thing safe to print.
type Counts struct {
	Scanned         int // .lrc files examined
	Ok              int
	MisSynced       int
	Categorical     int
	UnknownDuration int // no exact duration: failed open, never remediated
	NoAudio         int // a .lrc with no companion audio: realign's problem, not this one
	Errored         int // unreadable file or duration-store failure
}

// Plan is the result of a scan: what would be done, and nothing done yet.
type Plan struct {
	Counts   Counts
	Findings []Finding
	Moves    []realign.Move
}

// Revalidator walks roots, classifies, and plans remediation.
type Revalidator struct {
	lookup DurationLookup
	opts   Options
}

// New builds a Revalidator. lookup may be nil, in which case every file reads as
// unknown-duration and the pass reports without ever proposing a mutation -- the
// safe degradation, not an error.
func New(lookup DurationLookup, opts Options) *Revalidator {
	if opts.OnFail == "" {
		opts.OnFail = Demote
	}
	return &Revalidator{lookup: lookup, opts: opts}
}

// Plan walks every configured root, classifies each .lrc, and returns the
// aggregate counts plus the remediation moves that Apply would perform. It
// MUTATES NOTHING: the only filesystem calls are directory reads, stats, and
// reads of the .lrc files themselves.
func (r *Revalidator) Plan(ctx context.Context) (Plan, error) {
	var plan Plan
	for _, root := range r.opts.Roots {
		if err := r.walkRoot(ctx, root, &plan); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// walkRoot walks one root, honoring ctx cancellation between entries.
func (r *Revalidator) walkRoot(ctx context.Context, root string, plan *Plan) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if werr != nil {
			// An unreadable subtree is reported and skipped, never fatal: one
			// bad directory must not abandon the rest of a library.
			plan.Counts.Errored++
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".lrc") {
			return nil
		}
		// A symlinked sidecar is out of scope entirely: it is not counted as an
		// error (nothing is wrong with it), it is simply never a remediation
		// target, so a link cannot redirect a move or a delete out of the root.
		if fi, lerr := os.Lstat(path); lerr != nil || fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		r.classify(ctx, root, path, plan)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("revalidate: walk %q: %w", root, err)
	}
	return nil
}

// classify judges one .lrc and appends its finding (and any move) to plan.
func (r *Revalidator) classify(ctx context.Context, root, path string, plan *Plan) {
	audio, ok := companionAudio(path)
	if !ok {
		plan.Counts.NoAudio++
		plan.Findings = append(plan.Findings, Finding{Path: path, Outcome: "no_audio"})
		return
	}
	plan.Counts.Scanned++

	duration, derr := r.durationOf(ctx, audio)
	if derr != nil {
		plan.Counts.Errored++
		return
	}

	outcome, mag, _, err := lyrics.EvaluateLRCFile(path, duration)
	if err != nil {
		plan.Counts.Errored++
		return
	}
	f := Finding{Path: path, Outcome: outcome, Duration: duration, Overrun: mag.OverrunSeconds, Ratio: mag.Ratio}

	switch outcome {
	case timing.Ok:
		plan.Counts.Ok++
	case timing.UnknownDuration:
		// FAIL OPEN. The duration is not known, so there is no verdict to act
		// on. Counted, reported, never remediated -- no move is appended here
		// and that omission is the whole rail.
		plan.Counts.UnknownDuration++
	case timing.MisSynced:
		plan.Counts.MisSynced++
		mv, mok := r.demotionMove(root, path, audio)
		if mok {
			f.Action = mv.Kind
			plan.Moves = append(plan.Moves, mv)
		}
	case timing.Categorical:
		plan.Counts.Categorical++
		mv := r.removalMove(root, path)
		f.Action = mv.Kind
		plan.Moves = append(plan.Moves, mv)
	default:
		// An outcome this package does not recognize must never remediate.
		plan.Counts.Errored++
	}
	plan.Findings = append(plan.Findings, f)
}

// demotionMove builds the action for a MisSynced .lrc. Under Demote it reads
// the cues back and flattens them to the plain words via the shared
// lyrics.PlainBody, so the demoted .txt matches exactly what the accept-time
// guard would have written. A lyric that flattens to nothing (all decorative)
// has no words worth keeping, so it falls through to plain removal rather than
// writing an empty file.
func (r *Revalidator) demotionMove(root, path, audio string) (realign.Move, bool) {
	if r.opts.OnFail == Delete {
		return r.removalMove(root, path), true
	}
	synced, err := lyrics.ReadSyncedLRC(path)
	if err != nil {
		return realign.Move{}, false
	}
	body := lyrics.PlainBody(synced)
	if body == "" {
		return r.removalMove(root, path), true
	}
	mv := r.removalMove(root, path)
	mv.Kind = realign.KindDemote
	mv.Method = "revalidate-demote"
	mv.TextPath = strings.TrimSuffix(audio, filepath.Ext(audio)) + ".txt"
	mv.TextBody = body
	return mv, true
}

// removalMove builds the quarantine (default) or purge (opt-in) action for a
// .lrc. Quarantine preserves the file's path relative to root under
// QuarantineDir, so two identically-named sidecars from different albums cannot
// collide there.
func (r *Revalidator) removalMove(root, path string) realign.Move {
	mv := realign.Move{
		Orphan:    path,
		Method:    "revalidate",
		LibraryID: r.opts.LibraryID,
		Eligible:  true,
		Kind:      realign.KindQuarantine,
	}
	if r.opts.Purge {
		mv.Kind = realign.KindPurge
		return mv
	}
	mv.Target = quarantineTarget(r.opts.QuarantineDir, root, path)
	return mv
}

// quarantineTarget maps a sidecar under root to its place under quarantineDir.
// A path that is somehow not under root falls back to its base name, which the
// clobber-safe rename in realign still refuses to overwrite.
func quarantineTarget(quarantineDir, root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(path)
	}
	return filepath.Join(quarantineDir, rel)
}

// durationOf resolves the exact duration for an audio file from the injected
// cache. A miss returns 0, which is the UnknownDuration path; only a store
// failure is an error.
func (r *Revalidator) durationOf(ctx context.Context, audio string) (int, error) {
	if r.lookup == nil {
		return 0, nil
	}
	fi, err := os.Stat(audio)
	if err != nil {
		return 0, nil //nolint:nilerr // reason: an unstattable companion is unknown-duration (fail open), not a run failure
	}
	seconds, found, err := r.lookup(ctx, audio, fi.ModTime().UnixNano(), fi.Size())
	if err != nil {
		return 0, fmt.Errorf("revalidate: duration lookup: %w", err)
	}
	if !found {
		return 0, nil
	}
	return seconds, nil
}

// companionAudio returns the audio file sharing the sidecar's stem, and whether
// one exists. A .lrc with no companion is realign's problem (a renamed or
// deleted audio file), not this pass's, and is never remediated here.
func companionAudio(lrcPath string) (string, bool) {
	stem := strings.TrimSuffix(lrcPath, filepath.Ext(lrcPath))
	dir := filepath.Dir(lrcPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if !scanner.IsAudioFile(e.Name()) {
			continue
		}
		if strings.TrimSuffix(p, filepath.Ext(p)) == stem {
			return p, true
		}
	}
	return "", false
}

// Validate reports why the options cannot be applied, or nil. Called before any
// mutation so a misconfigured run fails before it touches a file rather than
// halfway through one.
func (o Options) Validate() error {
	switch o.OnFail {
	case "", Demote, Delete:
	default:
		return fmt.Errorf("revalidate: unknown --on-fail %q (want demote or delete)", o.OnFail)
	}
	if !o.Purge && strings.TrimSpace(o.QuarantineDir) == "" {
		return errors.New("revalidate: a quarantine directory is required unless --purge is set")
	}
	// A quarantine root INSIDE a scanned root re-walks its own output: the next
	// pass finds the quarantined sidecars, judges them again, and quarantines
	// them deeper. Nothing is lost, but the tree grows on every run and the
	// counts stop meaning anything. QuarantineDir defaults to
	// <db-dir>/quarantine, so an operator whose DB lives under a library root
	// hits this without doing anything unusual.
	//
	// Rejected here rather than skipped at walk time: a config that quietly
	// half-works is worse than one that refuses to start, and this is cheap to
	// state plainly before a single file moves.
	if !o.Purge {
		qAbs, qerr := filepath.Abs(o.QuarantineDir)
		if qerr != nil {
			return fmt.Errorf("revalidate: resolve quarantine dir %q: %w", o.QuarantineDir, qerr)
		}
		for _, root := range o.Roots {
			rAbs, rerr := filepath.Abs(root)
			if rerr != nil {
				return fmt.Errorf("revalidate: resolve root %q: %w", root, rerr)
			}
			// Compare lexically on absolute paths. EvalSymlinks is deliberately
			// NOT used: the quarantine dir need not exist yet, and a resolver
			// that errors on a missing path would reject a valid config.
			if qAbs == rAbs || strings.HasPrefix(qAbs, rAbs+string(filepath.Separator)) {
				return fmt.Errorf("revalidate: quarantine dir %q is inside scanned root %q; "+
					"a later pass would re-walk and re-quarantine its own output", qAbs, rAbs)
			}
		}
	}
	return nil
}
