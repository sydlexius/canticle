package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/scanner"
)

// indexMetadataRowCommitted, when non-nil, is invoked synchronously
// immediately after walkIndexMetadata successfully commits a row to
// audio_metadata (--yes runs only). It is a test-only synchronization seam
// (default nil = no-op in production, the same pattern as
// watcher.Watcher.armed) that lets a test observe "a row has just landed in
// the DB" deterministically -- via a channel send -- instead of polling
// countAudioMetadataRows on a 1ms sleep loop, which raced the DB's
// single-connection WAL setup on every db.Open and made the observed timing
// depend on scheduler luck rather than an actual ordering guarantee.
var indexMetadataRowCommitted func()

// runIndexMetadata walks a library's audio files and records the full tag set
// into audio_metadata (#646). Dry-run by default; --yes writes. A file whose
// (path, mtime, size) still matches its row is skipped without being opened,
// so a re-run over an unchanged library is nearly free and doubles as a
// coverage check.
//
// There is deliberately no JSONL backup here, unlike the reconcile-* commands:
// this operation is purely additive and destroys nothing, so there is nothing
// to restore.
func runIndexMetadata(ctx context.Context, out io.Writer, args ScanIndexMetadataCmd) int {
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
		_, _ = fmt.Fprintln(out, "index-metadata: no library roots configured")
		return 0
	}

	store := audiometa.New(sqlDB, scanner.DurationReaderVersion)

	// Bank the duration from the SAME read that fills audio_metadata (#717).
	//
	// This walk already opens every file and already holds facts.TrackLength
	// alongside the (mtime, size) audiodur keys on -- it simply threw the
	// duration away, so audio_durations stayed sparse while audio_metadata went
	// complete. Measured on the reference library: 0 scan_results rows missing a
	// metadata row, but 12,990 missing a duration row. Every one of those is a
	// file this sweep had open, read, and could have settled.
	//
	// The consequence is a repeat OPEN, which is what makes it worth fixing:
	// a spin-up event costs the same whether it reads one byte or a gigabyte, so
	// the count of touches is the metric, not their size. A duration miss sends
	// a later caller back to the disk for a file this command already read.
	//
	// Both stores share scanner.DurationReaderVersion, so a parser change
	// invalidates the two together and they can never disagree about which
	// reader produced them.
	durations := audiodur.New(sqlDB, scanner.DurationReaderVersion)

	// A --library run scopes the printed coverage number to that library's
	// canonical root, per #646's AC ("how many files in library N have
	// complete metadata"); a whole-library-set run keeps the global total.
	// Resolved once, outside the walk loop, from the same roots list.
	coveragePrefix := ""
	if len(roots) == 1 && strings.TrimSpace(args.Library) != "" {
		_, canonRoot := pathutil.CanonicalRoot(roots[0])
		coveragePrefix = canonRoot + string(os.PathSeparator)
	}

	// workDone is hoisted here (not declared per-root inside walkIndexMetadata)
	// so --limit is a per-RUN budget, not a per-ROOT one: with N roots, a
	// per-root counter would let up to Limit*N files be read in one invocation
	// (finding: --limit is per-root, not per-run). Threading it as a *int
	// alongside the other counters makes the budget carry over correctly from
	// one root to the next, exactly like walked/indexed/skipped/unreadable.
	var walked, indexed, skipped, unreadable, walkErrors, workDone, durationFailures int
	limitReached := false
	interrupted := false
	for _, root := range roots {
		if limitReached {
			break
		}
		// A root that cannot even be stat'd (unmounted NAS, permission denied
		// on the mount point, a typo'd path) must never read as a silently
		// empty walk: filepath.WalkDir would otherwise invoke walkFn once
		// with the error and immediately stop, producing "walked 0" that
		// looks identical to a genuinely empty, healthy library. Stat the
		// root up front, count it as a hard walk error, and skip walking it
		// -- the other configured roots still get processed, but the run's
		// exit code (below) will reflect that this root was unreachable.
		if _, serr := os.Stat(root); serr != nil {
			slog.Error("index-metadata: library root is unreachable; skipping", "root", root, "error", serr)
			walkErrors++
			continue
		}
		if err := walkIndexMetadata(ctx, store, durations, root, args, &walked, &indexed, &skipped, &unreadable, &walkErrors, &workDone, &durationFailures, &limitReached); err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("index-metadata: interrupted", "walked", walked, "indexed", indexed)
				interrupted = true
				break
			}
			slog.Error("index-metadata failed", "error", err)
			return 1
		}
	}

	// On interrupt, the incoming ctx is already canceled, so a Coverage query
	// against it would fail and the operator would lose the summary for work
	// already committed to the DB. context.WithoutCancel keeps the same
	// deadline-less lineage but drops the cancellation, so the query still
	// runs against the (uncanceled) sqlDB connection.
	coverageCtx := ctx
	if interrupted {
		coverageCtx = context.WithoutCancel(ctx)
	}
	coverage, cerr := store.Coverage(coverageCtx, coveragePrefix)
	if cerr != nil {
		slog.Error("index-metadata: failed to read coverage", "error", cerr)
		return 1
	}

	verb := "would index"
	if args.Yes {
		verb = "indexed"
	}
	// Duration-parse failures are appended only when non-zero, so the healthy
	// case reads exactly as before and a non-zero count is conspicuous rather
	// than a column of noise an operator learns to skip (#651).
	durationNote := ""
	if durationFailures > 0 {
		durationNote = fmt.Sprintf("; %d duration parse failure(s)", durationFailures)
	}
	_, _ = fmt.Fprintf(out, "index-metadata: walked %d file(s); %s %d (%d skipped unchanged, %d unreadable, %d walk error(s))%s; coverage now %d row(s)%s\n",
		walked, verb, indexed, skipped, unreadable, walkErrors, durationNote, coverage, suffixDryRun(args.Yes))
	// An operator-initiated Ctrl-C after partial, already-committed work is
	// not a failure: the work done so far is real and the summary above
	// reports it accurately, so exit 0 rather than 1. A genuine error path
	// (config/db/walk failure) still returns 1 above.
	//
	// walkErrors > 0, though, IS a failure worth a non-zero exit: it means a
	// library root could not be stat'd, or a subtree could not be traversed
	// (permission denied, an unmounted filesystem going away mid-walk), so
	// the printed counts are a floor, not the true state of the library. An
	// operator reading exit 0 here would reasonably believe the run was
	// complete; on an 84k-file production library the realistic case is a
	// vanished NAS mount producing "walked 0", which must not report success.
	// This is distinct from per-FILE unreadable counts, which are expected
	// noise on a large library and intentionally keep exit 0.
	if walkErrors > 0 {
		return 1
	}
	return 0
}

// walkIndexMetadata walks one library root's audio files, updating the running
// counters. It canonicalizes root exactly once (#643) -- via
// pathutil.CanonicalRoot -- and rebases every file under it with
// pathutil.RebaseUnderCanonicalRoot, so the metadata key matches the one a
// library scan would produce for the same file. Building the key any other way
// produces a duplicate row for the same inode, not merely a stale one.
//
// --limit is charged against WORK DONE (files actually read via
// scanner.ReadAudioFacts, whether or not the read succeeded), never against
// files skipped because an already-current row covers them. A file already
// indexed costs only a stat and a cache lookup, not a tag read, so letting it
// consume limit budget would mean a bounded run over a mostly-indexed library
// never reaches the files that actually need reading -- repeated `--limit N`
// runs would reprocess the same first N unindexed files forever instead of
// advancing through the library (issue: --limit cannot resume). Gating the
// limit on work-done instead makes resumability fall out for free: each run
// picks up wherever the previous one's budget ran out, with no stored cursor.
//
// workDone is a *int owned by the caller (runIndexMetadata), not a local
// declared fresh per call, because --limit is a per-RUN budget across ALL
// configured roots, not a per-root allowance: a local counter here would
// reset to zero at the start of every root, letting a run over N roots read
// up to Limit*N files (finding: --limit is per-root, not per-run).
func walkIndexMetadata(ctx context.Context, store *audiometa.Store, durations *audiodur.Store, root string, args ScanIndexMetadataCmd,
	walked, indexed, skipped, unreadable, walkErrors, workDone, durationFailures *int, limitReached *bool,
) error {
	absRoot, canonRoot := pathutil.CanonicalRoot(root)
	return filepath.WalkDir(absRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A per-entry walk error (permission denied on a subtree, a
			// directory that vanished mid-walk) must not abort the whole
			// run, but it also must not be invisible: it means the walk did
			// NOT see everything under this root, so the counters below are
			// a floor, not a complete accounting. walkErrors surfaces that in
			// the summary line and drives runIndexMetadata's exit code; Debug
			// is still the right level for the per-entry detail on an 84k-
			// file library (see the per-file unreadable path below for the
			// same reasoning), but the aggregate is no longer silent.
			slog.Debug("index-metadata: error walking path; skipping", "path", p, "error", walkErr)
			*walkErrors++
			return nil //nolint:nilerr // reason: a single unreadable directory entry must not abort the whole walk
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !scanner.IsAudioFile(d.Name()) {
			return nil
		}

		// walked reports every audio file the walk reaches, independent of the
		// limit; only the limit gate below cares about work done.
		*walked++
		canonPath := pathutil.RebaseUnderCanonicalRoot(absRoot, canonRoot, p)

		// d.Info() is an LSTAT: for a symlinked audio file it describes the
		// symlink itself, not its target. scanner.ReadAudioFacts, though,
		// opens the file and stamps MTimeNano/SizeBytes from an FSTAT on that
		// open handle -- the target's stat, not the link's. Those two never
		// agree for a symlink (a link's own size is the length of the path
		// text it holds, typically far smaller than the target audio file),
		// so store.Lookup's (mtime, size) key would never hit and the file
		// would be re-read and re-recorded on every single run forever --
		// unbounded repeated tag-read I/O on exactly the spun-down disks this
		// skip-before-open design exists to protect (finding 3).
		//
		// The fix is to follow the link when stat'ing an individual file, via
		// os.Stat instead of d.Info(), so the stamp we look up against is
		// taken from the same target the tag reader will open. This is
		// deliberately narrower than resolving the file's path for the
		// audio_metadata KEY: migration 035's comment block resolves symlinks
		// only for the library ROOT, once per scan, specifically to avoid an
		// EvalSymlinks per file on a large tree. Following a symlink's stat
		// here costs no extra syscall of that kind -- os.Stat is a single
		// syscall, no different in cost from the os.Lstat a symlinked
		// DirEntry would otherwise need -- and d.Type() already carries the
		// symlink bit for free from readdir, so detecting the case adds no
		// extra syscall either. It does not touch canonPath (the key), only
		// the stamp used to decide whether the file has already been read.
		var info os.FileInfo
		var ierr error
		if d.Type()&fs.ModeSymlink != 0 {
			info, ierr = os.Stat(p)
		} else {
			info, ierr = d.Info()
		}
		if ierr != nil {
			slog.Debug("index-metadata: could not stat file; counting unreadable", "file", p, "error", ierr)
			*unreadable++
			return nil
		}
		mtimeNano, size := info.ModTime().UnixNano(), info.Size()

		hit, lerr := store.Lookup(ctx, canonPath, mtimeNano, size)
		if lerr != nil {
			return fmt.Errorf("index-metadata: lookup %q: %w", canonPath, lerr)
		}
		if hit {
			*skipped++
			return nil
		}

		// This file needs real work (a tag read). Only now does it count
		// against --limit: an already-indexed file above never reaches here.
		if args.Limit > 0 && *workDone >= args.Limit {
			*limitReached = true
			return filepath.SkipAll
		}

		facts, ferr := scanner.ReadAudioFacts(p)
		if ferr != nil {
			slog.Debug("index-metadata: failed to read audio tags; counting unreadable", "file", p, "error", ferr)
			*unreadable++
			*workDone++
			return nil
		}

		// A file whose TAGS read fine but whose DURATION could not be parsed is
		// neither unreadable nor a walk error -- the row is written, only the
		// duration is missing (#651). Counted separately so it cannot hide in
		// either bucket: this is the exact class of failure that let a
		// third-party parser defect sit unnoticed until someone inspected an
		// index run by hand. The row is still recorded; the count only makes the
		// gap visible.
		if facts.DurationErr != nil {
			*durationFailures++
		}

		if args.Yes {
			if rerr := store.Record(ctx, canonPath, facts); rerr != nil {
				return fmt.Errorf("index-metadata: record %q: %w", canonPath, rerr)
			}
			// Bank the duration from the read just performed (#717). Keyed on the
			// identity the OPEN HANDLE reported (facts.MTimeNano/SizeBytes), not a
			// fresh os.Stat: re-statting would both cost another syscall and open a
			// TOCTOU window where a file rewritten mid-walk gets its new stat
			// stamped onto the old read's duration.
			//
			// NON-FATAL, unlike the metadata Record above. audio_metadata is this
			// command's product and a failure there means the run did not do its
			// job; audio_durations is an opportunistic byproduct, so failing the
			// whole sweep over it would trade a complete index for a partial one.
			// Record itself no-ops on a non-positive duration, so a file with an
			// unparsable duration (counted above) simply banks nothing.
			if derr := durations.Record(ctx, canonPath, facts.MTimeNano, facts.SizeBytes, facts.TrackLength); derr != nil {
				slog.Warn("index-metadata: could not bank duration; metadata row still recorded", "file", canonPath, "error", derr)
			}
			if indexMetadataRowCommitted != nil {
				indexMetadataRowCommitted()
			}
		}
		*indexed++
		*workDone++
		return nil
	})
}
