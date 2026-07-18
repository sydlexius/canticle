// Package realign re-attaches orphaned lyric sidecars (.lrc/.txt left behind when
// an audio file was renamed) to their audio via a four-tier confidence resolver:
// exact (provenance ISRC/MBID match), heuristic (single-candidate filesystem
// pairing gated by a name-similarity guard), ambiguous (multiple/zero candidates,
// reported and skipped), and conflict (contradictory signals or an existing
// destination, reported and skipped).
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
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
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

// Move is a planned sidecar rename with its resolved tier. Eligible is false when
// config gating (require_provenance) reports but suppresses the move.
type Move struct {
	Orphan     string
	Target     string
	Method     string // "exact" or "heuristic"
	LibraryID  int64
	Eligible   bool
	GateReason string  // why an ineligible move is suppressed (require_provenance)
	Confidence float64 // heuristic name-guard score (0 for exact / positional)
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

	identityKeys := NormalizeIdentityKeys(r.cfg.IdentityKeys)
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
func (r *Realigner) classifyDir(dir string, de *dirEntry, pool, identityKeys []string, getProv func(string) audioProvenance, claimed map[string]bool, libraryID int64, res *Result) {
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
				reason := fmt.Sprintf("%d orphan sidecar(s), %d audio file(s) missing a sidecar; cannot pair without provenance", len(orphans), len(missingAudio))
				res.Skips = append(res.Skips, Skip{Kind: "ambiguous", Path: orphan, Reason: reason})
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
			ok, score := heuristicNameGuard(orphanTags, stemOf(orphan), getProv(audio), stemOf(audio), r.cfg.MinConfidence)
			if !ok {
				res.Skips = append(res.Skips, Skip{Kind: "ambiguous", Path: orphan, Reason: fmt.Sprintf("name similarity %.2f below min_confidence %.2f", score, r.cfg.MinConfidence)})
				continue
			}
			claimed[target] = true
			mv := Move{Orphan: orphan, Target: target, Method: "heuristic", LibraryID: libraryID, Eligible: !r.cfg.RequireProvenance, Confidence: score}
			if !mv.Eligible {
				mv.GateReason = "require_provenance is set; heuristic matches are not applied"
			}
			res.Moves = append(res.Moves, mv)
		}
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
		if !mv.Eligible || (mv.Method == "heuristic" && !policy.AllowHeuristic) {
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
type backupRecord struct {
	OldPath   string `json:"old_path"`
	NewPath   string `json:"new_path"`
	LibraryID int64  `json:"library_id"`
	Method    string `json:"method"`
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

// resolveExact finds the unique audio file in pool whose embedded ISRC or MBID
// matches the orphan's, honoring identityKeys order (most authoritative first).
// Returns ("", "none") on no match, (path, "unique") on a single match, and
// ("", "conflict") when more than one audio shares the same id.
func resolveExact(tags lyrics.ProvenanceTags, identityKeys, pool []string, getProv func(string) audioProvenance) (string, string) {
	for _, key := range identityKeys {
		id := strings.TrimSpace(orphanKeyValue(tags, key))
		if id == "" {
			continue
		}
		var matches []string
		for _, a := range pool {
			pv := getProv(a)
			if pv.err != nil {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(audioKeyValue(pv, key)), id) {
				matches = append(matches, a)
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			return matches[0], "unique"
		default:
			return "", "conflict"
		}
	}
	return "", "none"
}

// heuristicNameGuard implements the min_confidence name guard for the heuristic
// tier, comparing the orphan's [ar:]/[ti:] header (or sidecar stem) against the
// candidate audio's artist/title via Jaro-Winkler. Degrades to positional matching
// (returns true) when neither side yields a name, so a plain .txt still realigns.
func heuristicNameGuard(tags lyrics.ProvenanceTags, orphanStem string, audio audioProvenance, audioStem string, minConf float64) (bool, float64) {
	hasOrphanName := tags.Artist != "" || tags.Title != ""
	hasAudioName := audio.artist != "" || audio.title != ""
	if !hasOrphanName && !hasAudioName {
		return true, 0
	}
	orphanStr := orphanStem
	if hasOrphanName {
		orphanStr = strings.TrimSpace(tags.Artist + " " + tags.Title)
	}
	audioStr := audioStem
	if hasAudioName {
		audioStr = strings.TrimSpace(audio.artist + " " + audio.title)
	}
	score := normalize.MatchConfidence(orphanStr, audioStr)
	return score >= minConf, score
}

// appendBackup writes and fsyncs one JSONL backup record for an applied move, so
// the backup-first guarantee survives a crash between the record and the rename.
func appendBackup(f *os.File, mv Move) error {
	rec := backupRecord{OldPath: mv.Orphan, NewPath: mv.Target, LibraryID: mv.LibraryID, Method: mv.Method}
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
// the effective key list in its header.
func NormalizeIdentityKeys(keys []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "mbid" && k != "isrc" {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

func orphanKeyValue(tags lyrics.ProvenanceTags, key string) string {
	switch key {
	case "mbid":
		return tags.MBID
	case "isrc":
		return tags.ISRC
	}
	return ""
}

func audioKeyValue(pv audioProvenance, key string) string {
	switch key {
	case "mbid":
		return pv.mbid
	case "isrc":
		return pv.isrc
	}
	return ""
}
