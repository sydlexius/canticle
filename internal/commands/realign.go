package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/doxazo-net/canticle/internal/config"
	"github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/library"
	"github.com/doxazo-net/canticle/internal/lyrics"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/normalize"
	"github.com/doxazo-net/canticle/internal/pathutil"
	"github.com/doxazo-net/canticle/internal/scanner"
)

// RealignCmd re-attaches orphaned lyric sidecars (.lrc/.txt left behind when an
// audio file was renamed) to their audio via a four-tier confidence resolver:
// exact (provenance ISRC/MBID match), heuristic (single-candidate filesystem
// pairing gated by a name-similarity guard), ambiguous (multiple/zero candidates,
// reported and skipped), and conflict (contradictory signals or an existing
// destination, reported and skipped). Dry-run unless --yes. It only ever changes
// a sidecar's stem, never its extension, so a synced .lrc or an instrumental .txt
// marker keeps its type.
type RealignCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id)" default:""`
	Yes        bool   `arg:"--yes" help:"actually rename sidecars (without it, prints what would change)"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of applied moves (default: <db-dir>/realign-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// realignBackupRecord is one JSONL line capturing an applied move so the
// operation is restorable (swap OldPath/NewPath to undo). Method records the
// resolver tier that justified the move.
type realignBackupRecord struct {
	OldPath   string `json:"old_path"`
	NewPath   string `json:"new_path"`
	LibraryID int64  `json:"library_id"`
	Method    string `json:"method"`
}

// realignMove is a planned sidecar rename with its resolved tier. Eligible is
// false when config gating (require_provenance) reports but suppresses the move.
type realignMove struct {
	orphan     string
	target     string
	method     string // "exact" or "heuristic"
	libraryID  int64
	eligible   bool
	gateReason string  // why an ineligible move is suppressed (require_provenance)
	confidence float64 // heuristic name-guard score (0 for exact / positional)
}

// realignSkip is a reported directory/orphan that was not moved (ambiguous or
// conflict), never guessed.
type realignSkip struct {
	kind   string // "ambiguous" or "conflict"
	path   string // orphan sidecar or directory
	reason string
}

// audioProvenance caches the identity signals read from one audio file.
type audioProvenance struct {
	isrc   string
	mbid   string
	artist string
	title  string
	err    error
}

// runRealign is the realign command handler. It walks each resolved library root,
// classifies every orphaned sidecar into a confidence tier, and (under --yes)
// applies eligible moves clobber-safely with a method-tagged JSONL backup.
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
	identityKeys := normalizeIdentityKeys(rc.IdentityKeys)
	minConf := rc.MinConfidence

	suffix := ""
	if !args.Yes {
		suffix = " [dry run; pass --yes to apply]"
	}
	_, _ = fmt.Fprintf(out, "realign: %d librar%s; require_provenance=%t cross_directory=%t identity_keys=[%s] min_confidence=%g%s\n",
		len(libs), plural(len(libs), "y", "ies"), rc.RequireProvenance, rc.CrossDirectory, strings.Join(identityKeys, ","), minConf, suffix)

	var (
		moves []realignMove
		skips []realignSkip
	)
	dirsChecked := 0
	orphansSeen := 0

	for _, lib := range libs {
		root := lib.Path
		resolvedRoot, ok := pathutil.ResolveWithinRoot(root, root)
		if !ok {
			// Root missing or unresolvable: report and skip rather than fail the run.
			_, _ = fmt.Fprintf(out, "skip library %q (id=%d): root %q is not accessible\n", lib.Name, lib.ID, root)
			continue
		}
		dirs, allAudio, werr := walkRealign(resolvedRoot)
		if werr != nil {
			slog.Warn("realign: walk failed; skipping library", "library", lib.Name, "root", resolvedRoot, "error", werr)
			continue
		}
		provCache := map[string]audioProvenance{}
		getProv := func(p string) audioProvenance {
			if v, ok := provCache[p]; ok {
				return v
			}
			isrc, mbid, artist, title, rerr := scanner.ReadAudioProvenance(p)
			v := audioProvenance{isrc: isrc, mbid: mbid, artist: artist, title: title, err: rerr}
			provCache[p] = v
			return v
		}

		dirPaths := make([]string, 0, len(dirs))
		for d := range dirs {
			dirPaths = append(dirPaths, d)
		}
		sort.Strings(dirPaths)

		for _, dir := range dirPaths {
			dirsChecked++
			de := dirs[dir]
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

			// Exact-tier candidate pool: the orphan's own directory, or the whole
			// library when cross_directory is enabled.
			pool := de.audio
			if rc.CrossDirectory {
				pool = allAudio
			}

			for _, orphan := range orphans {
				orphansSeen++
				orphanExt := filepath.Ext(orphan)
				orphanTags, terr := lyrics.ReadProvenanceTags(orphan)
				if terr != nil {
					slog.Warn("realign: failed to read sidecar header; treating as no provenance", "path", orphan, "error", terr)
					orphanTags = lyrics.ProvenanceTags{}
				}

				exactAudio, exactStatus := resolveRealignExact(orphanTags, identityKeys, pool, getProv)
				switch exactStatus {
				case "conflict":
					skips = append(skips, realignSkip{kind: "conflict", path: orphan, reason: "multiple audio files share the sidecar's ISRC/MBID"})
					continue
				case "unique":
					target := destForAudio(exactAudio, orphanExt)
					if dirPair && filepath.Dir(exactAudio) == dir && exactAudio != missingAudio[0] {
						skips = append(skips, realignSkip{kind: "conflict", path: orphan, reason: "exact and heuristic candidates disagree"})
						continue
					}
					if destinationBlocked(target, orphan) {
						skips = append(skips, realignSkip{kind: "conflict", path: orphan, reason: "destination " + target + " already exists"})
						continue
					}
					moves = append(moves, realignMove{orphan: orphan, target: target, method: "exact", libraryID: lib.ID, eligible: true})
				default: // "none": no provenance match
					if !dirPair {
						reason := fmt.Sprintf("%d orphan sidecar(s), %d audio file(s) missing a sidecar; cannot pair without provenance", len(orphans), len(missingAudio))
						skips = append(skips, realignSkip{kind: "ambiguous", path: orphan, reason: reason})
						continue
					}
					audio := missingAudio[0]
					target := destForAudio(audio, orphanExt)
					if destinationBlocked(target, orphan) {
						skips = append(skips, realignSkip{kind: "conflict", path: orphan, reason: "destination " + target + " already exists"})
						continue
					}
					ok, score := heuristicNameGuard(orphanTags, stemOf(orphan), getProv(audio), stemOf(audio), minConf)
					if !ok {
						// The lone in-directory pair exists but the name guard is not
						// confident enough to trust it; report as ambiguous and never guess.
						skips = append(skips, realignSkip{kind: "ambiguous", path: orphan, reason: fmt.Sprintf("name similarity %.2f below min_confidence %.2f", score, minConf)})
						continue
					}
					mv := realignMove{orphan: orphan, target: target, method: "heuristic", libraryID: lib.ID, eligible: !rc.RequireProvenance, confidence: score}
					if !mv.eligible {
						mv.gateReason = "require_provenance is set; heuristic matches are not applied"
					}
					moves = append(moves, mv)
				}
			}
		}
	}

	return finishRealign(out, args, cfg, moves, skips, dirsChecked, orphansSeen)
}

// finishRealign prints planned moves and skips, applies eligible moves under
// --yes with a backup-first clobber-safe rename, and prints the summary.
func finishRealign(out io.Writer, args RealignCmd, cfg config.Config, moves []realignMove, skips []realignSkip, dirsChecked, orphansSeen int) int {
	// Report ambiguous/conflict directories first (deterministic order).
	sort.Slice(skips, func(i, j int) bool {
		if skips[i].kind != skips[j].kind {
			return skips[i].kind < skips[j].kind
		}
		return skips[i].path < skips[j].path
	})
	var ambiguousN, conflictN int
	for _, s := range skips {
		switch s.kind {
		case "ambiguous":
			ambiguousN++
		case "conflict":
			conflictN++
		}
		_, _ = fmt.Fprintf(out, "%s: %s (%s); skipped\n", s.kind, s.orphanLabel(), s.reason)
	}

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("realign-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backup *os.File
	defer func() {
		if backup != nil {
			_ = backup.Close()
		}
	}()

	var (
		exactPlanned, heuristicPlanned int
		exactApplied, heuristicApplied int
		gatedSkipped, errCount         int
	)
	for _, mv := range moves {
		switch mv.method {
		case "exact":
			exactPlanned++
		case "heuristic":
			heuristicPlanned++
		}

		if !mv.eligible {
			gatedSkipped++
			_, _ = fmt.Fprintf(out, "skip [%s]: %s -> %s (%s)\n", mv.method, mv.orphan, mv.target, mv.gateReason)
			continue
		}

		if !args.Yes {
			_, _ = fmt.Fprintf(out, "would move [%s]: %s -> %s\n", mv.method, mv.orphan, mv.target)
			continue
		}

		// Apply order: backup first (skip this move if it fails), then rename, then
		// fsync the destination directory for crash durability.
		if backup == nil {
			f, ferr := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
			if ferr != nil {
				slog.Error("failed to open realign backup file", "path", backupPath, "error", ferr)
				return 1
			}
			backup = f
		}
		if berr := appendRealignBackup(backup, mv); berr != nil {
			slog.Error("realign: backup write failed; skipping move", "orphan", mv.orphan, "error", berr)
			errCount++
			continue
		}
		if rerr := os.Rename(mv.orphan, mv.target); rerr != nil {
			slog.Warn("realign: rename failed; leaving sidecar in place", "orphan", mv.orphan, "target", mv.target, "error", rerr)
			errCount++
			continue
		}
		lyrics.FsyncDir(filepath.Dir(mv.target))
		switch mv.method {
		case "exact":
			exactApplied++
		case "heuristic":
			heuristicApplied++
		}
		_, _ = fmt.Fprintf(out, "moved [%s]: %s -> %s\n", mv.method, mv.orphan, mv.target)
	}

	applied := exactApplied + heuristicApplied
	if args.Yes {
		_, _ = fmt.Fprintf(out, "realign done: dirs=%d orphans=%d applied=%d (exact=%d heuristic=%d) gated-skipped=%d ambiguous=%d conflict=%d errors=%d\n",
			dirsChecked, orphansSeen, applied, exactApplied, heuristicApplied, gatedSkipped, ambiguousN, conflictN, errCount)
		if applied > 0 {
			_, _ = fmt.Fprintf(out, "backup of applied moves written to %s\n", backupPath)
		}
	} else {
		_, _ = fmt.Fprintf(out, "realign summary: dirs=%d orphans=%d planned=%d (exact=%d heuristic=%d) gated-skipped=%d ambiguous=%d conflict=%d%s\n",
			dirsChecked, orphansSeen, exactPlanned+heuristicPlanned-gatedSkipped, exactPlanned, heuristicPlanned, gatedSkipped, ambiguousN, conflictN, suffixDryRun(args.Yes))
	}
	if errCount > 0 {
		return 1
	}
	return 0
}

// orphanLabel renders the subject of a skip line.
func (s realignSkip) orphanLabel() string { return s.path }

// realignDirEntry holds the audio files and sidecars found in one directory.
type realignDirEntry struct {
	audio    []string
	sidecars []string
}

// walkRealign walks root and partitions every regular file into audio files and
// .lrc/.txt sidecars, grouped by directory. It also returns the flat list of all
// audio paths for cross-directory exact matching. Every path stays under root by
// construction (filepath.WalkDir over the resolved root).
func walkRealign(root string) (map[string]*realignDirEntry, []string, error) {
	dirs := map[string]*realignDirEntry{}
	var allAudio []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			// A single unreadable entry should not abort the whole walk.
			slog.Warn("realign: skipping unreadable path", "path", p, "error", err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		dir := filepath.Dir(p)
		entry := dirs[dir]
		if entry == nil {
			entry = &realignDirEntry{}
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
		return nil, nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return dirs, allAudio, nil
}

// resolveRealignExact finds the unique audio file in pool whose embedded ISRC or
// MBID matches the orphan's, honoring identityKeys order (most authoritative
// first). It returns ("", "none") when the orphan carries no matchable id or no
// audio matches, (path, "unique") on a single match, and ("", "conflict") when
// more than one audio shares the same id.
func resolveRealignExact(tags lyrics.ProvenanceTags, identityKeys, pool []string, getProv func(string) audioProvenance) (string, string) {
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
			continue // try the next identity key
		case 1:
			return matches[0], "unique"
		default:
			return "", "conflict"
		}
	}
	return "", "none"
}

// heuristicNameGuard implements the min_confidence name guard for the heuristic
// tier. It compares the orphan's [ar:]/[ti:] header (falling back to the sidecar
// stem) against the candidate audio's artist/title tags via Jaro-Winkler, and
// reports whether the score meets minConf. When neither side yields an artist or
// title it degrades gracefully to pure positional matching (returns true), rather
// than erroring, so a plain .txt with no header still realigns.
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

// appendRealignBackup writes one JSONL backup record for an applied move.
func appendRealignBackup(f *os.File, mv realignMove) error {
	rec := realignBackupRecord{OldPath: mv.orphan, NewPath: mv.target, LibraryID: mv.libraryID, Method: mv.method}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal realign backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write realign backup record: %w", err)
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
	return err == nil
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

// stemSet returns the set of stems for the given paths.
func stemSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[stemOf(p)] = true
	}
	return set
}

// normalizeIdentityKeys lowercases, filters to known identity keys (mbid, isrc),
// and de-duplicates while preserving order.
func normalizeIdentityKeys(keys []string) []string {
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
