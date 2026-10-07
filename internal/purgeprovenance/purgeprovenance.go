// Package purgeprovenance bulk-deletes LRC/TXT sidecars matching a provenance
// filter (an exact [source:] tag, or the no-tag "inherited/foreign" cohort) and
// requeues the coupled work_queue/scan_results rows so the next scan re-fetches
// from current or better providers (issue #474).
//
// Unlike every other reconcile command to date, this one deletes real files
// from disk (not just database rows), so it follows a stricter discipline:
// dry-run by default, a caller-supplied Report invoked BEFORE each delete so a
// backup can be written and fsynced write-ahead, symlinked sidecars are never
// touched, and enumeration never leaves the configured library roots.
//
// It also cross-checks each candidate's on-disk [source:] tag against the
// coupled work_queue.provider_lane and refuses the delete when the two name
// different providers (issue #827). Sidecars written while the lane
// misattribution bug of #826 was live carry a tag that does not match the row,
// so selecting by the tag alone could delete a file the database credits to a
// different provider. Disputed provenance means the operator's --source cannot
// be established for that file, and this command deletes.
package purgeprovenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/revalidate"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// timeFormat matches the RFC3339-ish stamp the queue package writes for
// next_attempt_at, so a reset row sorts and compares consistently with rows
// written by the rest of the system.
const timeFormat = "2006-01-02T15:04:05Z"

func formatNow() string {
	return time.Now().UTC().Format(timeFormat)
}

// Filter selects which sidecars are in scope for a purge run. Exactly one of
// Source (non-empty), NoSource or Generated should be set by the caller;
// selects checks whichever is configured.
type Filter struct {
	// Source, when non-empty, matches sidecars whose [source:] tag equals this
	// value exactly.
	Source string
	// NoSource matches sidecars carrying no [source:] tag at all -- the
	// inherited/foreign cohort canticle never wrote.
	NoSource bool
	// Generated matches .lrc files whose header carries
	// [timing:canticle-aligner], an accepted aligner retiming (#1008). This
	// cohort is RESTORED from its .orig backup, never deleted or re-fetched
	// (see restoreGenerated).
	Generated bool
}

// selects reports whether a sidecar is in scope: by its [timing:] tag for the
// generated cohort (line-synced files only), else by its [source:] tag.
func (f Filter) selects(path string, pt lyrics.ProvenanceTags) bool {
	if f.Generated {
		return sidecar.KindOf(path) == sidecar.KindLineSynced && pt.Timing == lyrics.TimingAligner
	}
	return f.matches(pt.Source)
}

// matches reports whether a sidecar's read [source:] value is in scope.
func (f Filter) matches(source string) bool {
	if f.NoSource {
		return source == ""
	}
	return source == f.Source
}

// Options configures a purge run.
type Options struct {
	// Roots are the library directory trees to walk for *.lrc / *.txt sidecars.
	// Confines every candidate to these roots; walking never leaves them.
	Roots []string
	// LibraryID, when non-nil, narrows the database lookup (linking a sidecar to
	// its scan_results/work_queue rows) to one library.
	LibraryID *int64
	// Filter selects which sidecars are in scope.
	Filter Filter
	// DryRun computes and reports the purge set without deleting anything or
	// mutating the database.
	DryRun bool
	// Report, when set, is invoked once per matched, non-in-flight sidecar. In a
	// dry run it is a preview (no mutation follows). Under apply it is invoked
	// BEFORE the file is deleted, so a Report error aborts that sidecar's
	// deletion (backup-first): the caller writes and fsyncs a restorable JSONL
	// record here.
	//
	// It is ALSO invoked once for each matched sidecar's owned word-synced
	// companion (#986), right after that sidecar's own call and before the
	// companion is deleted, with a Record carrying only Path (no database IDs
	// or identities: the rows belong to the sidecar's record). A failure there
	// likewise aborts the pair's deletion.
	Report func(Record) error
}

// Record describes one matched sidecar and the database rows coupled to it, for
// preview output and the restorable JSONL backup. A record for an owned
// word-synced companion (#986) carries only Path; its rows are on the record
// of the sidecar it travels with.
type Record struct {
	// Path is the matched sidecar file (.lrc or .txt), or its owned
	// word-synced companion.
	Path string
	// ScanResultIDs are the scan_results rows whose expected output is this
	// sidecar (usually one; more than one only when distinct scan_results rows
	// happen to share the exact same output path, e.g. two audio formats of the
	// same track).
	ScanResultIDs []int64
	// WorkItemIDs are the work_queue rows linked to those scan_results rows,
	// deduplicated.
	WorkItemIDs []int64
	// Identities are the deduplicated (artist, title) pairs carried by those
	// scan_results rows -- the cache keys whose entries the purge invalidates.
	Identities []trackIdentity
}

// trackIdentity is one scan_results row's (artist, title) pair: the cache key
// EnqueuePending later probes for that row. Deliberately unexported -- it exists
// so the purge can invalidate exactly what the re-scan would look up, not as a
// public identity abstraction.
type trackIdentity struct {
	artist string
	title  string
}

// Artist returns the identity's artist as stored on the scan_results row.
func (t trackIdentity) Artist() string { return t.artist }

// Title returns the identity's title as stored on the scan_results row.
func (t trackIdentity) Title() string { return t.title }

// Result tallies a purge run.
type Result struct {
	Scanned           int // .lrc/.txt sidecars examined (symlinks excluded)
	Matched           int // sidecars whose provenance matched the filter
	Deleted           int // sidecars actually removed from disk (apply only)
	Restored          int // generated .lrc files replaced by their .orig backup (apply only; #1008)
	SkippedNoOriginal int // generated .lrc files left untouched: no regular .orig beside them
	CompanionsDeleted int // owned word-synced companions removed with their .lrc (apply only; #986)
	ScanResultsReset  int // scan_results rows reset to 'pending'
	WorkItemsRequeued int // work_queue rows reset to 'deferred' for re-fetch
	SkippedSymlink    int // symlinked sidecars never followed or touched
	SkippedProcessing int // matched sidecars left alone: a linked work_queue row is in-flight
	// SkippedManual counts matched sidecars left alone because a linked
	// work_queue row carries a manual instrumental mark (#1405), including
	// --source manual. Nothing is deleted, reset or invalidated for them.
	SkippedManual int
	// SkippedProvenanceMismatch counts matched sidecars refused because the
	// on-disk [source:] tag and the coupled work_queue.provider_lane name
	// different providers (issue #827). Nothing is deleted and no row is reset
	// for these.
	SkippedProvenanceMismatch int
	CacheInvalidated          int // lyrics_cache rows deleted so the requeued track re-fetches
	// UnlinkedNoCacheKey counts deleted sidecars that no scan_results row claims.
	// Nothing was requeued for them and no cache key could be derived, so a future
	// scan that rediscovers the track could still be served from cache.
	UnlinkedNoCacheKey int
	Errors             int // per-file failures (read, report, invalidate, delete, or reset); the run continues past them

	SkippedOriginalDiffers int // generated .lrc files left untouched: the .orig is another lyric (lyrics.SameLyric)
	RestoredNoRow          int // restores (planned ones, in a dry run) of a file with no work_queue row to unmark
	// SkippedOtherLibrary counts generated files a --library run left untouched
	// because the row found by its audio path is not provably that library's.
	SkippedOtherLibrary int
}

// Purger locates and purges provenance-matched sidecars against db.
type Purger struct {
	db *sql.DB
}

// New returns a Purger backed by db.
func New(db *sql.DB) *Purger {
	return &Purger{db: db}
}

// srInfo is one scan_results row's identity, indexed for expected-sidecar
// lookup. artist/title are the row's own metadata columns -- the exact values
// EnqueuePending later feeds to Cache.Lookup -- carried here so the purge can
// invalidate that cache entry from the same row it resets (see resetRows).
type srInfo struct {
	id     int64
	artist string
	title  string
}

// wqLink is one work_queue row linked to a scan_results row. lane is the row's
// provider_lane; it is empty when the column is NULL (not-yet-completed, or a
// verdict whose lane was cleared), which asserts nothing and so can never
// disagree with a tag.
type wqLink struct {
	id     int64
	status string
	lane   string
	manual bool // manual_instrumental_at is set (#1405)
}

// provenanceAgrees reports whether a sidecar's on-disk [source:] tag and a
// work_queue row's provider_lane name the same provider (issue #827).
//
// This is the deletion guard, so it is written to answer "is there positive
// evidence of DISAGREEMENT", not "do these strings match". Everything short of
// that -- an empty tag, a NULL lane -- agrees, because purge-provenance already
// has a well-defined behavior for those cohorts and this check exists to remove
// a hazard, not to add a new refusal to files nothing contradicts.
//
// The detector spelling is the one deliberate asymmetry: the writer stamps
// lyrics.SourceDetector ("canticle-detector") into a marker while the queue row
// carries the lane name ("detector"). Both constants are taken from their owning
// package rather than re-typed, so a rename cannot silently turn every
// instrumental marker in a library into a refusal.
func provenanceAgrees(tag, lane string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	lane = strings.ToLower(strings.TrimSpace(lane))
	if tag == "" || lane == "" {
		return true
	}
	if tag == lane {
		return true
	}
	return tag == lyrics.SourceDetector && lane == lyrics.DetectorLaneName
}

// Run walks every root for .lrc/.txt sidecars, matches each against opts.Filter
// via its [source:] tag, and -- for matches blocked by neither an in-flight
// linked work_queue row nor a provenance disagreement (see provenanceAgrees) --
// deletes the sidecar (apply only) and resets its coupled database rows so the
// next scan re-fetches. Per-file errors are counted and logged but never abort
// the run.
func (p *Purger) Run(ctx context.Context, opts Options) (Result, error) {
	idx, wqIdx, err := p.buildIndex(ctx, opts.LibraryID)
	if err != nil {
		return Result{}, err
	}

	var res Result
	for _, root := range opts.Roots {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A per-entry walk failure (an unreadable subdirectory, a file
				// that vanished mid-walk) is exactly the "per-file error" this
				// function's contract says is counted and logged, never fatal.
				// Returning it aborted the whole run -- including every root
				// still to come -- and, because runPurgeProvenance returns
				// early on a non-nil error, discarded the summary describing
				// what had ALREADY been deleted. Count and continue instead;
				// returning fs.SkipDir would silently drop the rest of a
				// directory, so the error is swallowed only for this entry.
				res.Errors++
				slog.Warn("purge-provenance: walk entry failed; skipping", "path", path, "error", err)
				return nil
			}
			// Context cancellation is genuinely fatal: it is a shutdown signal,
			// not a property of one file, and every later entry would fail the
			// same way. Abort the walk and let it propagate.
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			// Line-synced and unsynced sidecars only. A word-synced companion
			// (#986) is never judged on its own tags: it goes with its .lrc
			// (see processSidecar), so walking it here would handle it twice.
			if k := sidecar.KindOf(path); k != sidecar.KindLineSynced && k != sidecar.KindUnsynced {
				return nil
			}
			// Never follow a symlinked sidecar: skip it entirely, for both
			// matching and deletion. d.Type() is a Lstat-derived mode (no
			// symlink-following stat), matching the filesystem-tree WalkDir
			// contract.
			if d.Type()&fs.ModeSymlink != 0 {
				res.SkippedSymlink++
				return nil
			}
			p.processSidecar(ctx, path, idx, wqIdx, opts, &res)
			return nil
		})
		if walkErr != nil {
			// Only a fatal condition reaches here now (context cancellation);
			// per-entry failures were counted above and the walk continued.
			return res, fmt.Errorf("purgeprovenance: walk %s: %w", root, walkErr)
		}
	}
	return res, nil
}

// processSidecar examines one on-disk sidecar and, if it matches the filter and
// is not blocked by an in-flight linked row, deletes it (apply) and resets its
// coupled database rows. Failures are counted into res.Errors and logged; they
// never abort the walk.
func (p *Purger) processSidecar(ctx context.Context, path string, idx map[string][]srInfo, wqIdx map[int64][]wqLink, opts Options, res *Result) {
	pt, rerr := lyrics.ReadProvenanceTags(path)
	if rerr != nil {
		res.Errors++
		slog.Warn("purge-provenance: read provenance failed; skipping", "path", path, "error", rerr)
		return
	}
	res.Scanned++
	if !opts.Filter.selects(path, pt) {
		return
	}
	res.Matched++

	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	key := canonicalDir(filepath.Dir(path)) + "\x00" + stem
	srs := idx[key]

	var scanResultIDs, workItemIDs []int64
	var identities []trackIdentity
	seenID := make(map[trackIdentity]bool)
	seenWQ := make(map[int64]bool)
	processing, manual := false, false
	var mismatched []int64
	for _, sr := range srs {
		scanResultIDs = append(scanResultIDs, sr.id)
		id := trackIdentity{artist: sr.artist, title: sr.title}
		if !seenID[id] {
			seenID[id] = true
			identities = append(identities, id)
		}
		for _, link := range wqIdx[sr.id] {
			if link.status == "processing" {
				processing = true
			}
			manual = manual || link.manual
			// A restore puts the provider's own bytes back whichever lane the
			// row credits, so the #827 deletion guard does not apply to it.
			if !opts.Filter.Generated && !provenanceAgrees(pt.Source, link.lane) {
				mismatched = append(mismatched, link.id)
			}
			if !seenWQ[link.id] {
				seenWQ[link.id] = true
				workItemIDs = append(workItemIDs, link.id)
			}
		}
	}
	// The #827 deletion guard, checked BEFORE the Report call so a refused
	// sidecar never reaches the dry-run preview either: a purge that previews a
	// file it will not delete is worse than useless to the operator.
	//
	// It is ALSO checked before the in-flight return. A sidecar can be both
	// in-flight and disputed, and reporting it only as "skipped, processing"
	// hides the safety-relevant half: in-flight is a transient state that
	// resolves on the next run, while disputed provenance is a standing
	// contradiction the operator has to look at. So a disputed row is counted
	// and warned as disputed whatever its status.
	//
	// Exactly ONE of the two counters increments per sidecar, because the CLI
	// preview subtracts BOTH from the matched total; counting a sidecar twice
	// would undercount what a purge is going to delete.
	//
	// A single disagreeing row is enough. The two authorities for "which provider
	// served this" contradict each other, so which one the operator's --source
	// named is exactly what cannot be established -- and this command deletes.
	// The conservative branch is to leave the file alone and say so.
	//
	// Row ids only in the log: a work_queue row carries the library's private
	// artist/title metadata and the path is the same metadata by another name.
	// A manually marked row is never purged (#1405). Checked first so exactly
	// one counter increments, and not on a generated-retiming restore, which
	// touches only edit marks and never a marked row's file.
	if manual && !opts.Filter.Generated {
		res.SkippedManual++
		slog.Warn("purge-provenance: sidecar belongs to a manually marked instrumental; leaving it", "scan_result_ids", scanResultIDs)
		return
	}
	if len(mismatched) > 0 {
		res.SkippedProvenanceMismatch++
		slog.Warn("purge-provenance: sidecar provenance is disputed; refusing to delete it",
			"work_queue_ids", mismatched,
			"scan_result_ids", scanResultIDs,
			"reason", "the on-disk [source:] tag and work_queue.provider_lane name different providers")
		return
	}
	if processing {
		res.SkippedProcessing++
		return
	}
	if opts.Filter.Generated {
		p.restoreGenerated(ctx, path, pt, scanResultIDs, workItemIDs, opts, res)
		return
	}

	rec := Record{Path: path, ScanResultIDs: scanResultIDs, WorkItemIDs: workItemIDs, Identities: identities}
	// The owned word-synced companion (#986) goes with its .lrc, under its OWN
	// Report record (the .lrc's carries the rows). Foreign or absent: "", never
	// touched.
	companion := lyrics.OwnedCompanionOf(path)

	if opts.DryRun {
		if opts.Report != nil {
			if rerr := opts.Report(rec); rerr != nil {
				res.Errors++
				slog.Warn("purge-provenance: report failed", "path", path, "error", rerr)
			}
			if companion != "" {
				if rerr := opts.Report(Record{Path: companion}); rerr != nil {
					res.Errors++
					slog.Warn("purge-provenance: report failed", "path", companion, "error", rerr)
				}
			}
		}
		return
	}

	// Backup-first / write-ahead: the caller's Report writes and fsyncs the
	// restorable JSONL record before anything is deleted. A Report failure
	// skips this sidecar entirely -- it is never deleted without its record,
	// and neither is its companion.
	if opts.Report != nil {
		if rerr := opts.Report(rec); rerr != nil {
			res.Errors++
			slog.Warn("purge-provenance: backup failed; leaving sidecar untouched", "path", path, "error", rerr)
			return
		}
		if companion != "" {
			if rerr := opts.Report(Record{Path: companion}); rerr != nil {
				res.Errors++
				slog.Warn("purge-provenance: companion backup failed; leaving sidecar untouched", "path", companion, "error", rerr)
				return
			}
		}
	}

	// Database first, unlink second (#474 follow-up). The command's contract is
	// "delete this sidecar AND re-fetch it", so the two halves must not be able to
	// half-apply in the direction that loses the user's file. Deleting first left
	// the failure mode "file gone, rows never reset / cache never invalidated" --
	// the file is unrecoverable except from the JSONL backup and nothing ever
	// rewrites it. Committing the reset+invalidation first inverts that into
	// "rows requeued, file still on disk", which the next scan resolves by
	// re-fetching and overwriting the sidecar in place: wasted work, never loss.
	// Backup-first is unchanged -- the caller's Report has already fsynced the
	// restorable record above, before either half runs.
	if len(scanResultIDs) > 0 || len(workItemIDs) > 0 {
		srReset, wqReset, invalidated, rerr := p.resetRows(ctx, scanResultIDs, workItemIDs, identities, pt.Source)
		if errors.Is(rerr, errMarkedUnderfoot) {
			// Marked by hand after the index snapshot: the file is safe, and the
			// outcome is the same as for a row marked before the walk.
			res.SkippedManual++
			slog.Warn("purge-provenance: sidecar belongs to a manually marked instrumental; leaving it", "scan_result_ids", scanResultIDs)
			return
		}
		if rerr != nil {
			res.Errors++
			slog.Warn("purge-provenance: reset rows failed; leaving sidecar in place", "path", path, "error", rerr)
			return
		}
		res.ScanResultsReset += srReset
		res.WorkItemsRequeued += wqReset
		res.CacheInvalidated += invalidated
	} else {
		// No scan_results row claims this sidecar, so there is no cache key to
		// invalidate and nothing to requeue. If a later scan re-discovers the audio
		// file it will create a fresh row, and a surviving lyrics_cache entry for
		// that track could satisfy it as a hit -- leaving the sidecar unwritten.
		// Surfaced rather than silently assumed away.
		res.UnlinkedNoCacheKey++
		slog.Warn("purge-provenance: sidecar has no linked scan_results row; cannot invalidate its cache entry", "path", path)
	}

	if afterResetHook != nil {
		afterResetHook()
	}
	// stillUnmarked re-reads the marks just before an unlink. It narrows the
	// window in which a concurrent mark loses its file; it does not close it (a
	// mark landing between this read and the unlink still can).
	stillUnmarked := func() bool {
		if len(workItemIDs) == 0 {
			return true
		}
		marked, merr := markedRows(ctx, p.db, workItemIDs)
		if merr != nil {
			res.Errors++
			slog.Warn("purge-provenance: mark re-check failed; leaving sidecar", "path", path, "error", merr)
			return false
		}
		if len(marked) > 0 {
			res.SkippedManual++
			slog.Warn("purge-provenance: sidecar belongs to a manually marked instrumental; leaving it", "scan_result_ids", scanResultIDs)
			return false
		}
		return true
	}

	// Companion BEFORE the .lrc, the writer's rule: a failed companion removal
	// leaves the .lrc too, so the pair is never split. The re-fetch's writer
	// removes an owned companion before it writes, so the retry is safe.
	if companion != "" {
		if !stillUnmarked() {
			return
		}
		if rerr := removeFile(companion); rerr != nil && !os.IsNotExist(rerr) {
			res.Errors++
			slog.Warn("purge-provenance: companion delete failed; leaving the sidecar too", "path", companion, "error", rerr)
			return
		}
		res.CompanionsDeleted++
	}

	if !stillUnmarked() {
		return
	}
	if rerr := removeFile(path); rerr != nil {
		if !os.IsNotExist(rerr) {
			res.Errors++
			slog.Warn("purge-provenance: delete failed; rows already requeued, next scan will rewrite this sidecar", "path", path, "error", rerr)
			return
		}
		// Already gone (removed out-of-band since matching); the rows are reset
		// either way, which is what the caller wanted.
	}
	res.Deleted++
}

// restoreGenerated undoes one accepted aligner retiming (#1008): the .orig the
// first edit saved is renamed back over the .lrc, a generated companion is
// removed, and the row's edit mark is cleared. Nothing is re-fetched, so the
// row's status, scan_results and lyrics_cache are not touched. A file with no
// regular .orig is counted and left alone: deleting it would lose the lyric.
// A .orig is saved once and a re-fetch does not refresh it, so one that is not
// lyrics.SameLyric with the .lrc is counted and nothing is touched or recorded.
// Order under apply: backup record, edit mark, companion, rename. The mark
// goes first so a failure after it leaves the marker and the .orig for the
// next run to finish; the reverse could strand a mark on a restored file no
// selector finds again. The restore installs the .orig bytes SameLyric judged
// (never the .orig pathname) and then consumes the .orig if it is unchanged.
// On a --library run, a row found only by its audio path must be provably that
// library's, or the file is left alone (SkippedOtherLibrary).
func (p *Purger) restoreGenerated(ctx context.Context, path string, pt lyrics.ProvenanceTags, scanResultIDs, workItemIDs []int64, opts Options, res *Result) {
	orig := path + ".orig"
	fi, err := lstatFile(orig)
	if err != nil && !os.IsNotExist(err) {
		res.Errors++
		slog.Warn("purge-provenance: stat original failed; skipping", "path", orig, "error", err)
		return
	}
	if err != nil || !fi.Mode().IsRegular() {
		res.SkippedNoOriginal++
		return
	}
	// The .orig is read ONCE, through a no-follow handle fstat'ed regular, and
	// those bytes are both what SameLyric judges and what the restore installs:
	// an entry swapped in at the .orig name afterwards is never installed.
	origRaw, origFI, err := lyrics.ReadEditable(orig)
	var same, busy, outOfScope bool
	if err == nil {
		var raw []byte
		if raw, _, err = lyrics.ReadEditable(path); err == nil {
			same = lyrics.SameLyric(raw, origRaw)
		}
	}
	// No scan_results link: find the row by the audio beside the file (#1082).
	if err == nil && same && len(workItemIDs) == 0 {
		workItemIDs, busy, outOfScope, err = p.rowsBySource(ctx, path, opts.LibraryID)
	}
	if err != nil {
		res.Errors++
		slog.Warn("purge-provenance: comparing the original or resolving the queue row failed; skipping", "path", path, "error", err)
		return
	}
	if !same {
		res.SkippedOriginalDiffers++
		return
	}
	if outOfScope {
		// A --library run cannot show the row is that library's: leave the file
		// and the row for an unscoped run, rather than unmark another library's
		// row or restore the file and strand its mark.
		res.SkippedOtherLibrary++
		slog.Warn("purge-provenance: the row found by source path is not provably in the --library scope; skipping", "work_queue_ids", workItemIDs)
		return
	}
	if busy {
		res.SkippedProcessing++
		return
	}

	// Only a companion that is both owned and generated goes. Any other one
	// was not written by the accept, so it stays, and so does the row's tier.
	companion, keepTier := lyrics.OwnedCompanionOf(path), false
	recs := []Record{{Path: path, ScanResultIDs: scanResultIDs, WorkItemIDs: workItemIDs}}
	if companion != "" {
		if ct, cerr := lyrics.ReadProvenanceTags(companion); cerr != nil || ct.Timing != lyrics.TimingAligner {
			companion, keepTier = "", true
		} else {
			recs = append(recs, Record{Path: companion})
		}
	}
	// Backup-first, as for a delete: the caller's Report fsyncs the generated
	// bytes before anything changes, and a failure leaves the pair untouched.
	if opts.Report != nil {
		for _, rec := range recs {
			if rerr := opts.Report(rec); rerr != nil {
				res.Errors++
				slog.Warn("purge-provenance: report failed; leaving the generated file untouched", "path", rec.Path, "error", rerr)
				if !opts.DryRun {
					return
				}
			}
		}
	}
	if opts.DryRun {
		if len(workItemIDs) == 0 {
			res.RestoredNoRow++
		}
		return
	}

	if len(workItemIDs) > 0 {
		busy, cerr := p.clearEditMarks(ctx, workItemIDs, keepTier)
		if cerr != nil {
			res.Errors++
			slog.Warn("purge-provenance: clear edit mark failed; leaving the generated file in place", "path", path, "error", cerr)
			return
		}
		if busy {
			res.SkippedProcessing++
			return
		}
	}
	if companion != "" {
		if rerr := removeFile(companion); rerr != nil && !os.IsNotExist(rerr) {
			res.Errors++
			slog.Warn("purge-provenance: companion delete failed; leaving the generated file too", "path", companion, "error", rerr)
			return
		}
		res.CompanionsDeleted++
	}
	// Install the validated bytes through a fresh temp file renamed over the
	// .lrc, never the .orig pathname itself, so no entry that was not judged
	// (a symlink or other non-regular file swapped in since) is installed.
	if rerr := installFile(path, origRaw, origFI.Mode().Perm()); rerr != nil {
		res.Errors++
		slog.Warn("purge-provenance: restore failed; the edit mark is already cleared, rerun to finish", "path", path, "error", rerr)
		return
	}
	res.Restored++
	if len(workItemIDs) == 0 {
		res.RestoredNoRow++
	}
	// Consume the .orig only if it is still the file that was read: a swapped
	// entry is not ours to remove.
	if cur, lerr := lstatFile(orig); lerr != nil || !sameEntry(cur, origFI) {
		res.Errors++
		slog.Warn("purge-provenance: restored, but the original changed since it was read; leaving it", "path", orig, "error", lerr)
	} else if rerr := removeFile(orig); rerr != nil && !os.IsNotExist(rerr) {
		res.Errors++
		slog.Warn("purge-provenance: restored, but the original was not removed", "path", orig, "error", rerr)
	}
	// A .orig saved before the [re:canticle] (#483) or provenance backfill lacks tags the replaced
	// file had; put them back. Both helpers add absent keys only, and neither adds [upstream:].
	// A failure is an error (the restored file lacks a tag), though the restore stands.
	if _, ierr := injectEditorTag(path); ierr != nil {
		res.Errors++
		slog.Warn("purge-provenance: restored, but the editor tag was not re-added", "path", path, "error", ierr)
	}
	if _, _, ierr := injectProvenance(path, pt); ierr != nil {
		res.Errors++
		slog.Warn("purge-provenance: restored, but the provenance tags were not re-added", "path", path, "error", ierr)
	}
}

// sameEntry reports whether cur (an Lstat of the path now) is still the file
// read, whose handle FileInfo is read. os.SameFile alone is not enough: once
// the read file is deleted, the filesystem may hand its inode number to the
// entry created next (ext4 does, at once), so a symlink swapped in at the
// same name can match it. A non-regular entry is never the file read, and the
// size and mtime must also be the ones read.
func sameEntry(cur, read os.FileInfo) bool {
	return cur.Mode().IsRegular() && os.SameFile(cur, read) &&
		cur.Size() == read.Size() && cur.ModTime().Equal(read.ModTime())
}

// installFile atomically replaces path with data: a fresh exclusive temp file
// in path's directory is written, synced and given perm, then renamed over
// path (renameFile). The temp file is removed on any failure.
func installFile(path string, data []byte, perm os.FileMode) (retErr error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp") //nolint:gosec // reason: path is a sidecar found by the root walk; the temp file is created exclusive
	if err != nil {
		return fmt.Errorf("purgeprovenance: create restore temp: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if werr == nil {
		werr = tmp.Chmod(perm)
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("purgeprovenance: write restore temp: %w", werr)
	}
	return renameFile(tmp.Name(), path)
}

// rowsBySource returns the rows whose source_path is audio of lrc's stem. On a
// --library run (libraryID set) outOfScope reports that some found row is not
// provably that library's: it has no scan link to the library, or one to
// another. work_queue carries no library, so its scan links are the only proof.
func (p *Purger) rowsBySource(ctx context.Context, lrc string, libraryID *int64) (ids []int64, busy, outOfScope bool, retErr error) {
	var lib any // NULL: every row is in scope
	if libraryID != nil {
		lib = *libraryID
	}
	args := []any{lib, lib, lib}
	for _, sp := range revalidate.SiblingAudioPaths(lrc) {
		args = append(args, sp)
	}
	//nolint:gosec // reason: G202 - only "?" placeholders are built; every path is a bound parameter
	rows, err := p.db.QueryContext(ctx, `SELECT id, status,
	            ? IS NULL OR (
	              EXISTS (SELECT 1 FROM work_queue_scan_results j JOIN scan_results sr ON sr.id = j.scan_result_id
	                      WHERE j.work_queue_id = work_queue.id AND sr.library_id = ?)
	              AND NOT EXISTS (SELECT 1 FROM work_queue_scan_results j JOIN scan_results sr ON sr.id = j.scan_result_id
	                      WHERE j.work_queue_id = work_queue.id AND sr.library_id <> ?))
	         FROM work_queue WHERE source_path IN (?`+
		strings.Repeat(",?", len(args)-4)+`) ORDER BY id`, args...)
	if err != nil {
		return nil, false, false, fmt.Errorf("purgeprovenance: rows by source path: %w", err)
	}
	defer rows.Close() //nolint:errcheck // reason: read-only cursor; rows.Err() below reports any failure
	for rows.Next() {
		var id int64
		var status string
		var inScope bool
		if serr := rows.Scan(&id, &status, &inScope); serr != nil {
			return nil, false, false, fmt.Errorf("purgeprovenance: scan row by source path: %w", serr)
		}
		ids, busy, outOfScope = append(ids, id), busy || status == "processing", outOfScope || !inScope
	}
	return ids, busy, outOfScope, rows.Err()
}

// clearEditMarks forgets the edit mark on the rows of a file about to be
// restored and records the restored file's tier (line: the editor only ever
// retimes a line-tier file) unless keepTier. It re-reads each row's status in
// the transaction and reports busy, changing nothing, if a worker claimed one
// since the pre-walk index. Retried whole on SQLITE_BUSY like resetRows.
// The timing verdict may have judged the generated stamps (the serve sweep
// judges edited rows), so it is cleared in the same statement, with the columns
// queue.ApplyRemediated's reset clears, and ListTimingBacklog re-judges the file.
func (p *Purger) clearEditMarks(ctx context.Context, workItemIDs []int64, keepTier bool) (busy bool, retErr error) {
	retErr = dbpkg.RetryBatchTx(ctx, "purgeprovenance restore", func() error {
		busy = false
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("purgeprovenance: begin restore tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		for _, id := range workItemIDs {
			// The status guard is IN the UPDATE, so the row it changes is the
			// row it judged; no separate read can go stale before the write.
			ur, err := tx.ExecContext(ctx,
				`UPDATE work_queue
                 SET lyric_offset_ms = NULL,
                     lyric_edited_at = NULL,
                     timing_outcome = NULL, overrun_magnitude = NULL, overrun_ratio = NULL, evaluated_at = NULL,
                     timing_stamp_source = NULL, missync_recheck_generation = NULL,
                     sync_tier = CASE WHEN ? THEN sync_tier ELSE 'line' END
                 WHERE id = ? AND status <> 'processing'`,
				keepTier, id)
			if err != nil {
				return fmt.Errorf("purgeprovenance: clear edit mark %d: %w", id, err)
			}
			if n, err := ur.RowsAffected(); err != nil {
				return fmt.Errorf("purgeprovenance: clear edit mark %d: %w", id, err)
			} else if n > 0 {
				continue
			}
			// Nothing changed: the row is gone (it cannot be unmarked, nothing
			// to do) or in flight (busy: roll back, touch no file).
			var one int
			serr := tx.QueryRowContext(ctx, `SELECT 1 FROM work_queue WHERE id = ?`, id).Scan(&one)
			if errors.Is(serr, sql.ErrNoRows) {
				continue
			}
			if serr != nil {
				return fmt.Errorf("purgeprovenance: re-read work_queue %d: %w", id, serr)
			}
			busy = true
			return tx.Rollback()
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("purgeprovenance: commit restore tx: %w", err)
		}
		return nil
	})
	return busy, retErr
}

// resetRows resets the coupled scan_results and work_queue rows so the track
// re-fetches. Mirrors ResetInstrumental's reset shape (status='deferred',
// priority=-100 so the row is dequeue-eligible but strictly behind foreground
// work; next_attempt_at=now; last_error cleared): the same re-queue semantics
// scan reconcile already uses when it deletes a stale on-disk marker.
//
// Both updates guard on the row not already being in the target state /
// in-flight, so a race that claimed a row 'processing' between the caller's
// linkage check and this call leaves it untouched rather than corrupting an
// in-flight write -- the same residual TOCTOU window prune.deletePruned notes
// and accepts for the same reason (the alternative is holding a transaction
// open across the file delete, which this package deliberately does not do).
// errProvenanceChangedUnderfoot reports that a work_queue row's provider_lane
// stopped agreeing with the sidecar's [source:] tag between the pre-walk index
// build and the reset transaction. It aborts that sidecar's transaction, so the
// rows are not reset and the file is not deleted; the next run re-reads a
// consistent snapshot and either deletes it or refuses it as disputed.
// removeFile is the unlink seam, so a test can fail one delete of a pair.
var removeFile = os.Remove

var renameFile, lstatFile = os.Rename, os.Lstat // the restore's file seams

// The restore's tag seams, so a test can fail the re-add after a restore.
var injectEditorTag, injectProvenance = lyrics.InjectEditorTag, lyrics.InjectProvenance

var errProvenanceChangedUnderfoot = errors.New("purgeprovenance: provenance changed between index and delete")

// disputedLanes re-reads the given work_queue rows through the caller's
// transaction and returns the ids whose current provider_lane no longer agrees
// with tag. A row that has since vanished is not disputed: it cannot contradict
// anything, and the reset simply affects nothing.
func disputedLanes(ctx context.Context, tx *sql.Tx, workItemIDs []int64, tag string) ([]int64, error) {
	placeholders := make([]string, len(workItemIDs))
	args := make([]any, len(workItemIDs))
	for i, id := range workItemIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	//nolint:gosec // reason: G201 - the interpolated text is a generated "?,?" placeholder
	// list sized to workItemIDs; every id is bound as a parameter, never formatted in.
	q := "SELECT id, COALESCE(provider_lane, '') FROM work_queue WHERE id IN (" +
		strings.Join(placeholders, ",") + ")"
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("purgeprovenance: re-read provider lanes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // reason: read-only cursor; rows.Err() below reports any failure
	var disputed []int64
	for rows.Next() {
		var id int64
		var lane string
		if serr := rows.Scan(&id, &lane); serr != nil {
			return nil, fmt.Errorf("purgeprovenance: scan provider lane: %w", serr)
		}
		if !provenanceAgrees(tag, lane) {
			disputed = append(disputed, id)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("purgeprovenance: re-read provider lanes: %w", rerr)
	}
	return disputed, nil
}

// errMarkedUnderfoot reports that a work_queue row was marked instrumental by
// hand between the index build and the reset transaction. The sidecar is left
// alone and counted as skipped-manual, not as an error.
var errMarkedUnderfoot = errors.New("purgeprovenance: row marked instrumental since the index was built")

// rowQuerier is the read side of *sql.Tx and *sql.DB.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// afterResetHook, when non-nil, runs between the reset commit and the first
// unlink. Test seam only.
var afterResetHook func()

// markedRows re-reads the given rows inside tx and returns the ids now marked.
func markedRows(ctx context.Context, tx rowQuerier, workItemIDs []int64) ([]int64, error) {
	placeholders := make([]string, len(workItemIDs))
	args := make([]any, len(workItemIDs))
	for i, id := range workItemIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	//nolint:gosec // reason: G201 - the interpolated text is a generated "?,?" placeholder
	// list sized to workItemIDs; every id is bound as a parameter, never formatted in.
	q := "SELECT id FROM work_queue WHERE manual_instrumental_at IS NOT NULL AND id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("purgeprovenance: re-read manual marks: %w", err)
	}
	defer rows.Close() //nolint:errcheck // reason: read-only cursor; rows.Err() below reports any failure
	var marked []int64
	for rows.Next() {
		var id int64
		if serr := rows.Scan(&id); serr != nil {
			return nil, fmt.Errorf("purgeprovenance: scan manual mark: %w", serr)
		}
		marked = append(marked, id)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("purgeprovenance: re-read manual marks: %w", rerr)
	}
	return marked, nil
}

// resetRows retries its transaction whole on SQLITE_BUSY (#978). The sidecar's
// backup record was written by the caller BEFORE this runs and is written once
// regardless of how many attempts this takes, and nothing here touches the
// filesystem, so a rolled-back attempt leaves no trace to duplicate.
func (p *Purger) resetRows(ctx context.Context, scanResultIDs, workItemIDs []int64, identities []trackIdentity, tag string) (srReset, wqReset, invalidated int, retErr error) {
	retErr = dbpkg.RetryBatchTx(ctx, "purgeprovenance reset", func() error {
		var err error
		srReset, wqReset, invalidated, err = p.resetRowsOnce(ctx, scanResultIDs, workItemIDs, identities, tag)
		return err
	})
	if retErr != nil {
		return 0, 0, 0, retErr
	}
	return srReset, wqReset, invalidated, nil
}

// resetRowsOnce is one attempt of resetRows's transaction.
func (p *Purger) resetRowsOnce(ctx context.Context, scanResultIDs, workItemIDs []int64, identities []trackIdentity, tag string) (srReset, wqReset, invalidated int, retErr error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("purgeprovenance: begin reset tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-verify the provenance agreement INSIDE this transaction, before any
	// mutation in it (#827 follow-up). The lanes checked during the walk came
	// from buildIndex, which runs once BEFORE the filesystem walk -- so on a
	// large library that snapshot can be minutes stale by the time this sidecar
	// is reached, and a lane corrected in between would let a now-disputed file
	// be deleted on an agreeing value that no longer holds.
	//
	// Re-reading here rather than taking a process-wide lock: this transaction
	// is already the serialization point for every mutation the purge makes, so
	// a row that changes after this read cannot have been read by the rest of
	// the transaction either. A lock would also have to be held by a
	// provider-lane repair path, and no such path exists in the tree.
	if len(workItemIDs) > 0 {
		marked, merr := markedRows(ctx, tx, workItemIDs)
		if merr != nil {
			return 0, 0, 0, merr
		}
		if len(marked) > 0 {
			return 0, 0, 0, fmt.Errorf("%w: work_queue rows %v", errMarkedUnderfoot, marked)
		}
		disputed, verr := disputedLanes(ctx, tx, workItemIDs, tag)
		if verr != nil {
			return 0, 0, 0, verr
		}
		if len(disputed) > 0 {
			return 0, 0, 0, fmt.Errorf("%w: work_queue rows %v", errProvenanceChangedUnderfoot, disputed)
		}
	}

	// Cache invalidation rides in the SAME transaction as the row reset because
	// the two are one indivisible statement of intent: "this track's lyrics are
	// repudiated, fetch it again." A reset that commits without its invalidation
	// is precisely the data-loss defect (scan.Enqueuer.EnqueuePending consults
	// Cache.Lookup BEFORE enqueueing, marks the row done as a cache hit, and the
	// deleted sidecar is never rewritten), so they commit together or not at all.
	// cache.Invalidate removes every duration bucket for the key, which is what
	// makes this proof against Lookup's bucket-0 fallback.
	for _, id := range identities {
		n, ierr := cache.Invalidate(ctx, tx, id.artist, id.title)
		if ierr != nil {
			return 0, 0, 0, fmt.Errorf("purgeprovenance: invalidate cache: %w", ierr)
		}
		invalidated += n
	}

	now := formatNow()
	// The whole word-timing verdict (#982) is cleared: a 'queued' row must leave
	// recheck mode, or its settle would write scan_results back to done and the
	// purged track would never be re-fetched; and a served/absent verdict judged
	// the file being deleted, so keeping it would exclude the refetched lyric
	// from ever being rechecked. sync_tier (#1075) is cleared alongside it for
	// the same reason: it describes the .lrc this purge is about to delete, and
	// a row that later settles 'done' without a fresh completion (e.g.
	// prune.retireUnresolvable, source gone) must not keep reporting a tier for
	// a file that no longer exists.
	for _, id := range workItemIDs {
		res, err := tx.ExecContext(ctx,
			`UPDATE work_queue
             SET status = 'deferred',
                 priority = -100,
                 attempts = 0,
                 next_attempt_at = ?,
                 last_error = '',
                 word_timing_state = NULL,
                 word_timing_generation = NULL,
                 word_timing_checked_at = NULL,
                 sync_tier = NULL,
                 upgrade_checked_at = NULL,
                 upgrade_queued = 0
             WHERE id = ? AND status != 'processing'`,
			now, id)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("purgeprovenance: reset work_queue %d: %w", id, err)
		}
		wqReset += rowsAffected(res)
	}
	for _, id := range scanResultIDs {
		res, err := tx.ExecContext(ctx,
			`UPDATE scan_results SET status = 'pending' WHERE id = ? AND status != 'pending'`,
			id)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("purgeprovenance: reset scan_results %d: %w", id, err)
		}
		srReset += rowsAffected(res)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("purgeprovenance: commit reset tx: %w", err)
	}
	return srReset, wqReset, invalidated, nil
}

// buildIndex loads every in-scope scan_results row's expected-sidecar identity
// (keyed by canonical output directory + filename stem, extension-agnostic so a
// .lrc and a .txt sidecar for the same track both resolve to the same
// scan_results row) plus the work_queue rows linked to each, so per-file
// database lookups during the walk are pure in-memory map reads rather than a
// query per sidecar.
func (p *Purger) buildIndex(ctx context.Context, libraryID *int64) (map[string][]srInfo, map[int64][]wqLink, error) {
	idx := make(map[string][]srInfo)
	query := `SELECT id, outdir, filename, artist, title FROM scan_results WHERE filename != ''`
	var args []any
	if libraryID != nil {
		query += ` AND library_id = ?`
		args = append(args, *libraryID)
	}
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("purgeprovenance: query scan_results: %w", err)
	}
	srIDs := make(map[int64]bool)
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var outdir, filename, artist, title string
			if serr := rows.Scan(&id, &outdir, &filename, &artist, &title); serr != nil {
				err = fmt.Errorf("purgeprovenance: scan scan_results row: %w", serr)
				return
			}
			stem := strings.TrimSuffix(filename, filepath.Ext(filename))
			key := canonicalDir(outdir) + "\x00" + stem
			idx[key] = append(idx[key], srInfo{id: id, artist: artist, title: title})
			srIDs[id] = true
		}
		if rerr := rows.Err(); rerr != nil {
			err = fmt.Errorf("purgeprovenance: iterate scan_results: %w", rerr)
		}
	}()
	if err != nil {
		return nil, nil, err
	}

	// Push the --library scope into SQL rather than fetching every link row and
	// discarding the out-of-scope ones in Go. The predicate mirrors the
	// scan_results query above exactly, so the in-scope set is identical; the
	// srIDs guard below stays as a belt-and-braces check that also covers a
	// link row whose scan_results row has no filename.
	// COALESCE the nullable provider_lane to '' rather than scanning into a
	// *string: an empty lane and a NULL lane mean the same thing to
	// provenanceAgrees ("this row asserts no provider"), so collapsing them at
	// the boundary keeps the guard from having to distinguish two spellings of
	// nothing.
	wqQuery := `SELECT j.scan_result_id, wq.id, wq.status, COALESCE(wq.provider_lane, ''), wq.manual_instrumental_at IS NOT NULL
         FROM work_queue_scan_results j
         JOIN work_queue wq ON wq.id = j.work_queue_id`
	var wqArgs []any
	if libraryID != nil {
		wqQuery += `
         JOIN scan_results sr ON sr.id = j.scan_result_id
         WHERE sr.filename != '' AND sr.library_id = ?`
		wqArgs = append(wqArgs, *libraryID)
	}
	wqIdx := make(map[int64][]wqLink)
	wqRows, err := p.db.QueryContext(ctx, wqQuery, wqArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("purgeprovenance: query work_queue links: %w", err)
	}
	defer func() { _ = wqRows.Close() }()
	for wqRows.Next() {
		var srID, wqID int64
		var status, lane string
		var manual bool
		if serr := wqRows.Scan(&srID, &wqID, &status, &lane, &manual); serr != nil {
			return nil, nil, fmt.Errorf("purgeprovenance: scan work_queue link: %w", serr)
		}
		if !srIDs[srID] {
			// Not an in-scope scan_results row (e.g. excluded by --library); skip
			// to keep the index scoped to what buildIndex actually loaded.
			continue
		}
		wqIdx[srID] = append(wqIdx[srID], wqLink{id: wqID, status: status, lane: lane, manual: manual})
	}
	if rerr := wqRows.Err(); rerr != nil {
		return nil, nil, fmt.Errorf("purgeprovenance: iterate work_queue links: %w", rerr)
	}
	return idx, wqIdx, nil
}

// canonicalDir returns a canonical form of a directory path for comparison:
// made absolute (relative to the current working directory) then Cleaned.
// Symlinks are deliberately NOT resolved -- matching provenance.go's
// canonicalDir, which this mirrors -- so a stored outdir that no longer exists
// (or a symlinked component) still compares consistently rather than erroring.
func canonicalDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func rowsAffected(res sql.Result) int {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}
