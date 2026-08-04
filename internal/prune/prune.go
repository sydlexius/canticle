// Package prune reconciles the durable work queue and scan-result cache against
// the filesystem: rows whose source audio file no longer exists on disk are
// deleted so a renamed/merged/deleted track cannot leave a permanently-failing
// or wedged row behind (#453).
//
// The filesystem is the sole authority for "gone": a source path is a
// candidate for removal only when os.Stat of that path (Exact granularity) or
// its directory (Directory granularity) fails. The same primitive backs three
// callers -- a watcher-reactive prune on Remove/Rename events, a lazy periodic
// sweep, and the `scan reconcile-paths` CLI -- so the reconciliation rule
// lives in exactly one place.
//
// A gone row is no longer deleted unconditionally (#640): before deleting,
// reconcile consults the row's stored MBID/ISRC (populated by the scan loop
// into scan_results.isrc/recording_mbid, migration 037) against every OTHER
// still-present file in the library via the shared internal/identity exact
// tier -- the SAME resolver internal/realign uses to re-attach orphaned lyric
// sidecars, so a bulk move can never leave the sidecar pointing at one file and
// the database row pointing at another. A unique match RE-LINKS the row to its
// new path, preserving every telemetry/timing/provenance column the row
// carries; identity that is absent or ambiguous is never guessed at -- the row
// is KEPT and reported, because a wrongly-kept row is inert while a
// wrongly-deleted one destroys a GPU-class inference result. Only a row whose
// identity is present but matches nothing anywhere in the library is a genuine
// delete, and the reactive PrunePath path never performs one at all (relink or
// retain only), leaving genuine deletion to the periodic sweep and the CLI, by
// which time a rescan has had time to settle.
//
// A retained row is additionally RETIRED when it is provably unactionable --
// gone AND carrying no identity, so no future sweep could ever relink it (#732).
// Retiring deletes nothing and preserves every telemetry column; it only settles
// the work_queue row so the worker stops re-fetching lyrics it can never write.
// Before this, such a row stayed dequeue-eligible forever and each sweep
// re-classified it identically, a fixed point that never converged. Retirement
// is a mutation, so it obeys the same discipline as a genuine delete: never in a
// dry run, never on an in-flight row, and only under PolicyFull -- the reactive
// pass defers it to the periodic sweep, by which time a rescan of a moved file's
// new location has had time to re-create the row with identity.
package prune

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sydlexius/canticle/internal/identity"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
)

// timeFormat is the timestamp layout every stored column uses. Declared locally
// rather than shared, matching internal/queue and internal/reports, which each
// carry their own copy of the same constant.
const timeFormat = time.RFC3339

// Granularity selects how a candidate source path is tested for existence.
type Granularity int

const (
	// Directory stats only the parent directory of each candidate source path.
	// A source is considered gone when its directory is gone. This is the cheap
	// strategy for the unattended periodic sweep (one stat per directory rather
	// than per file), matching the ticket's disk-I/O constraint. Single-file
	// renames within a surviving directory are NOT caught at this granularity.
	Directory Granularity = iota
	// Exact stats every candidate source path individually, so a single-file
	// rename within a still-existing directory is caught. Used by the reactive
	// prune and the operator-invoked CLI.
	Exact
)

// Policy controls whether reconcile is allowed to perform a genuine delete
// once a gone row's identity is present but resolves to no candidate anywhere
// in the library.
type Policy int

const (
	// PolicyFull allows every outcome: relink, retain, or genuine delete. Used
	// by the periodic sweep and the CLI, both of which run well after any
	// rescan of a moved file's new location would have completed.
	PolicyFull Policy = iota
	// PolicyRelinkOrRetain never performs a genuine delete: a gone row with
	// identity that resolves to nothing is retained-and-reported instead, the
	// same as an ambiguous or absent identity. Used by the reactive PrunePath,
	// which fires the instant a filesystem event reports a path gone -- before
	// a corresponding rescan of the file's new location could plausibly have
	// populated the present-file candidate pool, so a "no match anywhere"
	// verdict at that moment cannot be trusted to mean "genuinely deleted".
	PolicyRelinkOrRetain
)

// PrunedRow describes one gone source path and the rows removed for it, for
// summaries and JSONL backups.
type PrunedRow struct {
	// SourcePath is the vanished audio file path (scan_results.file_path /
	// work_queue.source_path).
	SourcePath string
	// ScanResultIDs are the scan_results rows removed for this source.
	ScanResultIDs []int64
	// WorkItemIDs are the work_queue rows removed for this source.
	WorkItemIDs []int64
	// Inputs carries the removed work_queue rows' restorable payload for backup.
	Inputs []models.Inputs
}

// RelinkedRow describes one gone source path whose identity resolved uniquely
// to a present file elsewhere in the library: the linked work_queue row(s) had
// their source_path/outdir/filename updated to the new location, and the
// stale scan_results row was removed -- every telemetry/timing/provenance
// column on the work_queue row is left untouched.
type RelinkedRow struct {
	OldPath       string
	NewPath       string
	ScanResultIDs []int64
	WorkItemIDs   []int64
	// MBID/ISRC are whichever identity values the gone row carried and that
	// drove the match (both may be set; only one may have been the one an
	// operator cares to inspect).
	MBID string
	ISRC string
}

// RetainedRow describes one gone source path that was deliberately NOT
// deleted: its identity was absent, ambiguous (matched more than one present
// file), or -- under PolicyRelinkOrRetain -- present but as-yet unresolved.
// Never guessed at; a wrongly-kept row is inert, so the safe default is to
// keep it and report why.
type RetainedRow struct {
	SourcePath string
	Reason     string
	MBID       string
	ISRC       string
	// Retired reports that this row was taken out of the dequeue-eligible set as
	// well as retained. It is still a RetainedRow -- nothing was deleted and the
	// operator-facing accounting is unchanged -- but the worker will no longer
	// attempt it. See retireUnresolvable for when this is set.
	Retired bool
}

// unresolvableGoneError is the last_error value written when a row is retired as
// permanently unactionable: its source file is gone AND it carries no identity,
// so no relink can ever resolve it. Defined once so the SQL bind, the tests, and
// any log or inspection of the sentinel cannot drift -- the same discipline as
// queue.missLimitReachedError, whose retire-to-'done' pattern this mirrors.
//
// The text is written for someone reading it six months from now with no context:
// it states the mechanism, not a verdict.
const unresolvableGoneError = "source file is gone and the row carries no ISRC or MBID, so it can never be relinked; retired as unactionable"

// Result reports the totals and per-row detail of a prune (or, in dry-run, what
// would happen).
type Result struct {
	ScanResults int
	WorkItems   int
	Pruned      []PrunedRow
	Relinked    []RelinkedRow
	Retained    []RetainedRow
}

// SweepOptions controls a whole-scope reconciliation sweep.
type SweepOptions struct {
	// LibraryID, when non-nil, restricts candidates to that library's scan_results
	// (and their linked work_queue rows). Nil sweeps every configured library and
	// also catches link-less work_queue rows by source_path.
	LibraryID *int64
	// Granularity selects the directory-cheap or exact stat strategy.
	Granularity Granularity
	// DryRun computes and reports the prune set without mutating the database.
	DryRun bool
	// Report, when set, is invoked once per pruned source path AFTER the delete
	// commits, so a caller can back up or log each row -- and only for a row the
	// deletes actually removed, so a backup record never describes a row that a
	// race into 'processing' skipped or that a rollback left in place. In
	// dry-run it fires for every row that would be pruned.
	Report func(PrunedRow) error
	// ReportRelinked, when set, is invoked once per relinked source path AFTER
	// the update commits (or, in dry-run, for the row that would be relinked),
	// mirroring Report's discipline: if the relink transaction rolls back, no
	// report fires for anything in that batch.
	ReportRelinked func(RelinkedRow) error
	// ReportRetained, when set, is invoked once per retained source path.
	// Retaining never mutates the database, so this fires the same way in
	// dry-run and real runs.
	ReportRetained func(RetainedRow) error
}

// Pruner reconciles work_queue and scan_results against the filesystem.
type Pruner struct {
	db           *sql.DB
	identityKeys identity.Keys
}

// New returns a Pruner backed by db, with the default identity-key order
// (mbid, then isrc) for the exact-match re-link tier. Call SetIdentityKeys to
// honor an operator's configured config.RealignConfig.IdentityKeys order --
// the same config realign reads, so the two subsystems can never disagree
// about key precedence.
func New(db *sql.DB) *Pruner {
	return &Pruner{db: db, identityKeys: identity.NormalizeKeys([]string{"mbid", "isrc"})}
}

// SetIdentityKeys overrides the identity-key order the exact-match re-link
// tier consults, most authoritative first. Pass config.RealignConfig.IdentityKeys.
// An empty or all-unknown result from identity.NormalizeKeys is ignored (the
// existing order is kept) so a caller's misconfiguration cannot silently
// disable re-link matching altogether.
func (p *Pruner) SetIdentityKeys(keys []string) {
	if normalized := identity.NormalizeKeys(keys); len(normalized) > 0 {
		p.identityKeys = normalized
	}
}

// PrunePath reconciles the rows whose source path is at or under path, statting
// each candidate source file individually (Exact granularity). It is the
// reactive entry point: the caller already learned path vanished from a
// filesystem event, so this does no rescan -- only per-candidate os.Stat checks
// plus the DB writes (disk-cheap, not a directory walk). A removed directory is
// handled naturally -- every candidate source under it fails os.Stat.
//
// Runs under PolicyRelinkOrRetain: a gone row here is either relinked (its
// identity resolves uniquely to an already-present file) or retained, never
// genuinely deleted -- see PolicyRelinkOrRetain's doc for why.
func (p *Pruner) PrunePath(ctx context.Context, path string) (Result, error) {
	return p.reconcile(ctx, scope{prefix: path, scoped: true}, nil, Exact, false, PolicyRelinkOrRetain, reportHooks{})
}

// Sweep reconciles every candidate source path in scope. Directory granularity
// is the cheap backstop; Exact is the thorough operator-invoked pass. With
// DryRun set it reports without mutating. Runs under PolicyFull: a gone row
// whose identity is present but resolves to no candidate anywhere in the
// library is genuinely deleted, since the periodic sweep and the CLI both run
// well after any rescan of a moved file's new location would have settled.
func (p *Pruner) Sweep(ctx context.Context, opts SweepOptions) (Result, error) {
	return p.reconcile(ctx, scope{}, opts.LibraryID, opts.Granularity, opts.DryRun, PolicyFull, reportHooks{
		Prune:    opts.Report,
		Relinked: opts.ReportRelinked,
		Retained: opts.ReportRetained,
	})
}

// reportHooks bundles the three optional per-outcome callbacks so internal
// plumbing has one parameter instead of three.
type reportHooks struct {
	Prune    func(PrunedRow) error
	Relinked func(RelinkedRow) error
	Retained func(RetainedRow) error
}

// scope narrows candidate source paths to those at or under prefix. A zero scope
// (scoped=false) matches every candidate.
type scope struct {
	prefix string
	scoped bool
}

func (s scope) matches(p string) bool {
	if !s.scoped {
		return true
	}
	return p == s.prefix || pathutil.WithinRoot(s.prefix, p)
}

// childRange returns the half-open key range [lower, upper) that contains every
// path strictly under s.prefix (i.e. "<prefix>/..."). Because the byte after the
// path separator is its successor, all such strings sort within this range, so a
// `col >= lower AND col < upper` predicate can seek an index on the column
// instead of scanning every row. pathutil.WithinRoot remains the exact authority
// applied in Go, so this range only narrows what the database returns.
func (s scope) childRange() (lower, upper string) {
	sep := string(filepath.Separator)
	return s.prefix + sep, s.prefix + string(filepath.Separator+1)
}

// candidate is a source path with the row detail needed to prune, relink, or
// back it up.
type candidate struct {
	scanResultIDs []int64
	workItems     []workRow
	// processing is true when any linked work_queue row is still 'processing',
	// so the whole source is deferred (the worker owns it) to avoid a half-prune.
	processing bool
	// settled is true when every linked work_queue row has reached 'done' -- it is
	// no longer work, so there is nothing to retire. Distinct from processing: a
	// settled row is still a valid relink and prune target, it just must not be
	// re-retired and re-reported on every sweep.
	settled bool
	// libraryID scopes the present-file candidate pool this row's identity is
	// matched against. Known whenever a scan_results row backs this source;
	// nil for a link-less work_queue-only candidate (no scan_results row at
	// all), which falls back to an unscoped, whole-database search.
	libraryID *int64
	// mbid/isrc are this row's own stored identity, preferring
	// scan_results.recording_mbid/isrc (re-read on every scan, so it can never
	// be stale for a since-deleted file) and falling back to
	// work_queue.mbid/isrc (migration 033, the provider's resolved identity at
	// fetch time) only when scan_results carries none.
	mbid, isrc string
}

// workRow is a work_queue row's restorable detail.
type workRow struct {
	id     int64
	inputs models.Inputs
}

// reconcile is the shared core behind PrunePath and Sweep. It gathers candidate
// source paths, uses os.Stat (per granularity) as the sole authority for gone,
// applies the in-flight guard, classifies each gone candidate via the shared
// identity resolver into prune/relink/retain, and applies the result --
// deleting genuinely-gone rows, relinking rows whose identity resolved
// elsewhere, and reporting (never mutating) retained rows.
func (p *Pruner) reconcile(ctx context.Context, sc scope, libraryID *int64, g Granularity, dryRun bool, policy Policy, hooks reportHooks) (Result, error) {
	bySource, err := p.gatherCandidates(ctx, sc, libraryID)
	if err != nil {
		return Result{}, err
	}
	// Load the set of library roots that currently exist on disk. A source is
	// only ever judged "gone" when it sits under an available root, so an entire
	// library that is merely unmounted (its mountpoint present but empty, making
	// every child os.Stat return ENOENT) cannot be mass-deleted. A root that is
	// genuinely removed is left to `library remove`, not this reconciler.
	roots, err := p.availableRoots(ctx)
	if err != nil {
		return Result{}, err
	}
	idx := newPresentIndex(p.db, roots)

	statCache := make(map[string]bool) // directory -> exists (Directory granularity)
	var res Result
	var toPrune []PrunedRow
	var toRelink []classifiedRelink
	for _, src := range sortedKeys(bySource) {
		c := bySource[src]
		if !underAvailableRoot(src, roots) {
			continue
		}
		if !gone(src, g, statCache) {
			continue
		}
		if c.processing {
			// The worker still owns this source; deleting its scan_results row now
			// would null work_queue.scan_result_id (migration 009, ON DELETE SET
			// NULL) and cascade away the junction row mid-flight. Defer to a later
			// pass once the worker finishes or fails the item.
			continue
		}

		cg, err := p.classify(ctx, idx, policy, src, c)
		if err != nil {
			return Result{}, err
		}
		switch cg.outcome {
		case outcomeSettled:
			// Nothing to do and nothing to say: this candidate is already in its
			// terminal state. Deliberately absent from every Result slice, so a
			// repeat sweep over a settled library reports an empty result rather
			// than re-listing rows no action will ever touch again.
			continue
		case outcomeRetain:
			// Retire before reporting, so a report never describes a retirement that
			// did not commit -- the same backup-first discipline deletePruned follows.
			// A dry run retires nothing: the CLI's dry-run default is load-bearing, and
			// a preview that mutates is worse than no preview.
			if cg.retained.Retired && !dryRun {
				retired, err := p.retireUnresolvable(ctx, cg.c)
				if err != nil {
					return Result{}, fmt.Errorf("prune: retire %q: %w", src, err)
				}
				// An in-flight row is skipped by the UPDATE's status guard. Report it as
				// the plain retain it actually was, rather than claiming a retirement the
				// database declined to make.
				cg.retained.Retired = retired
			}
			res.Retained = append(res.Retained, cg.retained)
			if hooks.Retained != nil {
				if err := hooks.Retained(cg.retained); err != nil {
					return Result{}, fmt.Errorf("prune: report retained %q: %w", src, err)
				}
			}
		case outcomeRelink:
			toRelink = append(toRelink, cg.classifiedRelink)
		case outcomePrune:
			row := PrunedRow{SourcePath: src, ScanResultIDs: c.scanResultIDs}
			for _, w := range c.workItems {
				row.WorkItemIDs = append(row.WorkItemIDs, w.id)
				row.Inputs = append(row.Inputs, w.inputs)
			}
			toPrune = append(toPrune, row)
		}
	}
	res.Pruned = toPrune
	for _, cg := range toRelink {
		res.Relinked = append(res.Relinked, cg.relinked)
	}

	if dryRun {
		// Dry-run reports the intended outcome (gather-time counts), since no
		// mutation runs to measure.
		for _, row := range toPrune {
			res.ScanResults += len(row.ScanResultIDs)
			res.WorkItems += len(row.WorkItemIDs)
			if hooks.Prune != nil {
				if err := hooks.Prune(row); err != nil {
					return Result{}, fmt.Errorf("prune: report: %w", err)
				}
			}
		}
		for _, cg := range toRelink {
			if hooks.Relinked != nil {
				if err := hooks.Relinked(cg.relinked); err != nil {
					return Result{}, fmt.Errorf("prune: report relinked %q: %w", cg.relinked.OldPath, err)
				}
			}
		}
		return res, nil
	}

	if len(toRelink) > 0 {
		applied, retainedByConflict, err := p.applyRelinks(ctx, toRelink, hooks.Relinked, hooks.Retained)
		if err != nil {
			return Result{}, err
		}
		res.Relinked = applied
		// A candidate that failed to relink (its target is already owned by a
		// different work_queue row) is neither pruned nor relinked, but it must
		// still be accounted for -- appending here, not overwriting, keeps it
		// alongside any rows classify() already retained directly (absent or
		// ambiguous identity) so pruned+relinked+retained always equals the
		// number of gone rows considered.
		res.Retained = append(res.Retained, retainedByConflict...)
	}
	if len(toPrune) == 0 {
		return res, nil
	}
	scanDeleted, workDeleted, err := p.deletePruned(ctx, toPrune, hooks.Prune)
	if err != nil {
		return Result{}, err
	}
	// Report actual deletions, not the intended set, so a row that raced into
	// 'processing' (and was therefore skipped) is not counted as pruned.
	res.ScanResults = scanDeleted
	res.WorkItems = workDeleted
	return res, nil
}

// outcome classifies what reconcile decided for one gone candidate.
type outcome int

const (
	outcomePrune outcome = iota
	outcomeRelink
	outcomeRetain
	// outcomeSettled is a candidate there is nothing left to do about: gone,
	// unresolvable, and already out of the dequeue-eligible set. It is reported
	// nowhere and mutates nothing, which is what lets a sweep CONVERGE -- see the
	// settled field on candidate.
	outcomeSettled
)

// classifiedRelink carries both the reporting-facing RelinkedRow and the
// internal detail applyRelinks needs to actually perform the update.
type classifiedRelink struct {
	src      string
	c        *candidate
	relinked RelinkedRow
	target   presentRowDetail
}

type classified struct {
	outcome  outcome
	retained RetainedRow
	classifiedRelink
}

// classify decides one gone candidate's fate: retain (identity absent,
// ambiguous, or -- under PolicyRelinkOrRetain -- unresolved), relink (identity
// resolves uniquely to an already-present file), or prune (identity present
// but matches nothing anywhere in the library, and policy allows a genuine
// delete).
func (p *Pruner) classify(ctx context.Context, idx *presentIndex, policy Policy, src string, c *candidate) (classified, error) {
	if c.mbid == "" && c.isrc == "" {
		// Already retired by an earlier sweep (or never queued at all): gone,
		// unresolvable, and no longer work. Re-reporting it every sweep is exactly
		// the churn #732 exists to stop, so it drops out of the candidate set here.
		if c.settled && len(c.workItems) > 0 {
			return classified{outcome: outcomeSettled}, nil
		}
		// Gone, and carrying no identity: no relink can ever resolve this row, at any
		// future sweep, because there is nothing to resolve it BY. Keeping it is still
		// right -- deleting on a guess is what #640 exists to prevent -- but leaving it
		// dequeue-eligible made the worker re-fetch lyrics it could never write, every
		// backoff period, forever (#732).
		//
		// So: retain AND retire. Nothing is deleted, every telemetry column survives,
		// and the row simply stops being work. Retirement is a mutation, so it follows
		// the same policy discipline as a genuine delete -- the reactive pass defers it
		// to the periodic sweep, by which time a rescan of a moved file's new location
		// has had time to settle and re-create the row with identity.
		return classified{
			outcome: outcomeRetain,
			retained: RetainedRow{
				SourcePath: src,
				Reason:     "identity absent; never deleted on a guess",
				// Only a row that is still WORK can be retired. A settled row either was
				// never queued or has already been retired by a previous sweep; either
				// way there is nothing to take out of the dequeue-eligible set.
				Retired: policy == PolicyFull && !c.settled && len(c.workItems) > 0,
			},
			// Carry the candidate so the retire step can reach its work_queue rows.
			// The embedded classifiedRelink is otherwise only populated on the relink
			// path, and a nil c here made retireUnresolvable a silent no-op.
			classifiedRelink: classifiedRelink{src: src, c: c},
		}, nil
	}
	pool, err := idx.pool(ctx, c.libraryID)
	if err != nil {
		return classified{}, err
	}
	verdict, ref := identity.ResolveExact(c.mbid, c.isrc, p.identityKeys, pool)
	switch verdict {
	case identity.VerdictUnique:
		detail := idx.detail(c.libraryID, ref)
		rr := RelinkedRow{OldPath: src, NewPath: ref, ScanResultIDs: c.scanResultIDs, MBID: c.mbid, ISRC: c.isrc}
		for _, w := range c.workItems {
			rr.WorkItemIDs = append(rr.WorkItemIDs, w.id)
		}
		return classified{outcome: outcomeRelink, classifiedRelink: classifiedRelink{src: src, c: c, relinked: rr, target: detail}}, nil
	case identity.VerdictConflict:
		return classified{outcome: outcomeRetain, retained: RetainedRow{SourcePath: src, Reason: "identity matches more than one present file; never guessed", MBID: c.mbid, ISRC: c.isrc}}, nil
	default: // VerdictNone
		if policy == PolicyRelinkOrRetain {
			return classified{outcome: outcomeRetain, retained: RetainedRow{SourcePath: src, Reason: "identity present but not yet found elsewhere; reactive pass defers genuine delete to the periodic sweep", MBID: c.mbid, ISRC: c.isrc}}, nil
		}
		return classified{outcome: outcomePrune}, nil
	}
}

// applyRelinks performs every relink in one transaction, updating the
// surviving work_queue row(s)' source_path/outdir/filename to the resolved new
// location, ensuring the work_queue_scan_results junction points at the
// present-file scan_results row, and deleting only the stale (gone-path)
// scan_results row -- never the work_queue row itself, so every
// telemetry/timing/provenance column already on it survives untouched.
//
// A candidate whose present-file target is already junction-linked to a
// DIFFERENT work_queue row is not silently skipped: it is accounted for as a
// RetainedRow (see relinkOne), because #640's whole premise is keep-and-report,
// never decide silently -- a row this package declines to touch must still
// show up in the reconcile Result and the operator-facing summary/backup, the
// same as an absent or ambiguous identity would.
//
// Both reportRelinked and reportRetained fire once per outcome, after the
// commit, mirroring deletePruned's backup-first-on-report-time discipline: if
// any relink in the batch errors, the whole transaction rolls back and NO
// report fires for anything in this call, applied or retained-by-conflict
// alike, so a report is never written for a row a rollback left untouched by
// a different mechanism than it claims.
func (p *Pruner) applyRelinks(ctx context.Context, targets []classifiedRelink, reportRelinked func(RelinkedRow) error, reportRetained func(RetainedRow) error) (applied []RelinkedRow, retained []RetainedRow, retErr error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("prune: begin relink tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, cg := range targets {
		// Each candidate runs inside its own savepoint so a decline part-way
		// through a multi-row candidate (the in-flight guard rejecting the
		// second of two work items) unwinds that candidate's earlier writes
		// instead of committing a half-relink. Savepoint names are generated,
		// never caller-derived, so no identifier can be injected.
		sp := fmt.Sprintf("prune_relink_%d", i)
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+sp); err != nil { //nolint:gosec // reason: sp is a fixed prefix plus a loop index, never external input
			return nil, nil, fmt.Errorf("prune: savepoint relink: %w", err)
		}
		decision, err := relinkOne(ctx, tx, cg.c, cg.target)
		if err != nil {
			return nil, nil, err
		}
		if decision.reason != "" {
			// This candidate is declined: either the present-file scan_results
			// row is already owned by a DIFFERENT work_queue row (merging two
			// work_queue rows is internal/identityrepair's job, not prune's), or
			// a work item raced into 'processing' so its relink UPDATE matched
			// nothing. Either way the row still needs an outcome an operator can
			// see, not a bare drop from every count -- and its partial writes are
			// rolled back to the savepoint first, so the reported "retained" is
			// literally true of the database.
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO "+sp); err != nil { //nolint:gosec // reason: sp is a fixed prefix plus a loop index, never external input
				return nil, nil, fmt.Errorf("prune: rollback relink savepoint: %w", err)
			}
			retained = append(retained, RetainedRow{
				SourcePath: cg.relinked.OldPath,
				Reason:     decision.reason,
				MBID:       cg.relinked.MBID,
				ISRC:       cg.relinked.ISRC,
			})
		} else {
			applied = append(applied, cg.relinked)
		}
		if _, err := tx.ExecContext(ctx, "RELEASE "+sp); err != nil { //nolint:gosec // reason: sp is a fixed prefix plus a loop index, never external input
			return nil, nil, fmt.Errorf("prune: release relink savepoint: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("prune: commit relink tx: %w", err)
	}
	if reportRelinked != nil {
		for _, rr := range applied {
			if err := reportRelinked(rr); err != nil {
				return applied, retained, fmt.Errorf("prune: report relinked %q: %w", rr.OldPath, err)
			}
		}
	}
	if reportRetained != nil {
		for _, rr := range retained {
			if err := reportRetained(rr); err != nil {
				return applied, retained, fmt.Errorf("prune: report retained %q: %w", rr.SourcePath, err)
			}
		}
	}
	return applied, retained, nil
}

// relinkDecision is relinkOne's verdict for one candidate. A zero value means
// the relink was performed; a non-empty reason means it was declined and the
// caller must roll the candidate back and report it as retained.
type relinkDecision struct {
	reason string
}

// relinkOne performs one candidate's relink within tx, returning a non-empty
// decision reason (and leaving the caller to roll back to its savepoint) in
// either of two keep-and-report cases.
//
// FIRST, an ownership conflict: the present-file scan_results row is already
// junction-linked to a work_queue row other than this candidate's own -- an
// identity conflict this package declines to auto-merge, because merging two
// work_queue rows is internal/identityrepair's job.
//
// "This candidate's own" means EVERY work_queue row in c.workItems, not just
// the one being examined. gatherCandidates aggregates every work_queue row
// sharing a source_path into ONE candidate (source_path carries no uniqueness
// constraint -- migration 026 only indexes it), so a candidate routinely holds
// several rows. Excluding only the current row would let a SIBLING of the same
// candidate that already owns the target read as an ownership conflict, and the
// row would then be retained forever on a conflict with itself.
//
// SECOND, a lost in-flight race: the relink UPDATE is guarded on
// `status != 'processing'`, so a work item that raced into 'processing' between
// gatherCandidates and this transaction updates NOTHING. Proceeding past a
// zero-row UPDATE would junction-link and then DELETE the stale scan_results row
// while the work_queue row still pointed at the vanished path -- silent data
// loss reported as a successful relink, exactly the outcome #640 exists to
// prevent. The candidate is declined whole instead.
func relinkOne(ctx context.Context, tx *sql.Tx, c *candidate, target presentRowDetail) (relinkDecision, error) {
	own := make(map[int64]bool, len(c.workItems))
	for _, w := range c.workItems {
		own[w.id] = true
	}
	if len(own) > 0 {
		otherID, found, err := foreignOwner(ctx, tx, target.scanResultID, own)
		if err != nil {
			return relinkDecision{}, err
		}
		if found {
			return relinkDecision{reason: fmt.Sprintf("present-file candidate already linked to work_queue row %d; merging is identityrepair's job", otherID)}, nil
		}
	}
	for _, w := range c.workItems {
		res, err := tx.ExecContext(ctx,
			`UPDATE work_queue SET source_path = ?, outdir = ?, filename = ? WHERE id = ? AND status != 'processing'`,
			target.filePath, target.outdir, target.filename, w.id)
		if err != nil {
			return relinkDecision{}, fmt.Errorf("prune: relink work_queue %d: %w", w.id, err)
		}
		if rowsAffected(res) == 0 {
			// The row raced into 'processing' (or vanished) after gather. Decline
			// the whole candidate rather than deleting its scan_results row out
			// from under a work_queue row still pointing at the gone path.
			return relinkDecision{reason: fmt.Sprintf("work_queue row %d became in-flight (or vanished) before the relink could apply; kept for a later pass", w.id)}, nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
			w.id, target.scanResultID); err != nil {
			return relinkDecision{}, fmt.Errorf("prune: link work_queue %d to scan_result %d: %w", w.id, target.scanResultID, err)
		}
	}
	for _, id := range c.scanResultIDs {
		// Same in-flight guard as deletePruned: never delete a scan_results row
		// still linked to a processing work_queue row.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM scan_results WHERE id = ?
             AND NOT EXISTS (
                 SELECT 1 FROM work_queue_scan_results j
                 JOIN work_queue wq ON wq.id = j.work_queue_id
                 WHERE j.scan_result_id = ? AND wq.status = 'processing')`,
			id, id); err != nil {
			return relinkDecision{}, fmt.Errorf("prune: delete stale scan_result %d: %w", id, err)
		}
	}
	return relinkDecision{}, nil
}

// foreignOwner reports whether scanResultID is junction-linked to any
// work_queue row NOT in own -- the candidate's own set of aggregated work
// items. Filtering in Go rather than building a variadic NOT IN keeps the
// query a single fixed statement; the junction row count per scan_result is
// tiny (normally one), so the scan is trivial.
func foreignOwner(ctx context.Context, tx *sql.Tx, scanResultID int64, own map[int64]bool) (int64, bool, error) {
	var foreignID int64
	var found bool
	rows, err := tx.QueryContext(ctx,
		`SELECT work_queue_id FROM work_queue_scan_results WHERE scan_result_id = ?`, scanResultID)
	if err != nil {
		return 0, false, fmt.Errorf("prune: check present scan_result %d ownership: %w", scanResultID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, false, fmt.Errorf("prune: check present scan_result %d ownership: %w", scanResultID, err)
		}
		if !own[id] && !found {
			foreignID, found = id, true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("prune: check present scan_result %d ownership: %w", scanResultID, err)
	}
	return foreignID, found, nil
}

// presentRowDetail is the detail applyRelinks needs from the present-file
// scan_results row an identity match resolved to.
type presentRowDetail struct {
	scanResultID int64
	filePath     string
	outdir       string
	filename     string
}

// presentIndexGlobalKey is the map key presentIndex uses for an unscoped
// (whole-database) candidate pool, built for a candidate whose owning library
// is unknown (a link-less work_queue-only row with no scan_results of its own).
const presentIndexGlobalKey = int64(-1)

// presentIndex lazily builds and memoizes the library-scoped set of currently-
// present scan_results rows carrying identity -- the candidate pool prune's
// exact-match tier searches. Building a scope's pool costs one query plus one
// os.Stat per identity-bearing row in that library, so a caller should build it
// AT MOST ONCE PER LIBRARY SCOPE ENCOUNTERED during a reconcile pass (memoized
// here), not once per gone candidate.
type presentIndex struct {
	db      *sql.DB
	roots   []string
	byScope map[int64][]identity.Candidate
	details map[int64]map[string]presentRowDetail
}

func newPresentIndex(db *sql.DB, roots []string) *presentIndex {
	return &presentIndex{
		db:      db,
		roots:   roots,
		byScope: map[int64][]identity.Candidate{},
		details: map[int64]map[string]presentRowDetail{},
	}
}

// pool returns the present-file identity candidates for libraryID (or the
// unscoped global pool when libraryID is nil), building and caching it on
// first use.
func (idx *presentIndex) pool(ctx context.Context, libraryID *int64) ([]identity.Candidate, error) {
	key := presentIndexGlobalKey
	if libraryID != nil {
		key = *libraryID
	}
	if pool, ok := idx.byScope[key]; ok {
		return pool, nil
	}
	// Only rows carrying identity can ever win the exact tier, so filtering at
	// the SQL level (seekable via migration 037's partial indexes) skips the
	// os.Stat cost for the majority of a library that has no identity at all.
	query := `SELECT id, file_path, outdir, filename, isrc, recording_mbid FROM scan_results
              WHERE file_path != '' AND (isrc != '' OR recording_mbid != '')`
	var args []any
	if libraryID != nil {
		query += ` AND library_id = ?`
		args = append(args, *libraryID)
	}
	var pool []identity.Candidate
	details := map[string]presentRowDetail{}
	if err := queryRows(ctx, idx.db, query, args, func(rows *sql.Rows) error {
		var id int64
		var path, outdir, filename, isrc, mbid string
		if err := rows.Scan(&id, &path, &outdir, &filename, &isrc, &mbid); err != nil {
			return err
		}
		// DELIBERATE, BOUNDED I/O COST -- flagged because the issue's proposed
		// design explicitly wanted zero extra disk access, and this is the one
		// place that constraint is not fully met. Without this os.Stat, a
		// scan_results row whose file itself later vanished (a second, distinct
		// gone event the periodic sweep has not yet reconciled) would offer
		// itself as a relink target and hand a gone row's identity to another
		// gone row -- silently trading one dangling reference for another. The
		// cost is bounded and does not scale with library size: one os.Stat per
		// IDENTITY-BEARING row (filtered at the SQL level above, migration 037's
		// partial index; the common case is most rows carry no identity at all)
		// in a library SCOPE that actually has a gone-with-identity row to
		// resolve, and the whole pool is memoized here for the rest of the
		// reconcile pass (built at most once per library scope encountered, never
		// once per gone candidate). On a spun-down array this still wakes disks
		// under the stat, so it is a real cost, just a small and bounded one --
		// nowhere near what reading candidate durations for the heuristic tier
		// would have cost (Design Choice 1 explicitly ruled that out).
		if !underAvailableRoot(path, idx.roots) || !pathExists(path) {
			return nil
		}
		pool = append(pool, identity.Candidate{Ref: path, MBID: mbid, ISRC: isrc})
		details[path] = presentRowDetail{scanResultID: id, filePath: path, outdir: outdir, filename: filename}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: build present-file identity index: %w", err)
	}
	idx.byScope[key] = pool
	idx.details[key] = details
	return pool, nil
}

// detail returns the present-row detail for ref within libraryID's scope,
// populated by a prior call to pool. Callers only invoke this after pool
// already resolved ref via identity.ResolveExact, so the lookup is expected to
// always hit.
func (idx *presentIndex) detail(libraryID *int64, ref string) presentRowDetail {
	key := presentIndexGlobalKey
	if libraryID != nil {
		key = *libraryID
	}
	return idx.details[key][ref]
}

// availableRoots returns the configured library root paths that are currently
// mounted and populated. Deletion is confined to sources under these roots so an
// unmounted or unavailable library cannot trigger a mass prune. A root must be
// NON-EMPTY to count as available: an unmounted network share commonly leaves
// its mountpoint directory present but empty, which os.Stat alone would read as
// "exists" -- and then every child source stats ENOENT and the whole library
// would be pruned. Requiring at least one entry treats an empty mountpoint as
// unavailable. The trade-off is that a genuinely-emptied library is not
// auto-pruned (its rows are left to `library remove`), which is the safe bias
// for a destructive operation.
func (p *Pruner) availableRoots(ctx context.Context) ([]string, error) {
	var roots []string
	if err := queryRows(ctx, p.db, `SELECT path FROM libraries WHERE path != ''`, nil, func(rows *sql.Rows) error {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		if dirPopulated(path) {
			roots = append(roots, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: load library roots: %w", err)
	}
	return roots, nil
}

// dirPopulated reports whether path is a directory with at least one entry. Any
// error (not a dir, unreadable, gone, or an unmounted-but-present empty
// mountpoint) yields false, so the caller treats path as unavailable and skips
// pruning under it.
func dirPopulated(path string) bool {
	f, err := os.Open(path) //nolint:gosec // path is a configured library root, not untrusted input
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	// Readdirnames(1) returns io.EOF for an empty directory; any names means
	// populated. This reads at most one entry, so it stays cheap on large roots.
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

// underAvailableRoot reports whether src lies within one of the available roots.
func underAvailableRoot(src string, roots []string) bool {
	for _, root := range roots {
		if src == root || pathutil.WithinRoot(root, src) {
			return true
		}
	}
	return false
}

// gatherCandidates returns, keyed by source path, the scan_results and
// work_queue rows to consider, plus whether any linked work_queue row is
// 'processing'. Candidates come from scan_results (the library-file authority,
// library-scoped when requested) and, when unscoped, also from work_queue
// source paths so link-less rows are covered.
func (p *Pruner) gatherCandidates(ctx context.Context, sc scope, libraryID *int64) (map[string]*candidate, error) {
	bySource := make(map[string]*candidate)

	srQuery := `SELECT id, library_id, file_path, isrc, recording_mbid FROM scan_results WHERE file_path != ''`
	var srArgs []any
	if sc.scoped {
		// Push the path scope into SQL so the reactive PrunePath (fired per
		// filesystem event) narrows at the database instead of full-scanning and
		// filtering every row in Go.
		lower, upper := sc.childRange()
		srQuery += ` AND (file_path = ? OR (file_path >= ? AND file_path < ?))`
		srArgs = append(srArgs, sc.prefix, lower, upper)
	}
	if libraryID != nil {
		srQuery += ` AND library_id = ?`
		srArgs = append(srArgs, *libraryID)
	}
	if err := queryRows(ctx, p.db, srQuery, srArgs, func(rows *sql.Rows) error {
		var id, libID int64
		var path, isrc, mbid string
		if err := rows.Scan(&id, &libID, &path, &isrc, &mbid); err != nil {
			return err
		}
		if !sc.matches(path) {
			return nil
		}
		c := ensureCandidate(bySource, path)
		c.scanResultIDs = append(c.scanResultIDs, id)
		lib := libID
		c.libraryID = &lib
		if isrc != "" {
			c.isrc = isrc
		}
		if mbid != "" {
			c.mbid = mbid
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: gather scan_results: %w", err)
	}

	wqQuery := `SELECT id, artist, title, source_path, output_paths, status, isrc, mbid FROM work_queue WHERE source_path != ''`
	var wqArgs []any
	if libraryID != nil {
		// Library-scope work_queue through the junction so a scoped sweep only
		// prunes queue rows belonging to that library.
		wqQuery = `SELECT DISTINCT wq.id, wq.artist, wq.title, wq.source_path, wq.output_paths, wq.status, wq.isrc, wq.mbid
                   FROM work_queue wq
                   JOIN work_queue_scan_results j ON j.work_queue_id = wq.id
                   JOIN scan_results sr ON sr.id = j.scan_result_id
                   WHERE wq.source_path != '' AND sr.library_id = ?`
		wqArgs = append(wqArgs, *libraryID)
	} else if sc.scoped {
		// Reactive path scope: same index-seekable range predicate as scan_results
		// above, on the idx_work_queue_source_path index (migration 026).
		lower, upper := sc.childRange()
		wqQuery += ` AND (source_path = ? OR (source_path >= ? AND source_path < ?))`
		wqArgs = append(wqArgs, sc.prefix, lower, upper)
	}
	if err := queryRows(ctx, p.db, wqQuery, wqArgs, func(rows *sql.Rows) error {
		var id int64
		var artist, title, source, outputPaths, status string
		var isrc, mbid sql.NullString
		if err := rows.Scan(&id, &artist, &title, &source, &outputPaths, &status, &isrc, &mbid); err != nil {
			return err
		}
		if !sc.matches(source) {
			return nil
		}
		c := ensureCandidate(bySource, source)
		if status == "processing" {
			c.processing = true
			return nil
		}
		if status == "done" {
			// A settled row is not work, so it is not a retirement candidate. Tracking
			// this is what makes retirement CONVERGE: without it, a row retired to
			// 'done' is still gathered from scan_results on the next sweep, re-reported
			// as retained, and re-retired -- relocating the non-converging fixed point
			// (#732) instead of removing it. Recorded rather than returned early,
			// because a 'done' row still belongs to the candidate for the relink and
			// prune paths, which act on settled rows too.
			c.settled = true
		}
		var paths []models.OutputPath
		if outputPaths != "" {
			if err := json.Unmarshal([]byte(outputPaths), &paths); err != nil {
				return fmt.Errorf("unmarshal output_paths for work_queue %d: %w", id, err)
			}
		}
		c.workItems = append(c.workItems, workRow{
			id: id,
			inputs: models.Inputs{
				Track:       models.Track{ArtistName: artist, TrackName: title},
				SourcePath:  source,
				OutputPaths: paths,
			},
		})
		// work_queue.isrc/mbid (migration 033, the provider's resolved identity
		// at fetch time) is only a FALLBACK: scan_results' tag-read identity is
		// preferred because it can never be stale for a since-deleted file (it
		// is re-read on every scan), whereas this column is a point-in-time
		// stamp from whichever provider match won.
		if c.isrc == "" && isrc.Valid && isrc.String != "" {
			c.isrc = isrc.String
		}
		if c.mbid == "" && mbid.Valid && mbid.String != "" {
			c.mbid = mbid.String
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: gather work_queue: %w", err)
	}
	return bySource, nil
}

// deletePruned deletes the pruned rows in a single transaction, invoking report
// once for each row AFTER the commit, and only for rows that actually lost at
// least one row to the deletes, so a backup/log never records a row that a race
// into 'processing' skipped or that a rollback left in place.
//
// Both deletes guard on the SAME condition -- the linked work_queue row is not
// 'processing' -- so a row that raced into 'processing' between gather and delete
// (a worker claiming a pending row) is skipped on BOTH tables, never half-pruned.
// The work_queue guard is `status != 'processing'` (NOT an allow-list): a moved
// track whose lyrics already completed is 'done', and a reconciler for vanished
// sources must delete those too -- excluding 'done' would leak the queue row
// forever while still deleting its scan_results row. It returns the actual rows
// deleted (via RowsAffected) so the caller's totals reflect what happened, not
// just what was intended.
// retireUnresolvable takes a permanently-unactionable row out of the
// dequeue-eligible set without deleting anything (#732). It reports whether any
// row was actually retired.
//
// WHY 'done' AND NOT A NEW STATUS. work_queue.status carries a CHECK constraint,
// so a genuinely new value means recreating the table -- a migration far out of
// proportion to the fix. queue.RetireMiss already established the cheaper
// pattern for exactly this shape: settle to 'done' and record WHY in last_error.
// A distinct terminal state is the subject of #477, which will want to convert
// both this and RetireMiss together rather than have one of them arrive early
// and differently.
//
// THE STATUS GUARD IS THE IN-FLIGHT GUARD. `status NOT IN ('processing','done')`
// does two jobs: it never retires a row the worker currently owns, and it makes
// the whole operation idempotent, since an already-retired row is 'done' and no
// longer matches. That idempotence is the point of the fix -- without it this
// would relocate the non-converging fixed point rather than remove it.
//
// completed_at is stamped because the row IS settled; leaving it null would make
// a retired row look perpetually in-flight to every report that reads it.
func (p *Pruner) retireUnresolvable(ctx context.Context, c *candidate) (bool, error) {
	if c == nil || len(c.workItems) == 0 {
		return false, nil
	}
	now := time.Now().UTC().Format(timeFormat)
	retired := false
	for _, w := range c.workItems {
		res, err := p.db.ExecContext(ctx,
			`UPDATE work_queue
             SET status = 'done',
                 completed_at = ?,
                 last_error = ?
             WHERE id = ?
               AND status NOT IN ('processing', 'done')`,
			now, unresolvableGoneError, w.id)
		if err != nil {
			return false, fmt.Errorf("retire work item %d: %w", w.id, err)
		}
		if rowsAffected(res) > 0 {
			retired = true
		}
	}
	return retired, nil
}

func (p *Pruner) deletePruned(ctx context.Context, pruned []PrunedRow, report func(PrunedRow) error) (scanDeleted, workDeleted int, retErr error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("prune: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var applied []PrunedRow // rows that actually lost >=1 row, reported post-commit
	for _, row := range pruned {
		before := scanDeleted + workDeleted
		for _, id := range row.WorkItemIDs {
			res, err := tx.ExecContext(ctx,
				`DELETE FROM work_queue WHERE id = ? AND status != 'processing'`, id)
			if err != nil {
				return 0, 0, fmt.Errorf("prune: delete work_queue %d: %w", id, err)
			}
			workDeleted += rowsAffected(res)
		}
		for _, id := range row.ScanResultIDs {
			// Skip a scan_results row still linked to an in-flight (processing)
			// work_queue row, so a worker never has its scan_result_id nulled
			// (migration 009) and junction cascaded (010) out from under it.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM scan_results WHERE id = ?
                 AND NOT EXISTS (
                     SELECT 1 FROM work_queue_scan_results j
                     JOIN work_queue wq ON wq.id = j.work_queue_id
                     WHERE j.scan_result_id = ? AND wq.status = 'processing')`,
				id, id)
			if err != nil {
				return 0, 0, fmt.Errorf("prune: delete scan_results %d: %w", id, err)
			}
			scanDeleted += rowsAffected(res)
		}
		if scanDeleted+workDeleted > before {
			applied = append(applied, row)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("prune: commit tx: %w", err)
	}
	// Report only after the deletes are durably committed, so a backup record is
	// never written for a row that survived (skipped mid-tx or rolled back).
	if report != nil {
		for _, row := range applied {
			if err := report(row); err != nil {
				return scanDeleted, workDeleted, fmt.Errorf("prune: report %q: %w", row.SourcePath, err)
			}
		}
	}
	return scanDeleted, workDeleted, nil
}

// rowsAffected returns the affected-row count, treating a driver that does not
// report it as 0 (SQLite always reports, so this is defensive).
func rowsAffected(res sql.Result) int {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}

func ensureCandidate(m map[string]*candidate, src string) *candidate {
	c, ok := m[src]
	if !ok {
		c = &candidate{}
		m[src] = c
	}
	return c
}

// gone reports whether src's source file is absent, per granularity. Directory
// granularity caches directory existence so a large album is statted once.
func gone(src string, g Granularity, dirCache map[string]bool) bool {
	if g == Directory {
		dir := filepath.Dir(src)
		exists, cached := dirCache[dir]
		if !cached {
			exists = pathExists(dir)
			dirCache[dir] = exists
		}
		return !exists
	}
	return !pathExists(src)
}

// pathExists reports whether p exists. Only a definitive not-exist result counts
// as gone; any other stat error (permissions, I/O) is treated as "exists" so a
// transient error never triggers a destructive prune.
func pathExists(p string) bool {
	if _, err := os.Stat(p); err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	return true
}

func queryRows(ctx context.Context, db *sql.DB, query string, args []any, scan func(*sql.Rows) error) (retErr error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func sortedKeys(m map[string]*candidate) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Deterministic order keeps prune output and backups stable across runs.
	sort.Strings(keys)
	return keys
}
