// Package realign re-attaches orphaned lyric sidecars (.lrc/.txt left behind when
// an audio file was renamed) to their audio via a confidence resolver with these
// tiers: exact (provenance ISRC/MBID match), heuristic (single-candidate
// filesystem pairing gated by a name-similarity guard AND a runner-up margin
// against the directory's other audio), heuristic-nm (opt-in N:M
// name-similarity matching when a directory has multiple orphans and multiple
// sidecar-less audio files, pairing each orphan to its unambiguous best-scoring
// candidate only), ambiguous (multiple/zero candidates, or an N:M pairing too
// close to call, reported and skipped), and conflict (contradictory signals or an
// existing destination, reported and skipped).
//
// It is the shared core behind both the `realign` CLI command and serve mode's
// reactive realign (watcher / post-scan / Lidarr webhook). The package computes a
// structured plan (Plan*) separately from applying it (Apply), so the CLI can
// render a dry-run and serve mode can auto-apply, both from the same logic. A move
// only ever changes a sidecar's stem, never its extension, so a synced .lrc or an
// instrumental .txt marker keeps its type. Apply is backup-first and clobber-safe.
package realign

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/identity"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/scanner"
)

// LibraryLister lists and resolves configured library roots. Satisfied by
// *library.Repo.
type LibraryLister interface {
	List(ctx context.Context) ([]models.Library, error)
}

// ProvenanceReader reads the identity (ISRC/MBID) and name (artist/title) signals
// embedded in an audio file. Defaults to scanner.ReadAudioProvenance; injectable
// for tests.
type ProvenanceReader func(path string) (isrc, mbid, artist, title string, err error)

// Action kinds Apply can perform on a sidecar. They share ONE apply path and ONE
// JSONL backup trail deliberately: a second remediation stack would mean a second
// place for the backup-first, clobber-safe, fsync'd semantics to drift.
//
// KindRename is the realign resolver's own action and is the zero value, so every
// Move built before these existed keeps its behavior with no field to set.
const (
	// KindRename re-attaches an orphaned sidecar to its audio (realign).
	KindRename = ""
	// KindDemote writes a plain-words .txt beside the audio and moves the .lrc
	// aside. Used by revalidate (#442) on a MisSynced lyric: the words are
	// content-correct, only the timing is wrong.
	KindDemote = "demote"
	// KindQuarantine moves a sidecar aside without writing anything in its
	// place. Used on a Categorical lyric, which is almost certainly timed to a
	// different recording, so its words are not trustworthy either.
	KindQuarantine = "quarantine"
	// KindPurge hard-deletes a sidecar. Intentionally NON-REVERSIBLE: it is the
	// opt-in escape hatch (--purge), and its backup record is an audit trail,
	// not a restorable copy. Quarantine is the default for exactly this reason.
	KindPurge = "purge"
)

// Move is a planned sidecar action. For the realign resolver it is a rename with
// its resolved tier; Eligible is false when config gating (require_provenance)
// reports but suppresses the move. Kind selects the action Apply performs.
type Move struct {
	Orphan     string
	Target     string
	Method     string // "exact", "heuristic", "heuristic-nm", or a caller's own label
	LibraryID  int64
	Eligible   bool
	GateReason string  // why an ineligible move is suppressed (require_provenance)
	Confidence float64 // heuristic name-guard score (0 for exact / positional)

	// Kind is the action to perform; the zero value is KindRename.
	Kind string
	// TextPath and TextBody carry the demoted plain-words sidecar for
	// KindDemote. They are ignored by every other kind.
	//
	// TextBody is supplied by the caller rather than derived here because
	// deciding what counts as lyric text is internal/timing's job, not this
	// package's -- a copy of that judgment here would be the second
	// implementation the whole predicate consolidation exists to prevent.
	TextPath string
	TextBody string
}

// Skip is a reported orphan that was not moved (ambiguous or conflict), never
// guessed.
type Skip struct {
	Kind   string // "ambiguous" or "conflict"
	Path   string // orphan sidecar
	Reason string
}

// Result is the structured outcome of a plan: the moves to apply, the skips to
// report, and the corpus counters.
type Result struct {
	Moves       []Move
	Skips       []Skip
	DirsChecked int
	OrphansSeen int
}

// Applied records the outcome of one attempted move during Apply. Err is nil when
// the rename succeeded, non-nil when it was skipped or failed; GatedSkipped marks
// a move suppressed by the apply policy (ineligible / heuristic-not-allowed).
type Applied struct {
	Move         Move
	GatedSkipped bool
	Err          error
}

// Policy controls which eligible moves Apply actually performs. AllowHeuristic
// gates the heuristic tier; the exact tier always applies. The CLI passes
// AllowHeuristic=true (heuristic eligibility already encodes require_provenance);
// reactive callers pass the conservative auto_apply_heuristic value.
type Policy struct {
	AllowHeuristic bool
}

// Realigner holds the resolver's dependencies and configuration.
type Realigner struct {
	libraries LibraryLister
	cfg       config.RealignConfig
	readProv  ProvenanceReader
}

// New builds a Realigner over the given libraries and realign config.
func New(libraries LibraryLister, cfg config.RealignConfig) *Realigner {
	return &Realigner{libraries: libraries, cfg: cfg, readProv: scanner.ReadAudioProvenance}
}

// PlanLibrary computes the realign plan for every orphan under lib's root.
func (r *Realigner) PlanLibrary(lib models.Library) (Result, error) {
	resolvedRoot, ok := pathutil.ResolveWithinRoot(lib.Path, lib.Path)
	if !ok {
		return Result{}, fmt.Errorf("realign: library root %q is not accessible", lib.Path)
	}
	return r.plan(resolvedRoot, resolvedRoot, lib.ID)
}

// PlanDir computes the realign plan for orphans in a single directory under lib's
// root. The exact-tier candidate pool is that directory's audio, or the whole
// library's audio when cross_directory is enabled (so a scoped reactive pass still
// matches an orphan against an audio file that was moved elsewhere in the library).
func (r *Realigner) PlanDir(lib models.Library, dir string) (Result, error) {
	resolvedRoot, ok := pathutil.ResolveWithinRoot(lib.Path, lib.Path)
	if !ok {
		return Result{}, fmt.Errorf("realign: library root %q is not accessible", lib.Path)
	}
	resolvedDir, ok := pathutil.ResolveWithinRoot(resolvedRoot, dir)
	if !ok {
		return Result{}, fmt.Errorf("realign: directory %q is not within library root %q", dir, lib.Path)
	}
	return r.plan(resolvedDir, resolvedRoot, lib.ID)
}

// plan walks scopeRoot for orphans and classifies each. When cross_directory is
// set, the exact-match candidate pool is drawn from poolRoot (the library root);
// otherwise each orphan matches only within its own directory. When
// scopeRoot == poolRoot a single walk serves both.
func (r *Realigner) plan(scopeRoot, poolRoot string, libraryID int64) (Result, error) {
	dirs, scopeAudio, err := walk(scopeRoot)
	if err != nil {
		return Result{}, err
	}
	pool := scopeAudio
	if r.cfg.CrossDirectory && poolRoot != scopeRoot {
		_, poolAudio, perr := walk(poolRoot)
		if perr != nil {
			return Result{}, perr
		}
		pool = poolAudio
	}

	identityKeys := identity.NormalizeKeys(r.cfg.IdentityKeys)
	provCache := map[string]audioProvenance{}
	getProv := func(p string) audioProvenance {
		if v, ok := provCache[p]; ok {
			return v
		}
		isrc, mbid, artist, title, rerr := r.readProv(p)
		v := audioProvenance{isrc: isrc, mbid: mbid, artist: artist, title: title, err: rerr}
		provCache[p] = v
		return v
	}
	// claimed tracks target paths already spoken for by an earlier planned move in
	// this run. Two orphans carrying the same ISRC/MBID (duplicated tags) can each
	// resolve to the same audio file and target; without this both pass the
	// plan-time destinationBlocked check (nothing on disk yet) and the second
	// os.Rename would clobber the first. A second claim on a target is a conflict.
	claimed := map[string]bool{}

	dirPaths := make([]string, 0, len(dirs))
	for d := range dirs {
		dirPaths = append(dirPaths, d)
	}
	sort.Strings(dirPaths)

	var res Result
	for _, dir := range dirPaths {
		res.DirsChecked++
		de := dirs[dir]
		dirPool := de.audio
		if r.cfg.CrossDirectory {
			dirPool = pool
		}
		r.classifyDir(dir, de, dirPool, identityKeys, getProv, claimed, libraryID, &res)
	}
	return res, nil
}

// classifyDir classifies every orphan in one directory into the four tiers,
// appending moves/skips to res.
func (r *Realigner) classifyDir(dir string, de *dirEntry, pool []string, identityKeys identity.Keys, getProv func(string) audioProvenance, claimed map[string]bool, libraryID int64, res *Result) {
	audioStems := stemSet(de.audio)
	sidecarStems := stemSet(de.sidecars)
	orphans := make([]string, 0)
	for _, s := range de.sidecars {
		if !audioStems[stemOf(s)] {
			orphans = append(orphans, s)
		}
	}
	missingAudio := make([]string, 0)
	for _, a := range de.audio {
		if !sidecarStems[stemOf(a)] {
			missingAudio = append(missingAudio, a)
		}
	}
	sort.Strings(orphans)
	sort.Strings(missingAudio)
	dirPair := len(orphans) == 1 && len(missingAudio) == 1

	// deferredOrphans and deferredTags collect orphans that fell through to the
	// generic "cannot pair without provenance" case (not a 1:1 positional pair)
	// so the opt-in N:M name matcher gets a shot at them after the per-orphan
	// loop below, rather than reporting them ambiguous immediately.
	var deferredOrphans []string
	deferredTags := map[string]lyrics.ProvenanceTags{}

	for _, orphan := range orphans {
		res.OrphansSeen++
		orphanExt := filepath.Ext(orphan)
		orphanTags, terr := lyrics.ReadProvenanceTags(orphan)
		if terr != nil {
			slog.Warn("realign: failed to read sidecar header; treating as no provenance", "path", orphan, "error", terr)
			orphanTags = lyrics.ProvenanceTags{}
		}

		exactAudio, exactStatus := resolveExact(orphanTags, identityKeys, pool, getProv)
		switch exactStatus {
		case "conflict":
			res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: orphan, Reason: "multiple audio files share the sidecar's ISRC/MBID"})
			continue
		case "unique":
			target := destForAudio(exactAudio, orphanExt)
			if dirPair && filepath.Dir(exactAudio) == dir && exactAudio != missingAudio[0] {
				res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: orphan, Reason: "exact and heuristic candidates disagree"})
				continue
			}
			if destinationBlocked(target, orphan) {
				res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: orphan, Reason: "destination " + target + " already exists"})
				continue
			}
			if claimed[target] {
				res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: orphan, Reason: "destination " + target + " already claimed by another orphan this run (duplicate provenance?)"})
				continue
			}
			claimed[target] = true
			res.Moves = append(res.Moves, Move{Orphan: orphan, Target: target, Method: "exact", LibraryID: libraryID, Eligible: true})
		default: // "none": no provenance match
			if !dirPair {
				// Defer rather than immediately report ambiguous: when name_match
				// is enabled, the N:M matcher below gets a chance to pair this
				// orphan unambiguously against the directory's remaining
				// candidates. When name_match is off, deferredOrphans is drained
				// into the same generic ambiguous report after this loop, so
				// disabled behavior is byte-for-byte unchanged.
				deferredOrphans = append(deferredOrphans, orphan)
				deferredTags[orphan] = orphanTags
				continue
			}
			audio := missingAudio[0]
			target := destForAudio(audio, orphanExt)
			if destinationBlocked(target, orphan) {
				res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: orphan, Reason: "destination " + target + " already exists"})
				continue
			}
			if claimed[target] {
				res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: orphan, Reason: "destination " + target + " already claimed by another orphan this run (duplicate provenance?)"})
				continue
			}
			// The shared 1:1 resolver (#740). It owns the confidence floor, the
			// margin rule (#672) and the untagged-positional degradation, so prune
			// consults the SAME ladder rather than growing a second matcher.
			//
			// TARGETS vs RIVALS is the load-bearing distinction here: the only legal
			// destination is the lone sidecar-less gap, but the rival set is every
			// audio file in the directory INCLUDING ones that already have a
			// sidecar. An orphan that matches the already-paired track next door
			// just as well as it matches the gap carries a name signal that cannot
			// tell the tracks apart, and pairing on it attaches lyrics to the wrong
			// song. Passing only the target would leave nothing to be too close to,
			// so the margin rule would never fire.
			hres := identity.ResolveHeuristic(
				nameSignalForOrphan(orphanTags, stemOf(orphan)),
				[]identity.Candidate{candidateForAudio(audio, getProv)},
				candidatesForAudio(de.audio, getProv),
				r.cfg.MinConfidence, r.cfg.MinMargin,
			)
			switch hres.Verdict {
			case identity.VerdictNone:
				res.Skips = append(res.Skips, Skip{Kind: "ambiguous", Path: orphan, Reason: fmt.Sprintf("name similarity %.2f below min_confidence %.2f", hres.Score, r.cfg.MinConfidence)})
				continue
			case identity.VerdictConflict:
				res.Skips = append(res.Skips, Skip{Kind: "ambiguous", Path: orphan, Reason: fmt.Sprintf("name similarity %.2f too close to runner-up %.2f (margin %.2f < min_margin %.2f)", hres.Score, hres.RunnerUp, hres.Score-hres.RunnerUp, r.cfg.MinMargin)})
				continue
			case identity.VerdictUnique:
			}
			score := hres.Score
			claimed[target] = true
			mv := Move{Orphan: orphan, Target: target, Method: "heuristic", LibraryID: libraryID, Eligible: !r.cfg.RequireProvenance, Confidence: score}
			if !mv.Eligible {
				mv.GateReason = "require_provenance is set; heuristic matches are not applied"
			}
			res.Moves = append(res.Moves, mv)
		}
	}

	if len(deferredOrphans) == 0 {
		return
	}
	if !r.cfg.NameMatch {
		// name_match is off: fall back to the same generic ambiguous report the
		// pre-N:M code path emitted, unchanged.
		for _, orphan := range deferredOrphans {
			reason := fmt.Sprintf("%d orphan sidecar(s), %d audio file(s) missing a sidecar; cannot pair without provenance", len(orphans), len(missingAudio))
			res.Skips = append(res.Skips, Skip{Kind: "ambiguous", Path: orphan, Reason: reason})
		}
		return
	}

	// N:M name matching: score every deferred orphan against every remaining
	// (unclaimed-by-provenance) candidate audio file in the directory and
	// greedily accept only unambiguous pairings.
	pairings, unresolved := resolveNameMatch(deferredOrphans, deferredTags, missingAudio, getProv, r.cfg.MinConfidence, r.cfg.MinMargin)
	for _, p := range pairings {
		orphanExt := filepath.Ext(p.Orphan)
		target := destForAudio(p.Audio, orphanExt)
		if destinationBlocked(target, p.Orphan) {
			res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: p.Orphan, Reason: "destination " + target + " already exists"})
			continue
		}
		if claimed[target] {
			res.Skips = append(res.Skips, Skip{Kind: "conflict", Path: p.Orphan, Reason: "destination " + target + " already claimed by another orphan this run (duplicate provenance?)"})
			continue
		}
		claimed[target] = true
		mv := Move{Orphan: p.Orphan, Target: target, Method: "heuristic-nm", LibraryID: libraryID, Eligible: !r.cfg.RequireProvenance, Confidence: p.Score}
		if !mv.Eligible {
			mv.GateReason = "require_provenance is set; heuristic matches are not applied"
		}
		res.Moves = append(res.Moves, mv)
	}
	for _, u := range unresolved {
		res.Skips = append(res.Skips, Skip{Kind: "ambiguous", Path: u.Orphan, Reason: u.Reason})
	}
}

// Apply performs the eligible moves in order, backup-first and clobber-safe, and
// returns the per-move outcome so a caller can render or log it. A move is applied
// only when it is Eligible AND (exact tier OR policy.AllowHeuristic); others are
// returned with GatedSkipped set and no filesystem change. The backup file is
// opened lazily on the first actual move.
func (r *Realigner) Apply(moves []Move, backupPath string, policy Policy) (applied []Applied, retErr error) {
	var backup *os.File
	defer func() {
		if backup != nil {
			if cerr := backup.Close(); cerr != nil && retErr == nil {
				retErr = cerr
			}
		}
	}()

	for _, mv := range moves {
		if !mv.Eligible || (strings.HasPrefix(mv.Method, "heuristic") && !policy.AllowHeuristic) {
			applied = append(applied, Applied{Move: mv, GatedSkipped: true})
			continue
		}
		if backup == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
			if ferr != nil {
				return applied, fmt.Errorf("realign: open backup %q: %w", backupPath, ferr)
			}
			backup = f
		}
		// Backup first (skip this move if it fails), then a clobber-safe atomic
		// rename, then fsync the destination dir. The just-written backup line is
		// rolled back on any post-write failure -- but only when we captured a
		// valid pre-write offset. If Stat failed we skip the truncation rather
		// than zero the whole file (Truncate(0) would delete prior backup history).
		var backupOffset int64
		haveOffset := false
		if fi, serr := backup.Stat(); serr == nil {
			backupOffset = fi.Size()
			haveOffset = true
		}
		if berr := appendBackup(backup, mv); berr != nil {
			applied = append(applied, Applied{Move: mv, Err: fmt.Errorf("backup write failed: %w", berr)})
			continue
		}
		rollbackBackup := func(cause string, err error) {
			if !haveOffset {
				slog.Warn("realign: backup offset unknown; leaving possibly un-applied record in backup rather than truncating", "path", backupPath, "cause", cause, "error", err)
				return
			}
			if terr := backup.Truncate(backupOffset); terr != nil {
				slog.Warn("realign: failed to roll back backup line", "path", backupPath, "cause", cause, "error", terr)
				return
			}
			_ = backup.Sync() //nolint:errcheck // best-effort durability of the rollback truncation
		}
		// Re-check the destination immediately before the rename so Apply stays
		// clobber-safe even when moves from independently planned libraries are
		// merged into one slice -- the plan-time claimed map is per-plan, not
		// run-wide, and os.Rename would otherwise overwrite an existing sidecar
		// on POSIX.
		if mv.Kind != KindRename {
			if aerr := applyRemediation(mv); aerr != nil {
				rollbackBackup(mv.Kind+" failed", aerr)
				applied = append(applied, Applied{Move: mv, Err: aerr})
				continue
			}
			applied = append(applied, Applied{Move: mv})
			continue
		}
		if destinationBlocked(mv.Target, mv.Orphan) {
			rollbackBackup("destination blocked", nil)
			applied = append(applied, Applied{Move: mv, Err: fmt.Errorf("destination exists: %s", mv.Target)})
			continue
		}
		if rerr := os.Rename(mv.Orphan, mv.Target); rerr != nil {
			rollbackBackup("rename failed", rerr)
			applied = append(applied, Applied{Move: mv, Err: fmt.Errorf("rename: %w", rerr)})
			continue
		}
		lyrics.FsyncDir(filepath.Dir(mv.Target))
		applied = append(applied, Applied{Move: mv})
	}
	return applied, nil
}

// ReactiveDir plans and applies realign for a single directory under lib, using
// the conservative reactive apply policy (exact tier always; heuristic tier only
// when AutoApplyHeuristic is set). It is the entry point for serve-mode triggers.
func (r *Realigner) ReactiveDir(lib models.Library, dir, backupPath string) (Result, []Applied, error) {
	res, err := r.PlanDir(lib, dir)
	if err != nil {
		return Result{}, nil, err
	}
	if len(res.Moves) == 0 {
		return res, nil, nil
	}
	applied, aerr := r.Apply(res.Moves, backupPath, Policy{AllowHeuristic: r.cfg.AutoApplyHeuristic})
	return res, applied, aerr
}

// ResolveAndRealignDir resolves the library that owns dir, then plans and applies
// realign for dir. Used by the Lidarr webhook, which passes confined directories
// (an old audio file may already be deleted, but its directory -- where the
// sidecar strands -- still exists). When no configured library owns dir it is a
// no-op.
func (r *Realigner) ResolveAndRealignDir(ctx context.Context, dir, backupPath string) (Result, []Applied, error) {
	lib, ok, err := r.ownerLibrary(ctx, dir)
	if err != nil {
		return Result{}, nil, err
	}
	if !ok {
		return Result{}, nil, nil
	}
	return r.ReactiveDir(lib, dir, backupPath)
}

// ownerLibrary returns the most-specific configured library whose root contains
// path, or ok=false when none does.
func (r *Realigner) ownerLibrary(ctx context.Context, path string) (models.Library, bool, error) {
	libs, err := r.libraries.List(ctx)
	if err != nil {
		return models.Library{}, false, fmt.Errorf("realign: list libraries: %w", err)
	}
	var best models.Library
	found := false
	for _, lib := range libs {
		if pathutil.WithinRoot(lib.Path, path) && (!found || len(lib.Path) > len(best.Path)) {
			best = lib
			found = true
		}
	}
	return best, found, nil
}

// CountApplied tallies an Apply outcome slice into moved / skipped / errored.
func CountApplied(applied []Applied) (moved, skipped, errored int) {
	for _, a := range applied {
		switch {
		case a.GatedSkipped:
			skipped++
		case a.Err != nil:
			errored++
		default:
			moved++
		}
	}
	return moved, skipped, errored
}

// dirEntry holds the audio files and sidecars found in one directory.
type dirEntry struct {
	audio    []string
	sidecars []string
}

// audioProvenance caches the identity signals read from one audio file.
type audioProvenance struct {
	isrc, mbid, artist, title string
	err                       error
}

// backupRecord is one JSONL line capturing an applied move so the operation is
// restorable (swap OldPath/NewPath to undo). Method records the resolver tier.
//
// Kind and TextPath extend the same record to the remediation actions rather
// than adding a second format: undoing a demote means moving NewPath back to
// OldPath and deleting TextPath, which needs both paths in one line. Kind is
// omitted for a plain realign rename, so records written before remediation
// existed parse identically. A KindPurge record documents an intentionally
// non-reversible delete: it is an audit trail, not a restorable copy.
type backupRecord struct {
	OldPath   string `json:"old_path"`
	NewPath   string `json:"new_path"`
	LibraryID int64  `json:"library_id"`
	Method    string `json:"method"`
	Kind      string `json:"kind,omitempty"`
	TextPath  string `json:"text_path,omitempty"`
}

// walk walks root and partitions every regular file into audio files and .lrc/.txt
// sidecars, grouped by directory, plus the flat list of all audio paths for
// cross-directory matching. Every path stays under root by construction.
func walk(root string) (map[string]*dirEntry, []string, error) {
	dirs := map[string]*dirEntry{}
	var allAudio []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("realign: skipping unreadable path", "path", p, "error", err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		dir := filepath.Dir(p)
		entry := dirs[dir]
		if entry == nil {
			entry = &dirEntry{}
			dirs[dir] = entry
		}
		switch {
		case scanner.IsAudioFile(p):
			entry.audio = append(entry.audio, p)
			allAudio = append(allAudio, p)
		case isSidecar(p):
			entry.sidecars = append(entry.sidecars, p)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("realign: walk %s: %w", root, err)
	}
	return dirs, allAudio, nil
}

// resolveExact is a thin adapter over the shared identity exact tier: it hands
// the shared resolver a LAZY sequence of identity.Candidate values over
// realign's pool, each realized by getProv (an ID3 tag READ off disk, since
// realign has no persisted identity store of its own). Delegating the verdict
// keeps sidecar re-attachment from ever disagreeing with prune's row
// re-linking about where a file went.
//
// The sequence -- not a materialized slice -- is what preserves the
// pre-extraction access pattern: identity.ResolveExactSeq pulls candidates
// only from inside its per-key loop and only for a key the orphan actually
// carries a value for, so an orphan with no [isrc:]/[mbid:] header reads ZERO
// audio files here (it used to read the entire pool). getProv memoizes per
// plan() run, so the at-most-once-per-key re-iteration costs no extra reads.
//
// Returns ("", "none") on no match, (path, "unique") on a single match, and
// ("", "conflict") when more than one audio shares the same id.
func resolveExact(tags lyrics.ProvenanceTags, identityKeys identity.Keys, pool []string, getProv func(string) audioProvenance) (string, string) {
	candidates := func(yield func(identity.Candidate) bool) {
		for _, a := range pool {
			pv := getProv(a)
			if pv.err != nil {
				// Unreadable audio cannot be matched on identity it never
				// yielded; skip it rather than let a zero-valued provenance
				// masquerade as an empty-but-present identity.
				continue
			}
			if !yield(identity.Candidate{Ref: a, MBID: pv.mbid, ISRC: pv.isrc, Artist: pv.artist, Title: pv.title}) {
				return
			}
		}
	}
	verdict, ref := identity.ResolveExactSeq(tags.MBID, tags.ISRC, identityKeys, candidates)
	switch verdict {
	case identity.VerdictUnique:
		return ref, "unique"
	case identity.VerdictConflict:
		return "", "conflict"
	default:
		return "", "none"
	}
}

// nameSignalForOrphan describes an orphan sidecar's name evidence for the
// shared scorer: its [ar:]/[ti:] header plus its filesystem stem as the
// fallback. Building the signal here (rather than pre-flattening it to a
// string) is what keeps the 1:1 and N:M tiers scoring a pair IDENTICALLY: both
// hand the same NameSignal pair to identity.NameScore, which owns the whole
// decision about which fields discriminate.
func nameSignalForOrphan(tags lyrics.ProvenanceTags, stem string) identity.NameSignal {
	return identity.NameSignal{Artist: tags.Artist, Title: tags.Title, Stem: stem}
}

// nameSignalForAudio describes a candidate audio file's name evidence for the
// shared scorer: its embedded artist/title plus its filesystem stem.
func nameSignalForAudio(prov audioProvenance, stem string) identity.NameSignal {
	return identity.NameSignal{Artist: prov.artist, Title: prov.title, Stem: stem}
}

// candidateForAudio describes one audio file to the shared resolver (#740).
//
// Ref is the PATH, which is what the caller round-trips back out of a Unique
// verdict; identity.NameSignal's Stem fallback is derived from it inside the
// resolver, so the stem is not duplicated here.
func candidateForAudio(path string, getProv func(string) audioProvenance) identity.Candidate {
	prov := getProv(path)
	// Stem, not Ref, carries the filename evidence: Ref is the full path the
	// caller round-trips back out of a Unique verdict, and scoring a path against
	// a stem depresses every similarity score.
	return identity.Candidate{Ref: path, Artist: prov.artist, Title: prov.title, Stem: stemOf(path)}
}

// candidatesForAudio maps a directory's audio list into the resolver's rival
// set. Every file is included, INCLUDING ones that already carry a sidecar: a
// rival is something the name must distinguish the orphan from, not something
// the orphan may be attached to (#672).
func candidatesForAudio(paths []string, getProv func(string) audioProvenance) []identity.Candidate {
	out := make([]identity.Candidate, 0, len(paths))
	for _, p := range paths {
		out = append(out, candidateForAudio(p, getProv))
	}
	return out
}

// nmPairing is one accepted orphan->audio pairing from resolveNameMatch.
type nmPairing struct {
	Orphan string
	Audio  string
	Score  float64
}

// nmUnresolved records why an orphan was not paired by resolveNameMatch.
type nmUnresolved struct {
	Orphan string
	Reason string
}

// resolveNameMatch is the N:M name-similarity tier: it scores every
// orphan x candidate pair via normalize.MatchConfidence and greedily accepts
// pairings in descending score order, skipping any pairing that would be a
// guess.
//
// Acceptance requires ALL of:
//   - the score clears minConf (the same floor the single-candidate heuristic
//     tier uses);
//   - the candidate is not already claimed by a higher-scoring pairing;
//   - the pairing is the orphan's unambiguous best: its score exceeds its
//     runner-up over the FULL ORIGINAL candidate set by at least minMargin.
//
// Margin semantics when a candidate is already claimed: the runner-up is
// computed against every candidate in the original matrix, INCLUDING ones
// already claimed by a higher-scoring orphan. Claiming is a statement about
// AVAILABILITY; the margin rule measures DISCRIMINABILITY -- whether the name
// signal can tell the candidates apart at all. Another orphan taking C1 does
// not make this orphan's signal any sharper, so excluding C1 from the scan
// would convert a genuine near-tie into a confident pairing and destroy the
// evidence that the decision was a coin flip. Non-displacement is not
// non-ambiguity. Greedy accept in descending score order still guarantees no
// pairing displaces a stronger one; the margin rule is the separate guarantee
// that an accepted pairing was distinguishable in the first place.
//
// Single-candidate matrices (runnerUp stays -1 because the directory offered
// exactly one candidate) pair on minConf alone. There is no alternative for
// the name signal to be confused with, so there is nothing for a margin to
// measure -- the same reasoning under which the strict 1:1 heuristic tier
// guards on minConf only. Any orphan-side contest for that lone candidate
// (several orphans clearing minConf against it) is resolved by the claim guard
// plus descending order, exactly as it is for a contested candidate in a wider
// matrix; the margin rule was never the mechanism for that case.
//
// Returns the accepted pairings and, for every orphan that did not resolve, a
// reason suitable for a Skip.
func resolveNameMatch(orphans []string, orphanTags map[string]lyrics.ProvenanceTags, candidates []string, getProv func(string) audioProvenance, minConf, minMargin float64) ([]nmPairing, []nmUnresolved) {
	type score struct {
		orphan, audio string
		s             float64
	}
	var scores []score
	for _, o := range orphans {
		oSig := nameSignalForOrphan(orphanTags[o], stemOf(o))
		for _, a := range candidates {
			s, _ := identity.NameScore(oSig, nameSignalForAudio(getProv(a), stemOf(a)))
			scores = append(scores, score{orphan: o, audio: a, s: s})
		}
	}
	// Sort all pairs by descending score so the orphan/candidate combination
	// with the strongest signal in the whole matrix is always considered --
	// and, if accepted, claims its candidate -- before any weaker pairing that
	// might contest it. The ordering must be TOTAL: this decides which orphan
	// wins a contested candidate, and that decision moves files on disk. Exact
	// ties (identical tags on two sidecars are common) would otherwise resolve
	// arbitrarily and differently from run to run, so ties break on orphan then
	// audio path, both of which are unique within the matrix.
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].s != scores[j].s {
			return scores[i].s > scores[j].s
		}
		if scores[i].orphan != scores[j].orphan {
			return scores[i].orphan < scores[j].orphan
		}
		return scores[i].audio < scores[j].audio
	})

	claimedAudio := map[string]bool{}
	resolved := map[string]bool{}
	var pairings []nmPairing
	unresolvedReason := map[string]string{}

	for _, top := range scores {
		if resolved[top.orphan] {
			continue
		}
		if claimedAudio[top.audio] {
			// This orphan cleared the confidence floor against this candidate
			// but lost it to a stronger pairing. Record that specifically: the
			// fallthrough reason ("no candidate cleared min_confidence") would
			// be false here, and an operator reading it would lower a floor
			// that was never the obstacle.
			if _, has := unresolvedReason[top.orphan]; !has && top.s >= minConf {
				unresolvedReason[top.orphan] = fmt.Sprintf("best candidate (name similarity %.2f) already claimed by a stronger orphan match", top.s)
			}
			continue
		}
		if top.s < minConf {
			if _, has := unresolvedReason[top.orphan]; !has {
				unresolvedReason[top.orphan] = fmt.Sprintf("best name similarity %.2f below min_confidence %.2f", top.s, minConf)
			}
			continue
		}
		// Find this orphan's runner-up over the FULL original candidate set
		// (top.audio itself excluded). Claimed candidates are deliberately
		// still counted -- see the margin-semantics note on the doc comment.
		runnerUp := -1.0
		for _, s := range scores {
			if s.orphan != top.orphan || s.audio == top.audio {
				continue
			}
			if s.s > runnerUp {
				runnerUp = s.s
			}
		}
		// runnerUp < 0 means the matrix held exactly one candidate, so there is
		// no alternative the name signal could be confused with and nothing for
		// a margin to measure; minConf alone gates the pairing.
		if runnerUp >= 0 && top.s-runnerUp < minMargin {
			if _, has := unresolvedReason[top.orphan]; !has {
				unresolvedReason[top.orphan] = fmt.Sprintf("name similarity %.2f too close to runner-up %.2f (margin %.2f < min_margin %.2f)", top.s, runnerUp, top.s-runnerUp, minMargin)
			}
			continue
		}
		pairings = append(pairings, nmPairing{Orphan: top.orphan, Audio: top.audio, Score: top.s})
		resolved[top.orphan] = true
		claimedAudio[top.audio] = true
		delete(unresolvedReason, top.orphan)
	}

	var unresolved []nmUnresolved
	for _, o := range orphans {
		if resolved[o] {
			continue
		}
		reason, has := unresolvedReason[o]
		if !has {
			reason = "no candidate cleared min_confidence"
		}
		unresolved = append(unresolved, nmUnresolved{Orphan: o, Reason: reason})
	}
	sort.Slice(unresolved, func(i, j int) bool { return unresolved[i].Orphan < unresolved[j].Orphan })
	return pairings, unresolved
}

// applyRemediation performs a non-rename action. Every kind here is ORDERED so
// nothing is ever destroyed before its replacement exists:
//
//   - KindDemote writes TextBody to TextPath FIRST and only then moves the .lrc
//     aside. A failed .txt write leaves the .lrc exactly where it was, so the
//     operator loses nothing; the reverse order could lose the lyric entirely.
//     An existing sidecar at TextPath is never overwritten -- settled content on
//     disk outranks anything this pass would write, matching the accept-time
//     guard's settled-sidecar rule.
//   - KindQuarantine moves the sidecar to Target, which the caller places under
//     a quarantine root. Reversible: move it back.
//   - KindPurge unlinks. Not reversible, opt-in only.
//
// Symlinks are never followed or moved: a sidecar path that is a symlink is
// refused outright rather than acted on, so a link planted in a library root
// cannot redirect a delete or a rename outside it.
func applyRemediation(mv Move) error {
	if fi, err := os.Lstat(mv.Orphan); err != nil {
		return fmt.Errorf("%s: lstat %q: %w", mv.Kind, mv.Orphan, err)
	} else if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: refusing to act on symlink %q", mv.Kind, mv.Orphan)
	}

	switch mv.Kind {
	case KindPurge:
		// A purge MAY carry words to keep: that is "demote, then unlink" -- write
		// the .txt beside the audio, then delete the .lrc rather than moving it
		// aside. Written BEFORE the unlink, and a write failure aborts, so the
		// words can never be lost by deleting the only copy of them first.
		if mv.TextPath != "" {
			created, werr := writeDemotedText(mv.TextPath, mv.TextBody)
			if werr != nil {
				return werr
			}
			if err := os.Remove(mv.Orphan); err != nil {
				// Roll the .txt back for the same reason the demote arm does: a
				// failed action must leave nothing on disk the backup record does
				// not describe. Only when THIS call created it.
				if created {
					if rerr := os.Remove(mv.TextPath); rerr != nil && !os.IsNotExist(rerr) {
						slog.Warn("purge rollback: could not remove the text file",
							"path", mv.TextPath, "error", rerr)
					} else {
						lyrics.FsyncDir(filepath.Dir(mv.TextPath))
					}
				}
				return fmt.Errorf("purge %q: %w", mv.Orphan, err)
			}
			lyrics.FsyncDir(filepath.Dir(mv.TextPath))
			lyrics.FsyncDir(filepath.Dir(mv.Orphan))
			return nil
		}
		if err := os.Remove(mv.Orphan); err != nil {
			return fmt.Errorf("purge %q: %w", mv.Orphan, err)
		}
		lyrics.FsyncDir(filepath.Dir(mv.Orphan))
		return nil
	case KindDemote:
		if mv.TextPath == "" {
			return fmt.Errorf("demote %q: no text path", mv.Orphan)
		}
		created, err := writeDemotedText(mv.TextPath, mv.TextBody)
		if err != nil {
			return err
		}
		if merr := moveAside(mv); merr != nil {
			// Roll the .txt back so a failed demote leaves nothing on disk that the
			// backup record does not describe. The caller truncates its backup line
			// on error, so without this the file would survive unrecorded -- which
			// is precisely the backup-first invariant this package claims.
			//
			// Only when THIS call created it: writeDemotedText is O_EXCL and reports
			// an already-settled sidecar as a no-op, and removing a file we did not
			// write would destroy content that predates this run.
			if created {
				if rerr := os.Remove(mv.TextPath); rerr != nil && !os.IsNotExist(rerr) {
					slog.Warn("demote rollback: could not remove the text file",
						"path", mv.TextPath, "error", rerr)
				} else {
					lyrics.FsyncDir(filepath.Dir(mv.TextPath))
				}
			}
			return merr
		}
		return nil
	case KindQuarantine:
		return moveAside(mv)
	default:
		return fmt.Errorf("unknown remediation kind %q", mv.Kind)
	}
}

// writeDemotedText writes body to path unless a sidecar is already settled
// there, in which case it is a no-op and the existing file wins.
//
// The create is EXCLUSIVE (O_CREATE|O_EXCL) rather than a stat followed by a
// write. That is not just TOCTOU hygiene: a stat-first version has to decide
// what a stat error that is NOT not-exist means, and the conservative reading
// ("assume occupied, skip the write") is catastrophic here -- the caller then
// moves the .lrc aside believing the words were preserved, and they are gone.
// O_EXCL collapses the question: EEXIST is the settled case and every other
// error is a genuine failure the caller must not proceed past.
//
// The bool reports whether THIS call created the file, which the caller needs to
// roll the write back safely: on the settled (EEXIST) path the content predates
// this run, so removing it would destroy a file we did not write.
func writeDemotedText(path, body string) (created bool, err error) {
	if body == "" {
		return false, fmt.Errorf("demote: refusing to write an empty %q", path)
	}
	// 0o666 (subject to umask) matches what LRCWriter gives a final sidecar, so a
	// demoted .txt stays readable to whatever uid reads the library. The other
	// sidecar writers either use this mode or copy the original file's; 0o600 here
	// would make this the one path that produces an unreadable sidecar.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666) //nolint:gosec // reason: G304: path is derived from the caller's own audio-file enumeration; G302: mode intentionally matches the other sidecar writers so the file is readable by other uids
	if err != nil {
		if os.IsExist(err) {
			return false, nil // settled content on disk wins
		}
		return false, fmt.Errorf("demote: create %q: %w", path, err)
	}
	if _, werr := f.WriteString(body); werr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf("demote: write %q: %w", path, werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf("demote: sync %q: %w", path, serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("demote: close %q: %w", path, cerr)
	}
	lyrics.FsyncDir(filepath.Dir(path))
	return true, nil
}

// moveAside renames the sidecar to mv.Target, creating the destination
// directory tree and refusing to clobber anything already there.
func moveAside(mv Move) error {
	if mv.Target == "" {
		return fmt.Errorf("%s %q: no target", mv.Kind, mv.Orphan)
	}
	if err := os.MkdirAll(filepath.Dir(mv.Target), 0o750); err != nil {
		return fmt.Errorf("%s: mkdir %q: %w", mv.Kind, filepath.Dir(mv.Target), err)
	}
	if destinationBlocked(mv.Target, mv.Orphan) {
		return fmt.Errorf("%s: destination exists: %s", mv.Kind, mv.Target)
	}
	if err := os.Rename(mv.Orphan, mv.Target); err != nil {
		return fmt.Errorf("%s: rename %q: %w", mv.Kind, mv.Orphan, err)
	}
	lyrics.FsyncDir(filepath.Dir(mv.Target))
	lyrics.FsyncDir(filepath.Dir(mv.Orphan))
	return nil
}

// appendBackup writes and fsyncs one JSONL backup record for an applied move, so
// the backup-first guarantee survives a crash between the record and the rename.
func appendBackup(f *os.File, mv Move) error {
	rec := backupRecord{OldPath: mv.Orphan, NewPath: mv.Target, LibraryID: mv.LibraryID, Method: mv.Method, Kind: mv.Kind, TextPath: mv.TextPath}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal realign backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write realign backup record: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync realign backup record: %w", err)
	}
	return nil
}

// destForAudio returns the sidecar path an orphan should occupy next to audio,
// keeping the orphan's original extension (never converting .lrc<->.txt).
func destForAudio(audioPath, orphanExt string) string {
	return filepath.Join(filepath.Dir(audioPath), stemOf(audioPath)+orphanExt)
}

// destinationBlocked reports whether target already exists on disk and is not the
// orphan itself, so realign never overwrites an existing sidecar.
func destinationBlocked(target, orphan string) bool {
	if target == orphan {
		return false
	}
	_, err := os.Lstat(target)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	slog.Warn("realign: cannot stat destination; treating as blocked", "target", target, "error", err)
	return true
}

// isSidecar reports whether name is a .lrc or .txt lyric sidecar.
func isSidecar(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".lrc", ".txt":
		return true
	default:
		return false
	}
}

// stemOf returns the base name of path without its extension.
func stemOf(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func stemSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[stemOf(p)] = true
	}
	return set
}

// NormalizeIdentityKeys lowercases, filters to the known identity keys (mbid,
// isrc), and de-duplicates while preserving order. Exported so the CLI can render
// the effective key list in its header. Delegates to the shared
// identity.NormalizeKeys so realign and prune read config.RealignConfig the
// same way; kept as a []string-returning wrapper so the existing CLI call site
// (internal/commands/realign.go) needs no signature change.
func NormalizeIdentityKeys(keys []string) []string {
	return []string(identity.NormalizeKeys(keys))
}
