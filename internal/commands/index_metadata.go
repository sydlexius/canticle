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
	"github.com/sydlexius/canticle/internal/repair"
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
	sqlDB, err := db.OpenForBatch(ctx, cfg.DB.Path, args.Yes)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	roots, err := resolveIndexMetadataRoots(ctx, sqlDB, args.Library)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
			return 1
		}
		slog.Error("index-metadata: failed to resolve library roots", "error", err)
		return 1
	}
	if len(roots) == 0 {
		_, _ = fmt.Fprintln(out, "index-metadata: no library roots configured")
		return 0
	}

	store := audiometa.New(sqlDB, scanner.DurationReaderVersion)

	// A --library run scopes the printed coverage number to that library's
	// canonical root, per #646's AC ("how many files in library N have
	// complete metadata"); a whole-library-set run keeps the global total.
	// Resolved once, outside the walk loop, from the same roots list.
	coveragePrefix := ""
	if len(roots) == 1 && strings.TrimSpace(args.Library) != "" {
		_, canonRoot := pathutil.CanonicalRoot(roots[0])
		coveragePrefix = canonRoot + string(os.PathSeparator)
	}

	res, err := IndexMetadata(ctx, sqlDB, roots, IndexMetadataOptions{Apply: args.Yes, Limit: args.Limit})
	interrupted := false
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("index-metadata failed", "error", err)
			return 1
		}
		slog.Info("index-metadata: interrupted", "walked", res.Walked, "indexed", res.Indexed)
		interrupted = true
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
	if res.DurationFailures > 0 {
		durationNote = fmt.Sprintf("; %d duration parse failure(s)", res.DurationFailures)
	}
	// The recording-ID fill (#1520) is appended only when non-zero, like the
	// duration note: the healthy steady state reads exactly as before.
	if res.MBIDFilled > 0 {
		fillVerb := "would fill"
		if args.Yes {
			fillVerb = "filled"
		}
		durationNote += fmt.Sprintf("; %s the recording ID on %d empty scan row(s)", fillVerb, res.MBIDFilled)
	}
	_, _ = fmt.Fprintf(out, "index-metadata: walked %d file(s); %s %d (%d skipped unchanged, %d unreadable, %d walk error(s))%s; coverage now %d row(s)%s\n",
		res.Walked, verb, res.Indexed, res.Skipped, res.Unreadable, res.WalkErrors, durationNote, coverage, suffixDryRun(args.Yes))
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
	if res.WalkErrors > 0 {
		return 1
	}
	return 0
}

// IndexMetadataResult is the running tally of one index-metadata run. It holds
// counts only, never a path or a tag value, so any caller may print or log it.
type IndexMetadataResult struct {
	// Walked counts every audio file the walk reaches, independent of --limit.
	Walked int
	// Indexed counts files whose tags were read and recorded (or would be, on a dry run).
	Indexed int
	// Skipped counts files an already-current row covered, so none was opened.
	Skipped int
	// Unreadable counts files that could not be stat'd or whose tags could not be read.
	Unreadable int
	// WalkErrors counts unreachable roots and untraversable subtrees; the other
	// counts are then a floor, not a full accounting.
	WalkErrors int
	// WorkDone counts files actually read, the quantity --limit is charged against.
	WorkDone int
	// DurationFailures counts files whose tags read but whose duration did not parse.
	DurationFailures int
	// MBIDFilled counts scan rows whose empty recording ID was (or would be) filled.
	MBIDFilled int
	// LimitReached reports that the --limit budget stopped the walk early.
	LimitReached bool
}

// IndexMetadataOptions control one run.
type IndexMetadataOptions struct {
	// Apply writes the rows; without it the run only counts.
	Apply bool
	// Limit is a per-RUN budget of files read across ALL roots (0 = no limit).
	Limit int
	// Progress, when non-nil, is called once for each audio file the walk reaches
	// with the number reached so far and a total of 0: the total is unknown
	// because counting it would need a second walk of the library, which is
	// exactly the disk I/O this command avoids. It must not block.
	Progress repair.Progress
}

func (o IndexMetadataOptions) report(done int) {
	if o.Progress != nil {
		o.Progress(int64(done), 0)
	}
}

// resolveIndexMetadataRoots returns the library roots to walk: the one named by
// libraryArg (a name or numeric id) or, when it is blank, every configured
// root. A library that does not exist yields an error wrapping sql.ErrNoRows.
func resolveIndexMetadataRoots(ctx context.Context, sqlDB *sql.DB, libraryArg string) ([]string, error) {
	libRepo := library.New(sqlDB)
	if strings.TrimSpace(libraryArg) != "" {
		lib, err := resolveLibrary(ctx, libRepo, libraryArg)
		if err != nil {
			return nil, fmt.Errorf("resolve library: %w", err)
		}
		return []string{lib.Path}, nil
	}
	libs, err := libRepo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list libraries: %w", err)
	}
	roots := make([]string, 0, len(libs))
	for _, l := range libs {
		roots = append(roots, l.Path)
	}
	return roots, nil
}

// IndexMetadata walks roots against an already-open database and returns the
// tally. The result is returned even alongside an error (a canceled context
// returns context.Canceled with the work already committed counted), so the
// caller can still print an accurate summary. The returned error may contain a
// library path and is not display text; callers must not show or log it verbatim.
func IndexMetadata(ctx context.Context, sqlDB *sql.DB, roots []string, opts IndexMetadataOptions) (IndexMetadataResult, error) {
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

	// res is one value for the whole run so --limit (res.WorkDone) is a per-RUN
	// budget, not a per-ROOT one: with N roots, a per-root counter would let up
	// to Limit*N files be read in one invocation (finding: --limit is per-root,
	// not per-run).
	var res IndexMetadataResult
	for _, root := range roots {
		if res.LimitReached {
			break
		}
		// A root that cannot even be stat'd (unmounted NAS, permission denied
		// on the mount point, a typo'd path) must never read as a silently
		// empty walk: filepath.WalkDir would otherwise invoke walkFn once
		// with the error and immediately stop, producing "walked 0" that
		// looks identical to a genuinely empty, healthy library. Stat the
		// root up front, count it as a hard walk error, and skip walking it
		// -- the other configured roots still get processed, but the run's
		// exit code will reflect that this root was unreachable.
		if _, serr := os.Stat(root); serr != nil {
			slog.Error("index-metadata: library root is unreachable; skipping", "root", root, "error", serr)
			res.WalkErrors++
			continue
		}
		if err := walkIndexMetadata(ctx, sqlDB, store, durations, root, opts, &res); err != nil {
			return res, err
		}
	}
	return res, nil
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
// res.WorkDone lives in the caller's result (IndexMetadata), not in a local
// declared fresh per call, because --limit is a per-RUN budget across ALL
// configured roots, not a per-root allowance: a local counter here would
// reset to zero at the start of every root, letting a run over N roots read
// up to Limit*N files (finding: --limit is per-root, not per-run).
func walkIndexMetadata(ctx context.Context, sqlDB *sql.DB, store *audiometa.Store, durations *audiodur.Store, root string, opts IndexMetadataOptions, res *IndexMetadataResult) error {
	absRoot, canonRoot := pathutil.CanonicalRoot(root)
	// Walk the resolved root: WalkDir does not descend through a symlinked
	// root, so walking absRoot would visit nothing under /music -> /mnt/array/music.
	return filepath.WalkDir(canonRoot, func(p string, d os.DirEntry, walkErr error) error {
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
			res.WalkErrors++
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

		// Walked reports every audio file the walk reaches, independent of the
		// limit; only the limit gate below cares about work done.
		res.Walked++
		opts.report(res.Walked)
		canonPath := p
		walkPath := pathutil.RebaseUnderCanonicalRoot(canonRoot, absRoot, p) // the scanner's spelling

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
			res.Unreadable++
			return nil
		}
		mtimeNano, size := info.ModTime().UnixNano(), info.Size()

		hit, lerr := store.Lookup(ctx, canonPath, mtimeNano, size)
		if lerr != nil {
			return fmt.Errorf("index-metadata: lookup %q: %w", canonPath, lerr)
		}
		if hit {
			res.Skipped++
			// Bank the duration from the METADATA ROW, not from a re-read (#724).
			//
			// #720 attached the duration write to the read path below, but this
			// skip returns first, so a file already in audio_metadata could never
			// gain a duration row: measured on prod, 12,990 rows had a current
			// metadata row and no duration row, and the sweep banked exactly zero
			// of them however many times it ran. The skip is correct and
			// load-bearing -- it is what makes a re-run over an unchanged library
			// nearly free on spun-down disks -- so the fix belongs HERE, not in
			// weakening it.
			//
			// NO FILE IS OPENED. Facts re-queries the row the Lookup above just
			// validated against this same (mtime, size), so the duration is
			// already stored and costs one indexed SELECT rather than a spin-up.
			// That makes this strictly cheaper than the read path it complements.
			//
			// Non-fatal for the same reason as the read path: audio_metadata is
			// this command's product, audio_durations an opportunistic byproduct.
			// Record itself no-ops on a non-positive duration, so a row whose
			// duration never parsed banks nothing rather than a misleading 0 --
			// absence is how that table spells "unknown".
			facts, ok, ferr := store.Facts(ctx, canonPath, mtimeNano, size)
			switch {
			case ferr != nil:
				slog.Warn("index-metadata: could not read cached facts to bank duration", "file", canonPath, "error", ferr)
			// GATED ON opts.Apply, like every other write in this command. Without
			// it a DRY RUN mutated audio_durations on any already-indexed
			// library: the skip path returns before the opts.Apply check that
			// guards the read-path write, so "preview" silently wrote rows. A
			// preview that mutates is worse than no preview, because the operator
			// has been told it is safe.
			case ok && opts.Apply:
				if derr := durations.Record(ctx, canonPath, mtimeNano, size, facts.TrackLength); derr != nil {
					slog.Warn("index-metadata: could not bank duration from the metadata row", "file", canonPath, "error", derr)
				}
			}
			// The same cached row also settles the scan row's recording ID with
			// no file open, which covers a run that was interrupted between the
			// cache write and the fill (#1520).
			if ok {
				fillScanRecordingMBID(ctx, sqlDB, walkPath, canonPath, facts.MBID, opts.Apply, &res.MBIDFilled)
			}
			return nil
		}

		// This file needs real work (a tag read). Only now does it count
		// against --limit: an already-indexed file above never reaches here.
		if opts.Limit > 0 && res.WorkDone >= opts.Limit {
			res.LimitReached = true
			return filepath.SkipAll
		}

		facts, ferr := scanner.ReadAudioFacts(p)
		if ferr != nil {
			slog.Debug("index-metadata: failed to read audio tags; counting unreadable", "file", p, "error", ferr)
			res.Unreadable++
			res.WorkDone++
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
			res.DurationFailures++
		}

		if opts.Apply {
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
		fillScanRecordingMBID(ctx, sqlDB, walkPath, canonPath, facts.MBID, opts.Apply, &res.MBIDFilled)
		res.Indexed++
		res.WorkDone++
		return nil
	})
}

// fillScanRecordingMBID copies a file's recording ID from the tag cache onto
// its scan_results row (#1520). It FILLS ONLY: the UPDATE matches rows whose
// recording_mbid is empty, so a non-empty value is never overwritten here (a
// scan rewrites it, and the scan upsert keeps a non-empty value over an empty
// one). A settled file is never opened by a scan, so without this a row stored
// before the Vorbis fix keeps an empty ID forever and `scan reconcile-paths`
// retains it as "identity absent".
//
// The scanner stores file_path as the walk path under the library root as
// configured, while the tag-cache key is the symlink-resolved path, so a
// symlinked root spells the same file two ways: the row matches under either
// walkPath or canonPath (equal for an ordinary root, so nothing double counts).
//
// A row for a file that is gone is never visited, because the caller walks
// files that exist. Best-effort: a failure is logged and the run continues,
// since audio_metadata is this command's product. A dry run only counts the
// rows it would fill and writes nothing. filled counts rows, never IDs.
func fillScanRecordingMBID(ctx context.Context, sqlDB *sql.DB, walkPath, path, mbid string, apply bool, filled *int) {
	if mbid == "" {
		return
	}
	if !apply {
		// COUNT(*) over the same predicate as the UPDATE below, so the dry run
		// reports the number of rows the real run fills (two rows may share a path).
		var n int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM scan_results WHERE (file_path = ? OR file_path = ?) AND recording_mbid = ''`, walkPath, path).Scan(&n); err != nil {
			slog.Warn("index-metadata: could not check the scan row for a recording ID", "file", path, "error", err)
			return
		}
		*filled += n
		return
	}
	res, err := sqlDB.ExecContext(ctx,
		`UPDATE scan_results SET recording_mbid = ? WHERE (file_path = ? OR file_path = ?) AND recording_mbid = ''`, mbid, walkPath, path)
	if err != nil {
		slog.Warn("index-metadata: could not fill the scan row's recording ID", "file", path, "error", err)
		return
	}
	if n, rerr := res.RowsAffected(); rerr == nil {
		*filled += int(n)
	}
}
