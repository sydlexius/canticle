// Package prune reconciles the durable work queue and scan-result cache against
// the filesystem: rows whose source audio file no longer exists on disk are
// deleted so a renamed/merged/deleted track cannot leave a permanently-failing
// or wedged row behind (#453).
//
// The filesystem is the sole authority for "gone": a source path is a
// candidate for removal only when os.Stat of that path (Exact granularity) or
// its directory (Directory granularity, which also stats the files of a
// directory whose mtime changed) fails. The same primitive backs three
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
// retain only), leaving genuine deletion to the CLI and to the periodic sweep
// of a REMOVED directory (see policyInFolder for a surviving one), by which
// time a rescan has had time to settle.
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
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/identity"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/queue"
)

// timeFormat is the timestamp layout every stored column uses. Declared locally
// rather than shared, matching internal/queue and internal/reports, which each
// carry their own copy of the same constant.
const timeFormat = time.RFC3339

// Granularity selects how a candidate source path is tested for existence.
type Granularity int

const (
	// Directory stats only the parent directory of each candidate source path.
	// A source is gone when its directory is gone. A surviving directory costs
	// one stat while its mtime matches the stored one and, if it was stored with
	// a row still gone, no scan has indexed anything since (see dirMtimes);
	// otherwise that directory's row files are stat'ed exactly, so an in-folder
	// swap or rename is caught on the next sweep (#1262). A row gone inside a surviving directory is relinked or
	// retained (policyInFolder), never retired, and deleted by this unattended
	// pass only by reconcile's age-out: gone for goneGrace, then confirmed twice.
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
	// policyInFolder is the periodic Directory sweep's policy for a row gone
	// INSIDE a surviving directory (#1262). Like PolicyRelinkOrRetain it never
	// retires, deletes only by age-out, and never builds the identity pool, which
	// costs one stat per identity-bearing file in the library: the only relink
	// it makes is the same-stem sibling's. Relinking by MBID/ISRC or by title
	// stays with `scan reconcile-paths` and the removed-directory path.
	policyInFolder
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
	// AgedOut marks a source the periodic sweep deleted for having been gone
	// inside a surviving directory for the whole grace period (#1262).
	AgedOut bool
	// Corrects: an AgedOut source is reported BEFORE its delete; if a row then stays, this second report replaces the first.
	Corrects bool
	States   []WorkState // each WorkItemIDs row's state, in the same order
}

// ErrNotRecorded is what a Report hook returns for an AgedOut row whose backup
// record it could not write and has accounted for itself: the row is kept (an
// aged-out row is never deleted unrecorded) and the sweep does not fail.
var ErrNotRecorded = errors.New("prune: backup record not written")

// WorkState is a pruned work_queue row's state, for its backup record.
type WorkState struct {
	Status        string `json:"status"`
	OutcomeType   string `json:"outcome_type,omitempty"`
	SyncTier      string `json:"sync_tier,omitempty"`
	TimingOutcome string `json:"timing_outcome,omitempty"`
	LyricEditedAt string `json:"lyric_edited_at,omitempty"`
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
	// operator cares to inspect). Empty for a same-stem sibling relink (#1262),
	// which no identity drove.
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
	// Retired reports that this row was ACTUALLY taken out of the dequeue-eligible
	// set: the UPDATE ran and affected a row. It is still a RetainedRow -- nothing
	// was deleted and the operator-facing accounting is unchanged -- but the worker
	// will no longer attempt it.
	//
	// COMMITTED STATE, NEVER INTENT. False in a dry run (which mutates nothing) and
	// false when the UPDATE's status guard declined the row, so a report can never
	// describe a change the database did not make. Use WouldRetire to preview.
	Retired bool
	// WouldRetire reports that this row QUALIFIED for retirement: gone, carrying no
	// identity, still holding dequeue-eligible work. It is the plan, so it is set
	// identically whether or not the sweep is a dry run -- which is what makes a
	// dry run's report a truthful preview of a real one.
	WouldRetire bool
}

// unresolvableGoneError is the last_error value written when a row is retired as
// permanently unactionable: its source file is gone AND it carries no identity,
// so no relink can ever resolve it. Defined once so the SQL bind, the tests, and
// any log or inspection of the sentinel cannot drift -- the same discipline as
// queue.missLimitReachedError, whose retire-to-'done' pattern this mirrors.
//
// The text is written for someone reading it six months from now with no context:
// it states the mechanism, not a verdict. The literal lives in queue so the
// reconcile-remediated guard (#1143) recognizes a retired row without a copy.
const unresolvableGoneError = queue.UnresolvableGoneError

// Result reports the totals and per-row detail of a prune (or, in dry-run, what
// would happen).
type Result struct {
	ScanResults int
	WorkItems   int
	Pruned      []PrunedRow
	Relinked    []RelinkedRow
	Retained    []RetainedRow
	// EditHeld counts relinked work_queue rows a resurrect would have reopened
	// to 'pending' but that carry the hand-edit mark (#1228): their path moved,
	// their status did not. Applied runs only; a dry run leaves it 0.
	EditHeld int
	// RelinkOwned and RelinkChanged count planned relinks that did not happen
	// (#1262): the target already belongs to a different queue row, or a row was
	// claimed or changed after it was read. A dry run makes the ownership check
	// too, so RelinkOwned matches the apply's; RelinkChanged is a race, 0 there.
	RelinkOwned   int
	RelinkChanged int
	// AgedOut counts the Pruned sources the periodic sweep deleted (in a dry
	// run, would delete) because they were gone inside a surviving directory
	// for the whole grace period (#1262).
	AgedOut int
	// AgeOutHeld, when non-zero, is the age-out's circuit breaker: that many
	// sources are held after this sweep. It trips when the sources due plus those
	// already held exceed ageOutMaxPerSweep (none is deleted, each is held), and
	// a held source stays held, and counted here, until the attended run.
	AgeOutHeld int
	// PruneSkipped counts the Pruned (planned) sources an apply did not fully
	// delete: a row in flight, or moved since it was read. 0 in a dry run.
	PruneSkipped int
	// SkippedMalformed counts work_queue rows left out of the sweep because
	// their output_paths is not valid JSON (#937). They are neither pruned,
	// relinked nor retired; the rest of the sweep proceeds.
	SkippedMalformed int
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

// Heuristic-tier defaults, mirroring internal/config's realign defaults
// (realignMinConfidenceDefault / realignMinMarginDefault, both unexported
// there). Duplicated as literals rather than imported to keep prune free of a
// config dependency, matching how identityKeys defaults here and is then
// overridden by the caller from the same config -- see SetNameMatchThresholds.
// A caller that wires config always wins; these only cover a caller that does
// not, and they must stay conservative rather than permissive.
const (
	defaultMinConfidence = 0.75
	defaultMinMargin     = 0.05
)

// Pruner reconciles work_queue and scan_results against the filesystem.
type Pruner struct {
	db           *sql.DB
	identityKeys identity.Keys
	// minConfidence/minMargin gate the heuristic relink tier (#740), read from
	// the SAME config realign uses so the two subsystems cannot disagree about
	// what counts as a confident name match. Defaulted here to realign's own
	// documented defaults so a caller that never sets them still gets the
	// conservative behavior rather than a zero threshold that accepts anything.
	minConfidence float64
	minMargin     float64
	// stat and now are the reconcile's filesystem and clock, fields only so a
	// test can count stats and inject a stat failure.
	stat func(string) (fs.FileInfo, error)
	now  func() time.Time
	// readDir is the age-out's one directory read.
	readDir func(string) ([]os.DirEntry, error)
}

// New returns a Pruner backed by db, with the default identity-key order
// (mbid, then isrc) for the exact-match re-link tier. Call SetIdentityKeys to
// honor an operator's configured config.RealignConfig.IdentityKeys order --
// the same config realign reads, so the two subsystems can never disagree
// about key precedence.
func New(db *sql.DB) *Pruner {
	return &Pruner{
		db:            db,
		identityKeys:  identity.NormalizeKeys([]string{"mbid", "isrc"}),
		minConfidence: defaultMinConfidence,
		minMargin:     defaultMinMargin,
		stat:          os.Stat,
		now:           time.Now,
		readDir:       os.ReadDir,
	}
}

// SetNameMatchThresholds overrides the heuristic tier's confidence floor and
// runner-up margin (#740). Callers pass config.RealignConfig's values so prune
// and realign judge a name match identically; a zero or negative value is
// ignored rather than accepted, since a zero threshold would make every pair a
// confident match.
func (p *Pruner) SetNameMatchThresholds(minConfidence, minMargin float64) {
	if minConfidence > 0 {
		p.minConfidence = minConfidence
	}
	if minMargin > 0 {
		p.minMargin = minMargin
	}
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
	return p.reconcile(ctx, scope{prefix: path, scoped: true}, nil, nil, false, PolicyRelinkOrRetain, reportHooks{})
}

// Sweep reconciles every candidate source path in scope. Directory granularity
// is the cheap backstop; Exact is the thorough operator-invoked pass. With
// DryRun set it reports without mutating. Runs under PolicyFull (a gone row
// whose identity matches nothing anywhere in the library is genuinely
// deleted), except that a Directory sweep's row gone inside a SURVIVING
// directory runs under policyInFolder. An unscoped, non-dry Directory sweep
// then stores its directory state; a failure to store it is logged and the
// committed result still returned, since the next sweep just re-examines.
func (p *Pruner) Sweep(ctx context.Context, opts SweepOptions) (Result, error) {
	var ds *dirMtimes
	if opts.Granularity == Directory {
		var err error
		if ds, err = p.loadDirState(ctx); err != nil {
			return Result{}, err
		}
	}
	res, err := p.reconcile(ctx, scope{}, opts.LibraryID, ds, opts.DryRun, PolicyFull, reportHooks{
		Prune:    opts.Report,
		Relinked: opts.ReportRelinked,
		Retained: opts.ReportRetained,
	})
	// A dry run records nothing, and neither does a library-scoped sweep: it saw
	// only that library's rows, and the state is per directory, not per library.
	// Attended and unscoped: the next sweep looks again at what the breaker still
	// holds. A library-scoped apply releases none: it saw one library's rows, and
	// releasing the rest would only have the next sweep re-examine and trip again.
	if err == nil && ds == nil && !opts.DryRun && opts.LibraryID == nil {
		if _, rerr := p.db.ExecContext(ctx, `UPDATE prune_gone_since SET confirmed_at = first_seen WHERE confirmed_at = ?`, heldMark); rerr != nil {
			slog.Warn("path reconciliation could not release the age-out marks the periodic sweep is holding", "error", rerr)
		}
	}
	if err != nil || ds == nil || opts.DryRun || opts.LibraryID != nil {
		return res, err
	}
	if err := ds.save(ctx, p.db); err != nil && ctx.Err() == nil {
		slog.Warn("path-reconciliation sweep could not store its directory state; the next sweep re-examines", "error", err)
	}
	return res, nil
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
	// settled is true when EVERY linked work_queue row has reached a terminal
	// state -- 'done' OR 'unavailable' (#477) -- so the source is no longer
	// work, and there is nothing to retire. Distinct from processing: a
	// settled source is still a valid relink and prune target, it just must
	// not be re-retired and re-reported on every sweep. 'unavailable' counts
	// as settled because, like 'done', it is out of the dequeue-eligible set:
	// there is nothing for prune to retire. Settled does NOT mean "never
	// relinked" for it, though -- see holdsUnavailable and classify.
	//
	// Derived by markSettled after the gather, never latched during it: a source
	// may carry several work_queue rows, and "at least one is done" is a strictly
	// weaker claim that would drop a candidate still holding eligible work.
	settled bool
	// doneWorkItems counts linked work_queue rows already terminal ('done' or
	// 'unavailable'). Compared against len(workItems) to derive settled. The
	// field keeps its pre-#477 name (doneWorkItems, not settledWorkItems)
	// deliberately: renaming it would touch every reference for no behavioral
	// gain, and the doc comment here is the single source of truth for what it
	// now counts.
	doneWorkItems int
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
	// lastError is the row's stored last_error, carried so the settled
	// short-circuit can recognize a row THIS FEATURE retired (the
	// unresolvableGoneError sentinel) as distinct from a genuinely completed one.
	// See classify's retired-row reconsideration.
	lastError string
	// status is the row's work_queue status at gather time, carried so classify
	// can recognize an 'unavailable' row (see holdsUnavailable).
	status string
	// rawOutputPaths is the output_paths column exactly as gathered, used by
	// relinkOne's UPDATE as an optimistic-concurrency guard: the relinked list
	// is derived from this snapshot, so the write only lands if the column still
	// holds it.
	rawOutputPaths string
	state          WorkState
}

// retiredAsUnresolvable reports whether EVERY linked work item carries the
// retirement sentinel this package writes -- i.e. the candidate is settled
// because prune retired it as unrelinkable, not because its work completed.
//
// Requires ALL items, not any: a candidate holding one genuinely-completed item
// alongside a retired one is not ours to reconsider, and resurrecting the
// completed one would re-queue work that already succeeded. Erring toward
// leaving a row settled is the safe direction -- it costs a missed recovery,
// while the opposite costs duplicated work.
func (c *candidate) retiredAsUnresolvable() bool {
	if len(c.workItems) == 0 {
		return false
	}
	for _, w := range c.workItems {
		if w.lastError != unresolvableGoneError {
			return false
		}
	}
	return true
}

// holdsUnavailable reports whether any linked work item is in the 'unavailable'
// status queue.RetireMiss writes for an exhausted benign miss (#477).
//
// Such a row is settled but still worth a relink: its source file may simply
// have moved (an artist-folder rename), and nothing else repairs its
// source_path -- queue.Enqueue's upsert preserves source_path for an
// 'unavailable' row, so a rescan of the new location cannot. classify
// therefore consults the name tier for it exactly as it does for prune's own
// sentinel-retired rows, and drops it silently on a decline. relinkOne never
// resurrects it: resurrection keys on prune's own sentinel, and an
// 'unavailable' row carries RetireMiss's, so only the path columns move.
func (c *candidate) holdsUnavailable() bool {
	for _, w := range c.workItems {
		if w.status == queue.StatusUnavailable {
			return true
		}
	}
	return false
}

// taggedTitle returns the first non-empty TAG-derived track title among this
// candidate's linked work items, or "" when none carries one.
//
// Deliberately the TITLE ALONE and never the filename stem: the stem is a
// fallback for SCORING two sides that both lack tags, and prune must not relink
// on it (see the caller's Gate 4). Deliberately not the artist either -- an
// artist matches every track on an album and identifies no single song.
//
// A disagreement among several work items resolves to the first non-empty title
// in id order, which is deterministic. In practice a candidate's items all
// describe the same source file, so a disagreement means the tags changed
// between enqueues; taking one and requiring an EXACT match against a present
// file is safe either way, since a stale title simply matches nothing.
func (c *candidate) taggedTitle() string {
	for _, w := range c.workItems {
		if t := strings.TrimSpace(w.inputs.Track.TrackName); t != "" {
			return t
		}
	}
	return ""
}

// nameSignal describes this candidate's name evidence for the resolver, built
// from the title the CALLER already resolved (via taggedTitle) rather than
// re-derived here.
//
// Taking the title as a parameter is what keeps the filter and the score talking
// about the same string. An earlier version re-scanned the work items on an
// artist-OR-title predicate, so a candidate whose first item carried an artist
// but no title produced an empty Title -- identity.NameSignal.discriminator then
// fell back to the filename STEM and scored it against a pool that had been
// filtered on the real title. The result was a row that failed to relink even
// though its title matched exactly, reported with a reason that was false.
//
// The artist is deliberately absent: discriminator ignores it whenever a title
// is present (#672), and Gate 4 guarantees a title is present on every call.
func (c *candidate) nameSignal(src, title string) identity.NameSignal {
	return identity.NameSignal{Title: title, Stem: stemOf(src)}
}

// titleKey is prune's OWN title predicate, and it deliberately does NOT reuse
// normalize.NormalizeKey.
//
// NormalizeKey is a CACHE key: it NFKD-decomposes and then strips every combining
// mark (Unicode category Mn), which is right for its own callers -- folding
// accent variants together makes a cache hit more likely, and a wrong cache hit
// costs one re-fetch. That lossiness is catastrophic here, because a wrong relink
// is permanent. Japanese dakuten and handakuten ARE combining marks (U+3099 /
// U+309A after decomposition), so NormalizeKey DELETES them, collapsing
// voiced and unvoiced kana into one key -- different words, not variants.
// Measured: four of four tested kana pairs collided under NormalizeKey, and an
// end-to-end sweep relinked a queue row onto a different song through that
// collision. This repo ships a Japanese-catalog provider, so the affected
// population is real rather than theoretical.
//
// NFKC instead of NFKD-plus-strip keeps the compatibility folds that are safe and
// wanted (fullwidth to ASCII, a Roman-numeral glyph to letters, a ligature to its
// letters) while leaving marks attached, since a composed form keeps voiced kana
// as single code points. Case and surrounding whitespace still fold; a phoneme
// never does.
func titleKey(s string) string {
	return strings.ToLower(strings.TrimSpace(norm.NFKC.String(strings.ToValidUTF8(s, ""))))
}

// tryNameRelink is the name tier (#740): it decides whether a gone,
// identity-less row can be re-attached to a present file that carries the SAME
// TITLE, and returns the relink classification when it can.
//
// Returns (false, reason, _, nil) when the tier declines, where reason explains
// which gate stopped it so the retain is legible. Returns (false, "", _, err)
// only on a genuine I/O failure, which the caller MUST propagate rather than
// treat as a no-match -- see the caller's comment.
//
// EVERY GATE BELOW IS A SAFETY PRECONDITION, not a tidiness check. This tier
// rewrites which audio file a queue row points at, on name evidence alone, and a
// wrong answer is silent and permanent. Each gate closes a demonstrated defect.
func (p *Pruner) tryNameRelink(ctx context.Context, idx *presentIndex, policy Policy, src string, c *candidate) (bool, string, classified, error) {
	// GATE 1 -- PERIODIC SWEEP ONLY. The reactive pass (PrunePath, fired from the
	// watcher's delete event) runs BEFORE a rescan has indexed the moved file's
	// new location, so the true target is typically absent from the pool and the
	// best-scoring present file is a different song. The pre-existing code already
	// defers RETIREMENT to the periodic sweep for exactly this reason; a relink is
	// at least as consequential, so it earns the same discipline. By the periodic
	// sweep the rescan has settled and the real target is present.
	if policy == policyInFolder {
		return false, "identity absent, and no same-name file with another extension is indexed beside it; the periodic sweep relinks nothing else inside a surviving directory (`scan reconcile-paths` does)", classified{}, nil
	}
	if policy != PolicyFull {
		return false, "identity absent; deferred to the periodic sweep, which sees a settled index", classified{}, nil
	}
	// GATE 2 -- MUST BE REAL WORK. A candidate with no linked work_queue row has
	// nothing to repoint: relinkOne's ownership check is skipped when the owned
	// set is empty, its update loop never executes (so the in-flight guard never
	// runs), and control reaches the DELETE, destroying a scan_results row on a
	// name guess while nothing was moved. Retiring is also meaningless here. The
	// row is left exactly as it is.
	if len(c.workItems) == 0 {
		return false, "identity absent, and no queue row to re-attach; left untouched", classified{}, nil
	}
	// GATE 3 -- LIBRARY-SCOPED ONLY. A nil libraryID makes poolHeuristic omit its
	// library_id filter and score across EVERY library in the database, where a
	// same-title file is a duplicate copy rather than a move. A matching MBID may
	// justify crossing that boundary; a matching name never does.
	if c.libraryID == nil {
		return false, "identity absent, and the row is not library-scoped; never matched across libraries by name", classified{}, nil
	}
	// GATE 4 -- THE ORPHAN MUST CARRY A REAL TITLE TAG. Without one there is no
	// evidence to match on: identity.NameSignal treats a bare filename stem as
	// untagged, and ResolveHeuristic's untagged degradation then pairs
	// POSITIONALLY on a lone target. That degradation is correct for realign,
	// whose targets are the single sidecar-less gap in ONE DIRECTORY, and wrong
	// here, where targets are the whole library -- it was demonstrated relinking
	// onto a completely unrelated file. Requiring a title makes the degradation
	// unreachable from prune.
	title := c.taggedTitle()
	if title == "" {
		return false, "identity absent, and no title tag to match on; never relinked on a filename alone", classified{}, nil
	}
	wantTitle := titleKey(title)
	if wantTitle == "" {
		return false, "identity absent, and its title normalizes to nothing; never relinked on an empty key", classified{}, nil
	}

	hpool, err := idx.poolHeuristic(ctx, c.libraryID)
	if err != nil {
		return false, "", classified{}, err
	}

	// GATE 5 -- EXACT NORMALIZED TITLE. The filter, and the reason a near-miss
	// can no longer win: a title that merely RESEMBLES another is excluded here,
	// before any scoring happens.
	//
	// The orphan's OWN row is dropped in the same pass. poolHeuristic performs no
	// per-row stat, so the gone source is still in the pool with the very
	// artist/title being scored; left in, it matches itself and either wins (then
	// fails the stat below) or ties the true match into a Conflict, so the tier
	// would relink essentially nothing.
	pool := make([]identity.Candidate, 0, 4)
	for _, hc := range hpool {
		if hc.Ref == src {
			continue
		}
		if titleKey(hc.Title) == wantTitle {
			pool = append(pool, hc)
		}
	}
	if len(pool) == 0 {
		return false, "identity absent, and no present file shares this title; never deleted on a guess", classified{}, nil
	}

	// Every survivor has an identical normalized title, so all score 1.0: a lone
	// survivor resolves Unique, and two or more fall inside the runner-up margin
	// and resolve Conflict. Passing one slice as both targets and rivals is right
	// here -- every same-title present file is both a possible destination and a
	// possible confusion.
	hres := identity.ResolveHeuristic(c.nameSignal(src, title), pool, pool, p.minConfidence, p.minMargin)
	switch hres.Verdict {
	case identity.VerdictUnique:
		// Resolved -- fall through to the stat and detail checks below.
	case identity.VerdictConflict:
		return false, "identity absent, and several present files share this title; never picked one on a guess", classified{}, nil
	default:
		// VerdictNone: the pool was non-empty (checked above) but nothing cleared
		// the guard. Reported distinctly rather than folded into the conflict
		// message, which would tell an operator there were several candidates when
		// there was one.
		return false, "identity absent, and the sole same-title file did not clear the name guard; never relinked on a weak signal", classified{}, nil
	}
	// Stat ONLY the winner. The pool deliberately skips the per-row os.Stat that
	// pool() performs -- statting a whole library scope would multiply disk
	// wakeups on a spun-down array -- so this is where that safety is bought back,
	// for one stat rather than tens of thousands. A winner that has itself
	// vanished is no target: it would trade one dangling reference for another.
	if !pathExists(p.stat, hres.Ref) {
		return false, "identity absent, and the only same-title file has itself vanished; never relinked onto a dead path", classified{}, nil
	}
	// A detail MISS must decline, never proceed with the zero value: a zero detail
	// carries scanResultID 0 and empty output columns, which would junction the
	// row to a nonexistent scan_result and blank its outdir/filename.
	detail, ok := idx.detailWide(c.libraryID, hres.Ref)
	if !ok || detail.scanResultID == 0 {
		return false, "identity absent, and the matched file's row could not be resolved; left untouched", classified{}, nil
	}
	rr := RelinkedRow{OldPath: src, NewPath: hres.Ref, ScanResultIDs: c.scanResultIDs}
	for _, w := range c.workItems {
		rr.WorkItemIDs = append(rr.WorkItemIDs, w.id)
	}
	return true, "", classified{
		outcome: outcomeRelink,
		classifiedRelink: classifiedRelink{
			src: src, c: c, relinked: rr, target: detail,
			// Past Gate 1 the policy is known to be PolicyFull, and Gate 2 already
			// established there is at least one work item, so this mirrors the
			// retain path's own condition. It only takes effect if the relink is
			// DECLINED at apply time; a successful relink leaves the row eligible,
			// which is the whole point of relinking it.
			retireIfDeclined: !c.settled,
			alreadySettled:   c.settled,
		},
	}, nil
}

// reconcile is the shared core behind PrunePath and Sweep. It gathers candidate
// source paths, uses os.Stat (per granularity) as the sole authority for gone,
// applies the in-flight guard, classifies each gone candidate via the shared
// identity resolver into prune/relink/retain, and applies the result --
// deleting genuinely-gone rows, relinking rows whose identity resolved
// elsewhere, and reporting (never mutating) retained rows.
//
// ds non-nil selects Directory granularity (see dirMtimes); nil stats every
// candidate source exactly.
func (p *Pruner) reconcile(ctx context.Context, sc scope, libraryID *int64, ds *dirMtimes, dryRun bool, fullPolicy Policy, hooks reportHooks) (Result, error) {
	bySource, malformed, err := p.gatherCandidates(ctx, sc, libraryID)
	if err != nil {
		return Result{}, err
	}
	// Load the set of library roots that currently exist on disk. A source is
	// only ever judged "gone" when it sits under an available root, so an entire
	// library that is merely unmounted (its mountpoint present but empty, making
	// every child os.Stat return ENOENT) cannot be mass-deleted. A root that is
	// genuinely removed is left to `library remove`, not this reconciler.
	roots, offline, err := p.availableRoots(ctx)
	if err != nil {
		return Result{}, err
	}
	idx := newPresentIndex(p.db, roots)
	idx.scopeLibrary, idx.stat = libraryID, p.stat

	// An Exact sweep (`scan reconcile-paths`, attended) is the circuit breaker's
	// exit: it also deletes the rows whose marks are due and confirmed, uncapped.
	marks := ds
	if ds != nil {
		ds.offline = offline
	} else if fullPolicy == PolicyFull {
		if marks, err = p.loadDirState(ctx); err != nil {
			return Result{}, err
		}
	}
	var res Result
	res.SkippedMalformed = malformed
	var toPrune, aged []PrunedRow
	listings := map[string][]os.DirEntry{}
	var toRelink []classifiedRelink
	prunedRow := func(src string, c *candidate) PrunedRow {
		row := PrunedRow{SourcePath: src, ScanResultIDs: c.scanResultIDs}
		for _, w := range c.workItems {
			row.WorkItemIDs = append(row.WorkItemIDs, w.id)
			row.Inputs = append(row.Inputs, w.inputs)
			row.States = append(row.States, w.state)
		}
		return row
	}
	for _, src := range sortedKeys(bySource) {
		c := bySource[src]
		if !underAvailableRoot(src, roots) {
			continue
		}
		policy := fullPolicy
		if ds == nil {
			if pathExists(p.stat, src) {
				continue
			}
		} else if isGone, inFolder := ds.gone(src); !isGone {
			continue
		} else if inFolder {
			policy = policyInFolder
		}
		if c.processing {
			ds.retry(src) // not recorded: looked at again once the worker lets go
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
		// THE AGE-OUT (#1262). A row gone inside a surviving directory that this
		// examination could neither relink nor tie to a present same-name file
		// is marked, and is deleted once the mark is a week old and confirmed. A row
		// held for a present sibling (ambiguous, owned, another recording,
		// unreadable), for an indexed file with its MBID/ISRC that is present or
		// cannot be ruled out, or for the one present file the name tier would
		// relink it to by title, never ages: its replacement may be there. Nor
		// does a row whose queue row another library's file also links.
		ageable := (policy == policyInFolder || (ds == nil && marks.confirmedDue(src))) && !cg.siblingHeld && cg.outcome != outcomeRelink
		if ageable {
			held, err := p.identityHeld(ctx, src, c, roots)
			if err != nil {
				return Result{}, err
			}
			ageable = !held
		}
		if ageable {
			shared, err := p.sharedAcrossLibraries(ctx, src, c, libraryID)
			if err != nil {
				return Result{}, err
			}
			ageable = !shared
		}
		if !ageable {
			ds.forget(src)
		} else if ds == nil { // attended: a refusal is reported as the row it is
			ageable = !p.replacementOnDisk(src, listings)
		}
		if ageable && (ds == nil || ds.aged(src)) {
			aged = append(aged, prunedRow(src, c))
			continue
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
			// Retired reports COMMITTED state, never intent -- so it starts false and
			// is set only by an UPDATE that actually affected a row. A dry run
			// therefore reports Retired=false throughout: it changes nothing, and a
			// preview that claims a queue row moved is exactly the defect #725 shipped
			// (a dry run whose report described writes it never made). WouldRetire
			// carries the plan for anyone previewing.
			cg.retained.WouldRetire = cg.shouldRetire
			if cg.shouldRetire && !dryRun {
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
			toPrune = append(toPrune, prunedRow(src, c))
		}
	}
	// An aged row is deleted only if its file is STILL definitively gone now and
	// its directory, read once, holds something (an unmounted nested mountpoint
	// is an empty directory whose every file reads not-exist) and no unindexed
	// same-name file. Checked in a dry run too, so the plan matches the apply.
	// A refusal drops the mark. Then the confirmation, then the circuit breaker,
	// a hard stop: the marks already held count toward the cap, so rows newly due
	// that would take the total over it are held too, and a held row is never in
	// due on a periodic sweep (confirm refuses it), so only the attended run
	// deletes one.
	var due []PrunedRow
	for _, row := range aged {
		if _, err := p.stat(row.SourcePath); !errors.Is(err, fs.ErrNotExist) || p.replacementOnDisk(row.SourcePath, listings) {
			ds.forget(row.SourcePath)
		} else if ds == nil || ds.confirm(row.SourcePath) {
			row.AgedOut = true
			due = append(due, row)
		}
	}
	held := 0
	if ds != nil {
		held = ds.held(bySource)
	}
	if ds != nil && held+len(due) > ageOutMaxPerSweep {
		res.AgeOutHeld = held + len(due)
		for _, row := range due {
			ds.since[row.SourcePath], ds.changed[row.SourcePath] = goneMark{ds.since[row.SourcePath].first, heldMark}, true
		}
	} else {
		toPrune, res.AgedOut = append(toPrune, due...), len(due)
		res.AgeOutHeld = held // still held, at or under the cap: reported every sweep
	}
	res.Pruned = toPrune

	if dryRun {
		// The plan takes the apply's ownership check, so the two report the same.
		planned, declined, owned, err := p.planRelinks(ctx, toRelink)
		if err != nil {
			return Result{}, err
		}
		res.Relinked, res.RelinkOwned = planned, owned
		res.Retained = append(res.Retained, declined...)
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
		for _, rr := range planned {
			if hooks.Relinked != nil {
				if err := hooks.Relinked(rr); err != nil {
					return Result{}, fmt.Errorf("prune: report relinked %q: %w", rr.OldPath, err)
				}
			}
		}
		for _, rr := range declined {
			if hooks.Retained != nil {
				if err := hooks.Retained(rr); err != nil {
					return Result{}, fmt.Errorf("prune: report retained %q: %w", rr.SourcePath, err)
				}
			}
		}
		return res, nil
	}

	if len(toRelink) > 0 {
		applied, retainedByConflict, editHeld, declines, err := p.applyRelinks(ctx, toRelink, hooks.Relinked, hooks.Retained)
		if err != nil {
			return Result{}, err
		}
		res.Relinked = applied
		res.EditHeld = editHeld
		for _, r := range applied {
			ds.relinked(r.OldPath)
		}
		for _, src := range declines.raced {
			ds.retry(src) // a worker raced this relink: look again
		}
		res.RelinkOwned, res.RelinkChanged = declines.owned, declines.changed
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
	scanDeleted, workDeleted, skipped, applied, err := p.deletePruned(ctx, toPrune, hooks.Prune)
	if err != nil {
		return Result{}, err
	}
	// Report actual deletions, not the intended set, so a row that raced into
	// 'processing' (and was therefore skipped) is not counted as pruned.
	res.ScanResults = scanDeleted
	res.WorkItems = workDeleted
	res.PruneSkipped, res.AgedOut = skipped, 0
	for _, row := range applied {
		if row.AgedOut {
			res.AgedOut++
			ds.relinked(row.SourcePath) // no longer a gone row of its directory
		}
	}
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
	// alreadySettled marks a candidate that was ALREADY retired and is only being
	// reconsidered on the chance its target has since been indexed. If such a
	// relink is DECLINED at apply time, the row is exactly as settled as it was
	// before, so it drops out silently rather than being reported: reporting it
	// would re-list it on every sweep forever, which is the churn #732 removed.
	// Nothing is mutated on that path -- the row is already 'done'.
	alreadySettled bool
	// retireIfDeclined carries the retirement plan ACROSS the relink path, and
	// exists because a planned relink can still be declined at apply time (the
	// target turns out to be owned by a different work_queue row, or a work item
	// raced into 'processing').
	//
	// Without it such a row settles nowhere: classify returned outcomeRelink, so
	// the retire branch in reconcile never ran, and applyRelinks turned the
	// decline into a plain RetainedRow. The row then stays 'failed' and
	// dequeue-eligible while pointing at a vanished path -- forever, since every
	// later sweep reaches the identical decline. That is exactly the
	// non-converging worker loop #732 removed, and it is the MOST LIKELY
	// post-reorg state: the rescan that creates the target's scan_results row
	// (which the name tier needs) also enqueues and junction-links it.
	//
	// Named differently from classified.shouldRetire deliberately: classified
	// EMBEDS this struct, and a same-named field would silently shadow rather
	// than conflict.
	retireIfDeclined bool
}

type classified struct {
	outcome  outcome
	retained RetainedRow
	// shouldRetire is the PLAN: this candidate qualifies for retirement. Kept
	// internal and separate from RetainedRow.Retired, which reports what actually
	// committed -- collapsing the two is how a dry run ends up claiming a write it
	// never made.
	shouldRetire bool
	// siblingHeld marks a row the sibling tier stopped on a PRESENT same-name
	// file it would not relink to, or one the name tier would relink. Such a
	// row never ages out.
	siblingHeld bool
	classifiedRelink
}

// classify decides one gone candidate's fate: retain (identity absent,
// ambiguous, or -- under PolicyRelinkOrRetain -- unresolved), relink (identity
// resolves uniquely to an already-present file), or prune (identity present
// but matches nothing anywhere in the library, and policy allows a genuine
// delete).
func (p *Pruner) classify(ctx context.Context, idx *presentIndex, policy Policy, src string, c *candidate) (classified, error) {
	if c.mbid == "" && c.isrc == "" {
		// THE SAME-STEM SIBLING TIER (#1262), ahead of the settled short-circuit
		// (the rows a format swap strands are mostly 'done') and of the name
		// tier: see trySiblingRelink for the order.
		if ok, cls, err := p.trySiblingRelink(ctx, idx.scopeLibrary, policy, src, c); err != nil || ok {
			return cls, err
		}
		// Already settled: gone and no longer work. Re-reporting it every sweep is
		// exactly the churn #732 exists to stop, so it normally drops out of the
		// candidate set here.
		//
		// EXCEPT when it was retired by THIS feature's own sentinel, which is a
		// race the tier would otherwise lose permanently. The tier can only match
		// once a rescan has indexed the moved file, and the periodic sweep is
		// scheduled independently -- so a sweep that runs first finds no same-title
		// file and retires the row. Without this carve-out the row is settled
		// forever after, and the tier never gets to see the target that appeared
		// moments later. The fix's entire premise is that retiring these rows is
		// wrong precisely because a name match can still resolve them, so letting
		// the first sweep lock the tier out would make the whole feature a coin
		// flip against the scan scheduler.
		//
		// Gated on the sentinel, never on 'done' generally: a row that genuinely
		// completed its work must never be resurrected. retireUnresolvable is the
		// only writer of this exact string, so it identifies our own retirements
		// and nothing else -- including retirements by SHIPPED builds, which this
		// therefore also recovers.
		//
		// The same carve-out covers an 'unavailable' row (#477, holdsUnavailable):
		// the tier may relink it, and on a decline it drops out silently below.
		if c.settled && len(c.workItems) > 0 && !c.retiredAsUnresolvable() && !c.holdsUnavailable() {
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
		// THE NAME TIER, consulted BEFORE any terminal decision (#740).
		//
		// "No MBID and no ISRC" rules out the EXACT tier only. It says nothing
		// about a name-based tier, and retiring here without asking one made that
		// route structurally unreachable for exactly the population it would
		// serve. A library reorg (an artist-folder rename) therefore discarded
		// queue state for files still on disk, terminally: measured on one
		// deployment, 49 rows retired in ~2s, 48 of their audio files still
		// present, and none of the 48 carried a work_queue or scan_results row
		// afterward. Unresolved is not unresolvable, and only the latter justifies
		// retirement.
		//
		// WHY THIS TIER IS EXACT-TITLE AND NOT FUZZY, which is the whole safety
		// argument. An earlier revision scored Jaro-Winkler similarity against a
		// confidence floor and a runner-up margin, mirroring realign. Adversarial
		// review demonstrated that this relinks a row onto a DIFFERENT SONG, and
		// that the margin rule structurally cannot prevent it:
		//
		//   - The margin rule only detects ambiguity INSIDE the pool. It cannot
		//     detect the TRUE TARGET BEING ABSENT, which is the normal case in a
		//     reactive pass -- the rescan has not yet indexed the file's new
		//     location, so the nearest thing present is a different song. Its
		//     rivals score ~0.5, so the margin is wide and the verdict is Unique.
		//   - Measured similarities: a title against its plural scored 0.983,
		//     against a spaced variant 0.983, against a "(Live)" variant 0.922 --
		//     all far above any workable floor. Live/remix/alternate-take variants
		//     are the COMMON library shape, so no threshold separates them.
		//
		// A wrong relink is silent, permanent, and strictly worse than the retain
		// it replaces (a retain is inert and reported). So the predicate is
		// EXACT NORMALIZED TITLE EQUALITY: same song or no answer. That still
		// resolves #740, whose scenario is a PATH change with the tags intact.
		//
		// The similarity resolver is still what adjudicates the filtered set --
		// every survivor has an identical normalized title and therefore scores
		// 1.0, so a lone survivor is Unique and two or more trip the margin rule
		// as a Conflict. That is exactly the wanted semantics, and it keeps one
		// shared definition of a name verdict rather than a second private one.
		nameHeld := false
		if policy == policyInFolder {
			var err error
			if nameHeld, _, _, err = p.tryNameRelink(ctx, idx, PolicyFull, src, c); err != nil {
				return classified{}, err
			}
		}
		reason := "identity absent, and no present file shares this title; never deleted on a guess"
		if ok, why, cls, err := p.tryNameRelink(ctx, idx, policy, src, c); err != nil {
			// NEVER swallow this error. The exact tier below propagates its pool
			// failure, and doing otherwise here converted a transient DB fault into
			// a PERMANENT retirement -- the precise damage #740 exists to undo,
			// reachable from an I/O blip -- while reporting a "no match" reason that
			// was never actually established.
			return classified{}, err
		} else if ok {
			return cls, nil
		} else if why != "" {
			reason = why
		}
		// An ALREADY-RETIRED row that the tier just declined drops back out
		// silently. It was only reconsidered on the chance that its target had
		// since been indexed; with no match it is exactly as settled as before, and
		// reporting it on every sweep would reinstate the per-sweep churn #732
		// removed -- for a population that is, by construction, every row this
		// feature has ever retired. Nothing is mutated: it is already 'done'.
		// An 'unavailable' row (#477) is treated identically: already out of the
		// eligible set, so a declined tier leaves it untouched and uncounted.
		if c.settled && (c.retiredAsUnresolvable() || c.holdsUnavailable()) {
			return classified{outcome: outcomeSettled, siblingHeld: nameHeld}, nil
		}
		return classified{
			outcome:     outcomeRetain,
			siblingHeld: nameHeld,
			retained: RetainedRow{
				SourcePath: src,
				// The reason distinguishes WHY the tier declined -- not eligible,
				// no same-title file, several, or a winner that had itself vanished.
				// One flat string for all four left an operator unable to tell a
				// genuine no-match from a suppressed condition.
				Reason: reason,
			},
			// Only a row that is still WORK can be retired. A settled candidate has
			// every linked item in 'done' already -- either never queued, or retired by
			// an earlier sweep -- so there is nothing to take out of the eligible set.
			// This is the PLAN; RetainedRow.Retired records what actually committed.
			shouldRetire: policy == PolicyFull && !c.settled && len(c.workItems) > 0,
			// Carry the candidate so the retire step can reach its work_queue rows.
			// The embedded classifiedRelink is otherwise only populated on the relink
			// path, and a nil c here made retireUnresolvable a silent no-op.
			classifiedRelink: classifiedRelink{src: src, c: c},
		}, nil
	}
	if policy == policyInFolder {
		// No pool here: it stats every identity-bearing file in the library, and
		// a retained row would buy that again on each examination (#1262).
		if ok, cls, err := p.trySiblingRelink(ctx, idx.scopeLibrary, policy, src, c); err != nil || ok {
			return cls, err
		}
		return classified{outcome: outcomeRetain, retained: RetainedRow{SourcePath: src, Reason: "gone inside a surviving directory with no same-name file with another extension indexed beside it; the periodic sweep neither deletes it nor searches the library by MBID/ISRC (`scan reconcile-paths` does)", MBID: c.mbid, ISRC: c.isrc}}, nil
	}
	pool, err := idx.pool(ctx, c.libraryID)
	if err != nil {
		return classified{}, err
	}
	verdict, ref := identity.ResolveExact(c.mbid, c.isrc, p.identityKeys, pool)
	if verdict != identity.VerdictUnique {
		// A re-rip often changes or drops the tags: the in-place swap is
		// consulted before the row is deleted or retained on this verdict (#1262).
		if ok, cls, err := p.trySiblingRelink(ctx, idx.scopeLibrary, policy, src, c); err != nil || ok {
			return cls, err
		}
	}
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
			return classified{outcome: outcomeRetain, retained: RetainedRow{SourcePath: src, Reason: "identity present but not yet found elsewhere; this pass never deletes (a removed directory or `scan reconcile-paths` does)", MBID: c.mbid, ISRC: c.isrc}}, nil
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
func (p *Pruner) applyRelinks(ctx context.Context, targets []classifiedRelink, reportRelinked func(RelinkedRow) error, reportRetained func(RetainedRow) error) (applied []RelinkedRow, retained []RetainedRow, editHeld int, declines relinkDeclines, retErr error) {
	// Retirements owed by DECLINED relinks, applied after the transaction commits
	// (retireUnresolvable runs against p.db and would deadlock against tx on
	// SQLite). idx points at the RetainedRow this retirement belongs to, so the
	// committed result can be stamped onto the right row rather than guessed at.
	type retireePlan struct {
		c   *candidate
		idx int
	}
	var toRetire []retireePlan

	// The transaction is retried whole on SQLITE_BUSY (#978). Nothing in it has
	// an effect outside the database (every report and retirement runs after the
	// commit), so each attempt starts from empty accumulators and a rolled-back
	// attempt leaves no trace.
	if err := dbpkg.RetryBatchTx(ctx, "prune relink", func() error {
		applied, retained, toRetire, editHeld, declines = nil, nil, nil, 0, relinkDeclines{}
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("prune: begin relink tx: %w", err)
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
				return fmt.Errorf("prune: savepoint relink: %w", err)
			}
			decision, err := relinkOne(ctx, tx, cg.c, cg.target)
			if err != nil {
				return err
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
					return fmt.Errorf("prune: rollback relink savepoint: %w", err)
				}
				// Counted before the silent drop below (#1262).
				if decision.owned {
					declines.owned++
				} else {
					declines.changed++
					declines.raced = append(declines.raced, cg.src)
				}
				// An already-retired row whose reconsidered relink was declined drops
				// out silently: it is exactly as settled as before, nothing was
				// mutated, and reporting it would re-list it on every future sweep.
				if cg.alreadySettled {
					if _, err := tx.ExecContext(ctx, "RELEASE "+sp); err != nil { //nolint:gosec // reason: sp is a fixed prefix plus a loop index, never external input
						return fmt.Errorf("prune: release relink savepoint: %w", err)
					}
					continue
				}
				row := RetainedRow{
					SourcePath: cg.relinked.OldPath,
					Reason:     decision.reason,
					MBID:       cg.relinked.MBID,
					ISRC:       cg.relinked.ISRC,
				}
				// A declined relink still has to SETTLE, or the row stays
				// dequeue-eligible pointing at a vanished path and every later sweep
				// reaches the identical decline (#732's non-converging loop). The plan
				// was computed by the tier; carrying it here is what makes the retire
				// branch reachable from the relink path at all.
				//
				// Deferred until after the commit rather than run inside tx:
				// retireUnresolvable executes against p.db, so calling it here would
				// deadlock against this transaction on SQLite. Collected now, applied
				// below.
				if cg.retireIfDeclined {
					toRetire = append(toRetire, retireePlan{c: cg.c, idx: len(retained)})
				}
				retained = append(retained, row)
			} else {
				applied = append(applied, cg.relinked)
				// Counted only for a candidate that keeps its writes: a
				// declined one rolls its edit-held rows back with the rest
				// (CodeRabbit 4162619974).
				editHeld += decision.editHeld
			}
			if _, err := tx.ExecContext(ctx, "RELEASE "+sp); err != nil { //nolint:gosec // reason: sp is a fixed prefix plus a loop index, never external input
				return fmt.Errorf("prune: release relink savepoint: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("prune: commit relink tx: %w", err)
		}
		return nil
	}); err != nil {
		return nil, nil, 0, relinkDeclines{}, err
	}
	// Settle every declined relink, now that tx is committed and the row's
	// pre-decline writes are rolled back. Stamped onto the RetainedRow BEFORE it
	// is reported, so a hook never sees a row whose Retired flag is about to
	// change. Retired reflects what the UPDATE actually affected -- a row that
	// raced into 'processing' or 'done' is left alone by the status guard and
	// correctly reports false.
	for _, plan := range toRetire {
		retired, err := p.retireUnresolvable(ctx, plan.c)
		if err != nil {
			return nil, nil, 0, relinkDeclines{}, fmt.Errorf("prune: retire declined relink %q: %w", retained[plan.idx].SourcePath, err)
		}
		retained[plan.idx].WouldRetire = true
		retained[plan.idx].Retired = retired
	}
	if reportRelinked != nil {
		for _, rr := range applied {
			if err := reportRelinked(rr); err != nil {
				return applied, retained, editHeld, declines, fmt.Errorf("prune: report relinked %q: %w", rr.OldPath, err)
			}
		}
	}
	if reportRetained != nil {
		for _, rr := range retained {
			if err := reportRetained(rr); err != nil {
				return applied, retained, editHeld, declines, fmt.Errorf("prune: report retained %q: %w", rr.SourcePath, err)
			}
		}
	}
	return applied, retained, editHeld, declines, nil
}

// relinkDeclines tallies the planned relinks one applyRelinks call declined, by
// reason, including the already-settled ones it drops without a RetainedRow.
// raced names the sources behind changed, so the caller retries exactly those.
type relinkDeclines struct {
	owned, changed int
	raced          []string
}

// planRelinks is the dry run's stand-in for applyRelinks: the same ownership
// check, mutating nothing. claimed carries the links an earlier relink in this
// batch would create, which the apply sees as written rows by the time it
// reaches a later candidate. The apply's other decline (a row claimed or
// changed after it was read) is a race with a worker and cannot be previewed.
func (p *Pruner) planRelinks(ctx context.Context, targets []classifiedRelink) (planned []RelinkedRow, retained []RetainedRow, owned int, retErr error) {
	claimed := map[int64][]int64{}
	for _, cg := range targets {
		own := cg.c.ownIDs()
		reason, err := ownershipConflict(ctx, p.db, cg.target, own, claimed)
		if err != nil {
			return nil, nil, 0, err
		}
		if reason == "" {
			planned = append(planned, cg.relinked)
			for _, srID := range cg.target.linkIDs() {
				for id := range own {
					claimed[srID] = append(claimed[srID], id)
				}
			}
			continue
		}
		owned++
		if cg.alreadySettled {
			continue
		}
		retained = append(retained, RetainedRow{
			SourcePath: cg.relinked.OldPath, Reason: reason,
			MBID: cg.relinked.MBID, ISRC: cg.relinked.ISRC,
			WouldRetire: cg.retireIfDeclined,
		})
	}
	return planned, retained, owned, nil
}

// ownIDs is the set of this candidate's own work_queue row ids.
func (c *candidate) ownIDs() map[int64]bool {
	own := make(map[int64]bool, len(c.workItems))
	for _, w := range c.workItems {
		own[w.id] = true
	}
	return own
}

// ownershipConflict returns the decline reason when a scan_results row the
// relink would link is already linked (in the database, or by an earlier
// planned relink in claimed) to a work_queue row outside own, else "". A
// same-stem sibling target (keepOutput) is also owned by any other row whose
// source_path already is that file, junction-linked or not: moving a second
// row onto it would leave two rows sharing one source path.
func ownershipConflict(ctx context.Context, q rowsQuerier, target presentRowDetail, own map[int64]bool, claimed map[int64][]int64) (string, error) {
	const reason = "present-file candidate already linked to work_queue row %d; merging is identityrepair's job"
	if len(own) == 0 {
		return "", nil
	}
	if target.keepOutput {
		var at []int64
		rows, err := q.QueryContext(ctx, `SELECT id FROM work_queue WHERE source_path = ? ORDER BY id`, target.filePath)
		if err != nil {
			return "", fmt.Errorf("prune: read rows at the sibling: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return "", fmt.Errorf("prune: scan row at the sibling: %w", err)
			}
			at = append(at, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return "", fmt.Errorf("prune: read rows at the sibling: %w", err)
		}
		for _, id := range at {
			if !own[id] {
				return fmt.Sprintf(reason, id), nil
			}
		}
	}
	for _, srID := range target.linkIDs() {
		for _, id := range claimed[srID] {
			if !own[id] {
				return fmt.Sprintf(reason, id), nil
			}
		}
		otherID, found, err := foreignOwner(ctx, q, srID, own)
		if err != nil {
			return "", err
		}
		if found {
			return fmt.Sprintf(reason, otherID), nil
		}
	}
	return "", nil
}

// trySiblingRelink is the same-stem sibling tier (#1262): a gone source whose
// directory holds exactly ONE present audio file with the same stem and another
// extension (an album re-ripped from .mp3 to .flac in place) is relinked to it.
// It covers what queue.RepointGoneSource's one-shot repair misses: a swap whose
// old file still existed when the replacement was scanned.
//
// ORDER. A unique MBID/ISRC match wins; this tier runs when that does not
// resolve, and BEFORE the name tier, which picks the lone same-title file
// anywhere in the library (a compilation's copy, say) while the sibling is the
// one path that replaces the source without moving its sidecar. Unlike the name
// tier it also runs in the reactive pass and for settled rows.
//
// EVIDENCE. Siblings come from scan_results by an index range on file_path,
// never a directory read; one no scan has indexed yet is not seen, and the row
// waits. Each is statted once: a definitely-gone one is ignored, any other stat
// failure RETAINS the row (an unreadable sibling is where a delete is least
// justified), and two or more present siblings are ambiguous. A sibling whose
// MBID or ISRC differs from the gone row's own is another recording: retained.
// A sibling another queue row owns is retained too, as the apply's decline is.
// Every one of these is a HARD STOP: once a present sibling is found the row
// is never deleted or handed to the name tier, which would relink it elsewhere.
// A key on one side only is no contradiction, and a differing TITLE is
// deliberately not one either, since a re-rip is routinely retitled. A source
// with no queue row at all is not this tier's: nothing would move, so it keeps
// the outcome it had before. The relink keeps outdir, filename and
// output_paths (the sidecar did not move) and links every library's row for
// the sibling. A library-scoped run (lib non-nil) sees, links and checks only
// that library's rows; a sibling indexed only in another library is not seen.
func (p *Pruner) trySiblingRelink(ctx context.Context, lib *int64, policy Policy, src string, c *candidate) (bool, classified, error) {
	if len(c.workItems) == 0 {
		return false, classified{}, nil
	}
	noIdentity := c.mbid == "" && c.isrc == ""
	retain := func(reason string) classified {
		return classified{
			outcome:  outcomeRetain,
			retained: RetainedRow{SourcePath: src, Reason: reason, MBID: c.mbid, ISRC: c.isrc},
			// An identity-less row that is still work is retired as well, as in
			// classify's own retain: left eligible, it is re-fetched forever (#732).
			shouldRetire:     policy == PolicyFull && !c.settled && noIdentity,
			siblingHeld:      true,
			classifiedRelink: classifiedRelink{src: src, c: c},
		}
	}
	// stop is retain for a decline that used to fall through: an identity-less
	// row that is already settled drops out unreported, as it did then.
	stop := func(reason string) classified {
		if c.settled && noIdentity {
			return classified{outcome: outcomeSettled, siblingHeld: true}
		}
		return retain(reason)
	}
	var scopeArg any
	if lib != nil {
		scopeArg = *lib
	}
	differs := func(own, other string) bool {
		return own != "" && other != "" && !strings.EqualFold(strings.TrimSpace(own), strings.TrimSpace(other))
	}
	prefix := strings.TrimSuffix(src, filepath.Ext(src)) + "."
	rowsByPath := map[string][]int64{}
	contradicts := map[string]bool{}
	var paths []string
	if err := queryRows(ctx, p.db,
		`SELECT id, file_path, recording_mbid, isrc FROM scan_results
         WHERE file_path >= ? AND file_path < ? AND (? IS NULL OR library_id = ?)
         ORDER BY file_path, id`,
		[]any{prefix, prefix[:len(prefix)-1] + "/", scopeArg, scopeArg}, func(rows *sql.Rows) error {
			var id int64
			var path, mbid, isrc string
			if err := rows.Scan(&id, &path, &mbid, &isrc); err != nil {
				return err
			}
			if !queue.SameStemSibling(path, src) {
				return nil
			}
			if differs(c.mbid, mbid) || differs(c.isrc, isrc) {
				contradicts[path] = true
			}
			if _, seen := rowsByPath[path]; !seen {
				paths = append(paths, path)
			}
			rowsByPath[path] = append(rowsByPath[path], id)
			return nil
		}); err != nil {
		return false, classified{}, fmt.Errorf("prune: read same-stem siblings: %w", err)
	}
	var live []string
	for _, path := range paths { // statted only after the rows are closed
		_, err := p.stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return true, retain("a same-name file with another extension could not be read; kept until it can be"), nil
		}
		live = append(live, path)
	}
	if len(live) == 0 {
		return false, classified{}, nil
	}
	if len(live) > 1 {
		return true, stop("several present files share this file's name with another extension; never picked one on a guess"), nil
	}
	if contradicts[live[0]] {
		return true, retain("the same-name file with another extension carries a different MBID or ISRC; a different recording, never relinked"), nil
	}
	ids := rowsByPath[live[0]]
	target := presentRowDetail{scanResultID: ids[0], filePath: live[0], keepOutput: true, alsoLink: ids[1:]}
	// A sibling with its own queue row (junction-linked to it, or already sitting
	// at it by source_path) is a different track as far as the queue can tell.
	// A hard stop, matching the apply's own ownership decline: falling through
	// deleted an identity-bearing row beside its replacement and let the name
	// tier move an identity-less one onto some other same-title file.
	if conflict, err := ownershipConflict(ctx, p.db, target, c.ownIDs(), nil); err != nil {
		return false, classified{}, err
	} else if conflict != "" {
		return true, stop("the same-name file with another extension already belongs to a different queue row; merging is identityrepair's job"), nil
	}
	// MBID/ISRC stay empty: no identity drove this match.
	rr := RelinkedRow{OldPath: src, NewPath: live[0], ScanResultIDs: c.scanResultIDs}
	for _, w := range c.workItems {
		rr.WorkItemIDs = append(rr.WorkItemIDs, w.id)
	}
	return true, classified{
		outcome: outcomeRelink,
		classifiedRelink: classifiedRelink{
			src: src, c: c, relinked: rr, target: target, alreadySettled: c.settled,
			// A declined identity-less row still has to settle, as in the name tier.
			retireIfDeclined: policy == PolicyFull && !c.settled && noIdentity,
		},
	}, nil
}

// relinkDecision is relinkOne's verdict for one candidate. A zero value means
// the relink was performed; a non-empty reason means it was declined and the
// caller must roll the candidate back and report it as retained.
type relinkDecision struct {
	reason string
	// owned marks a decline for an ownership conflict, as opposed to a row that
	// was claimed or changed after it was read.
	owned bool
	// editHeld counts this candidate's rows relinked WITHOUT the resurrect
	// because they carry the hand-edit mark (#1228). Meaningful only when
	// reason is empty; a declined candidate is rolled back whole.
	editHeld int
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
	if reason, err := ownershipConflict(ctx, tx, target, c.ownIDs(), nil); err != nil {
		return relinkDecision{}, err
	} else if reason != "" {
		return relinkDecision{reason: reason, owned: true}, nil
	}
	// A row this package RETIRED is relinking back out of its terminal state, so
	// the retirement stamps have to come off with it: leaving status='done' would
	// repoint the row at a file the worker will never pick up, which is the same
	// undone-work the retirement caused.
	//
	// Restored to 'pending', NOT 'failed'. A relink is not a fetch outcome -- the
	// row has never been attempted at its new path, so nothing failed, and
	// resurrecting it as 'failed' smuggled a claim about RETRY POSTURE through a
	// column that also drives REPORTING (#789): reports.RecentOutcomes and
	// FailureAnalysis both read status/outcome_type to describe what the FETCHER
	// did, and 'failed' told them a fetch had failed when none had ever run.
	// attempts and next_attempt_at are reset explicitly (attempts=0, next_attempt_at
	// = now) rather than carried over from the retirement -- that IS the retry
	// posture the old comment wanted to preserve, and it belongs in those columns,
	// not smuggled through status. completed_at and last_error are cleared so the
	// row does not read as settled to any report that joins on them.
	//
	// Applied ONLY to our own sentinel-retired rows (see retiredAsUnresolvable),
	// so a genuinely completed row is never resurrected by a relink.
	//
	// priority is deliberately NOT reset here. A row retired while 'deferred'
	// carries priority=-100 from that miss, and resurrecting it to 'pending'
	// without touching priority reproduces the pending+(-100)+miss_count>0 shape
	// migration 030 (internal/db/migrations/030_work_queue_prev_status.sql)
	// describes and repairs -- but that migration's own reasoning is that the
	// shape is dequeue-inert (pending and deferred at priority -100 match the
	// same dequeue predicate at the same priority), only invisible to
	// RecheckDeferred/`queue deferred`, so leaving it alone here is consistent
	// with that conclusion rather than a gap in this fix.
	//
	// outcome_type, outcome_detail and timing_outcome ARE cleared, for the same
	// reason status is: they describe a fetch, and no fetch has happened at the
	// new path. reports.CountInstrumental counts outcome_type = 'instrumental'
	// with NO status filter, so a stale stamp would keep a row counted as a
	// settled instrumental while it sits in 'pending' waiting to be re-fetched.
	// prune's own retire UPDATE excludes rows already in 'done', so a completed
	// fetch's outcome cannot arrive here by that route -- but purgeprovenance.
	// resetRows and queue.RecheckRetired both move a settled row OUT of 'done'
	// without clearing these columns, and such a row is then eligible for the
	// retirement above. The two sibling unsettle paths, queue.ResetInstrumental
	// and queue.UnsettleInstrumental, already NULL outcome_type for exactly this
	// reason; this matches them.
	//
	// A resurrected row also leaves word-recheck mode (#1039): it is reopened
	// for an ordinary fetch, and a 'queued' state would route it to the recheck
	// path, which writes nothing without words and settles it straight back.
	//
	// A HAND-EDITED row is never resurrected (#1228): reopening it to 'pending'
	// would re-fetch over the edit. relinkResurrectSQL carries the mark guard, so
	// an edited row matches nothing there and takes relinkPathSQL+relinkEditedOnly
	// instead: its path still moves (it stays attached to its file), its status
	// and stamps do not. The guard is in SQL, inside this write transaction, so
	// an editor save that set the mark after gather is honored too. Prune never
	// touches a sidecar, so an edited .lrc that did not move with its audio is
	// left exactly where it is.
	var decision relinkDecision
	resurrect := c.retiredAsUnresolvable()
	resurrectNow := time.Now().UTC().Format(timeFormat)
	for _, w := range c.workItems {
		var outputPathsJSON []byte
		var err error
		if target.keepOutput {
			// A same-stem sibling relink (#1262): the sidecar did not move.
			target.outdir, target.filename, outputPathsJSON = w.inputs.Outdir, w.inputs.Filename, []byte(w.rawOutputPaths)
		} else if outputPathsJSON, err = json.Marshal(relinkOutputPaths(w.inputs.OutputPaths, w.inputs.Outdir, w.inputs.Filename, target)); err != nil {
			return relinkDecision{}, fmt.Errorf("prune: marshal relinked output_paths for work_queue %d: %w", w.id, err)
		}
		var res sql.Result
		if resurrect {
			res, err = tx.ExecContext(ctx,
				relinkResurrectSQL,
				target.filePath, target.outdir, target.filename, string(outputPathsJSON), resurrectNow, w.id, w.rawOutputPaths)
			if err == nil && rowsAffected(res) == 0 {
				res, err = tx.ExecContext(ctx, relinkPathSQL+relinkEditedOnly,
					target.filePath, target.outdir, target.filename, string(outputPathsJSON), w.id, w.rawOutputPaths)
				if err == nil && rowsAffected(res) > 0 {
					decision.editHeld++
				}
			}
		} else {
			res, err = tx.ExecContext(ctx, relinkPathSQL,
				target.filePath, target.outdir, target.filename, string(outputPathsJSON), w.id, w.rawOutputPaths)
		}
		if err != nil {
			return relinkDecision{}, fmt.Errorf("prune: relink work_queue %d: %w", w.id, err)
		}
		if rowsAffected(res) == 0 {
			// The row raced into 'processing', vanished, or had its output_paths
			// rewritten after gather (the relinked list above was derived from the
			// gathered snapshot, so writing it over a newer value would lose that
			// update). Decline the whole candidate rather than deleting its
			// scan_results row out from under a work_queue row still pointing at
			// the gone path.
			return relinkDecision{reason: fmt.Sprintf("work_queue row %d became in-flight, vanished, or changed since it was read before the relink could apply; kept for a later pass", w.id)}, nil
		}
		for _, srID := range target.linkIDs() {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
				w.id, srID); err != nil {
				return relinkDecision{}, fmt.Errorf("prune: link work_queue %d to scan_result %d: %w", w.id, srID, err)
			}
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
	return decision, nil
}

// relinkOutputPaths returns a work_queue row's output_paths, rewritten so a
// relink's new destination is actually consulted at write time (#921).
//
// internal/worker's outputPaths() prefers a non-empty output_paths verbatim
// over the outdir/filename pair the relink UPDATE otherwise keeps in sync, so
// leaving output_paths untouched left every relinked row writing to the
// vanished pre-move directory forever -- the writer refuses (the directory no
// longer exists), the worker w.fail()s, and 'failed' is dequeue-eligible, so
// the row re-fetches from the provider on every backoff expiry with no way to
// ever succeed.
//
// output_paths can carry more than one entry: internal/identityrepair's
// mergeQueueRows unions two work_queue rows' output_paths when their
// (artist_key, title_key) collide (e.g. a duplicate track cataloged under two
// scan_results rows), so a single work_queue row can legitimately name several
// real write destinations, not all of which correspond to THIS candidate's
// source_path. Only the entry that matches the row's PRE-relink location
// (oldOutdir, oldFilename -- the outdir/filename this same UPDATE is replacing)
// is rewritten to the new one; every other entry is preserved untouched, since
// it names a destination this relink has no information about and no license
// to alter or drop.
//
// When NOTHING matches the pre-relink location (an empty list included), the
// row's location columns and its output_paths already disagreed before this
// relink ran -- e.g. a row a prior build's relink already moved once, or a
// merged row whose outdir/filename name only one of its unioned entries. The
// list can then hold both stale entries and real destinations belonging to a
// merged row, and nothing in the row says which is which. The only evidence
// consulted is the filesystem: an entry is dropped ONLY when os.Stat on its
// Outdir reports fs.ErrNotExist. Any other stat error (permission denied, an
// unavailable mount) keeps the entry, because a transient failure must never
// delete a destination. The new destination is then appended and the result
// de-duplicated by (Outdir, Filename), keeping first occurrence order. A kept
// entry whose directory exists may still be stale; this pass cannot tell, and
// does not claim to.
//
// The stat calls run only here, i.e. only for rows actually being relinked.
func relinkOutputPaths(existing []models.OutputPath, oldOutdir, oldFilename string, target presentRowDetail) []models.OutputPath {
	newEntry := models.OutputPath{Outdir: target.outdir, Filename: target.filename}
	out := make([]models.OutputPath, len(existing))
	copy(out, existing)
	matched := false
	for i, p := range out {
		if p.Outdir == oldOutdir && p.Filename == oldFilename {
			out[i] = newEntry
			matched = true
		}
	}
	if matched {
		return dedupeOutputPaths(out)
	}
	kept := make([]models.OutputPath, 0, len(existing)+1)
	for _, p := range existing {
		if _, err := os.Stat(p.Outdir); err != nil && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		kept = append(kept, p)
	}
	kept = append(kept, newEntry)
	return dedupeOutputPaths(kept)
}

// dedupeOutputPaths removes repeated (Outdir, Filename) pairs, keeping first
// occurrence order. Both branches of relinkOutputPaths need it: a merged row can
// already hold the new destination beside the entry being rewritten.
func dedupeOutputPaths(paths []models.OutputPath) []models.OutputPath {
	seen := make(map[models.OutputPath]bool, len(paths))
	deduped := paths[:0]
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		deduped = append(deduped, p)
	}
	return deduped
}

// foreignOwner reports whether scanResultID is junction-linked to any
// work_queue row NOT in own -- the candidate's own set of aggregated work
// items. Filtering in Go rather than building a variadic NOT IN keeps the
// query a single fixed statement; the junction row count per scan_result is
// tiny (normally one), so the scan is trivial.
func foreignOwner(ctx context.Context, tx rowsQuerier, scanResultID int64, own map[int64]bool) (int64, bool, error) {
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

// rowsQuerier is the read both *sql.DB (the dry run) and *sql.Tx (the apply)
// offer, so one ownership check serves both.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// presentRowDetail is the detail applyRelinks needs from the present-file
// scan_results row an identity match resolved to.
type presentRowDetail struct {
	scanResultID int64
	filePath     string
	outdir       string
	filename     string
	// keepOutput leaves each row's outdir, filename and output_paths alone;
	// alsoLink is the other libraries' rows for the file. Sibling tier only.
	keepOutput bool
	alsoLink   []int64
}

// linkIDs is every scan_results row a relink to this target links.
func (d presentRowDetail) linkIDs() []int64 {
	return append([]int64{d.scanResultID}, d.alsoLink...)
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
	// byScopeWide caches the HEURISTIC pool: every present file in the scope,
	// including the identity-less majority the exact tier can never match. Kept
	// separate from byScope so the exact tier keeps its narrow, seekable pool and
	// pays none of this cost (#740).
	byScopeWide map[int64][]identity.Candidate
	detailsWide map[int64]map[string]presentRowDetail
	// scopeLibrary is the RUN's requested library (SweepOptions.LibraryID), nil
	// when unscoped. Not candidate.libraryID, which is the row's own library.
	scopeLibrary *int64
	stat         func(string) (fs.FileInfo, error)
}

func newPresentIndex(db *sql.DB, roots []string) *presentIndex {
	return &presentIndex{
		stat:        os.Stat,
		db:          db,
		roots:       roots,
		byScope:     map[int64][]identity.Candidate{},
		details:     map[int64]map[string]presentRowDetail{},
		byScopeWide: map[int64][]identity.Candidate{},
		detailsWide: map[int64]map[string]presentRowDetail{},
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
		if !underAvailableRoot(path, idx.roots) || !pathExists(idx.stat, path) {
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

// poolHeuristic returns EVERY present file in the scope as a name-scored
// candidate, for the heuristic tier (#740). Built and cached separately from
// pool(), and LAZILY -- a sweep with no identity-less gone candidate never calls
// it and pays nothing.
//
// WHY A SECOND POOL RATHER THAN WIDENING THE FIRST. pool() filters to
// identity-bearing rows because only those can win the exact tier, and that
// filter is what keeps its os.Stat cost bounded. The heuristic tier needs the
// OPPOSITE population: an identity-less orphan is most likely to match an
// identity-less present file -- a file whose tags lack MBID/ISRC is exactly why
// its row had no identity to begin with. Measured on one deployment: 15,025
// present files carry identity and 51,409 do not, so consulting the narrow pool
// would have scored against a set that structurally cannot contain the answer.
// The fix would have looked correct, passed its tests, and relinked nothing.
//
// NO PER-ROW os.Stat, and that is the load-bearing difference. pool() stats
// every candidate so a gone row cannot be relinked onto another gone file. Doing
// that here would stat the WHOLE library scope: 54,936 files on the largest
// library measured, against 11,404 today -- a 4.8x increase in disk wakeups on a
// spun-down array, for a pool that is mostly irrelevant to any single orphan.
// Instead the caller stats ONLY THE WINNER after scoring, which buys the same
// safety for one stat rather than tens of thousands. Scoring itself is pure
// in-memory string work over a single indexed query.
//
// underAvailableRoot is still applied per row: it is an in-memory prefix check
// against the configured roots, costs no I/O, and keeps a row from an unmounted
// library out of the candidate set.
func (idx *presentIndex) poolHeuristic(ctx context.Context, libraryID *int64) ([]identity.Candidate, error) {
	key := presentIndexGlobalKey
	if libraryID != nil {
		key = *libraryID
	}
	if pool, ok := idx.byScopeWide[key]; ok {
		return pool, nil
	}
	query := `SELECT id, file_path, outdir, filename, artist, title FROM scan_results
              WHERE file_path != ''`
	var args []any
	if libraryID != nil {
		query += ` AND library_id = ?`
		args = append(args, *libraryID)
	}
	var pool []identity.Candidate
	details := map[string]presentRowDetail{}
	if err := queryRows(ctx, idx.db, query, args, func(rows *sql.Rows) error {
		var id int64
		var path, outdir, filename, artist, title string
		if err := rows.Scan(&id, &path, &outdir, &filename, &artist, &title); err != nil {
			return err
		}
		if !underAvailableRoot(path, idx.roots) {
			return nil
		}
		// Stem carries the filename-derived name evidence; Ref stays the opaque
		// path handle the caller round-trips back out of a Unique verdict.
		pool = append(pool, identity.Candidate{Ref: path, Artist: artist, Title: title, Stem: stemOf(path)})
		details[path] = presentRowDetail{scanResultID: id, filePath: path, outdir: outdir, filename: filename}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: build present-file heuristic index: %w", err)
	}
	idx.byScopeWide[key] = pool
	idx.detailsWide[key] = details
	return pool, nil
}

// detailWide returns the present-row detail for ref within the name pool's
// scope, and whether it was found.
//
// The ok flag is load-bearing rather than idiomatic garnish. A bare map index
// returns a ZERO presentRowDetail on a miss -- scanResultID 0 and empty
// outdir/filename -- which relinkOne would then write: junctioning the work item
// to a nonexistent scan_result and blanking the row's output columns. Today
// every ref comes from this same map, so a miss should be unreachable; returning
// the flag means an unreachable case that becomes reachable DECLINES rather than
// silently corrupts.
func (idx *presentIndex) detailWide(libraryID *int64, ref string) (presentRowDetail, bool) {
	key := presentIndexGlobalKey
	if libraryID != nil {
		key = *libraryID
	}
	d, ok := idx.detailsWide[key][ref]
	return d, ok
}

// stemOf is the filename stem used as name evidence when a row carries no
// artist/title tags.
func stemOf(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
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

// identityHeld reports whether another indexed file with src's MBID or ISRC is
// present (or unreadable): one indexed query, a stat per match, no relink.
// A match under no available root cannot be ruled out, so it holds too.
func (p *Pruner) identityHeld(ctx context.Context, src string, c *candidate, roots []string) (bool, error) {
	if c.mbid == "" && c.isrc == "" {
		return false, nil
	}
	var paths []string
	// Each arm repeats its partial index's own predicate so the index is usable.
	if err := queryRows(ctx, p.db, `SELECT DISTINCT file_path FROM scan_results WHERE file_path != ?
        AND ((recording_mbid = ? AND recording_mbid != '') OR (isrc = ? AND isrc != ''))`,
		[]any{src, c.mbid, c.isrc}, func(rows *sql.Rows) error {
			paths = append(paths, "")
			return rows.Scan(&paths[len(paths)-1])
		}); err != nil {
		return false, fmt.Errorf("prune: read same-identity files: %w", err)
	}
	for _, path := range paths { // statted only after the rows are closed
		if !underAvailableRoot(path, roots) || pathExists(p.stat, path) {
			return true, nil
		}
	}
	return false, nil
}

// sharedAcrossLibraries reports whether a queue row of c is also linked to
// another library's scan_results row. work_queue is unique on (artist_key,
// title_key), so two libraries' files of one song share ONE queue row, and
// aging it out for the copy that is gone would delete the row the other
// library's file still uses (and cascade its link). Such a row never ages out,
// on any sweep; it is classified as it was before the age-out (#1293 tracks
// what that classification itself does with a shared row).
// A library-scoped run holds on any link into another library. An unscoped one
// gathered every library's row for src into c, so there only a link to another
// library's row for a DIFFERENT file holds; with no scan_results row of its own
// to name a library, c is not held.
// Asked only of a candidate otherwise ageable: per queue row, one prefix probe
// of the junction's primary key and a rowid lookup per link; no stat.
func (p *Pruner) sharedAcrossLibraries(ctx context.Context, src string, c *candidate, scope *int64) (bool, error) {
	lib, own := scope, ""
	if lib == nil {
		lib, own = c.libraryID, src
	}
	if lib == nil {
		return false, nil
	}
	for _, w := range c.workItems {
		var shared bool
		if err := p.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_queue_scan_results j WHERE j.work_queue_id = ?
            AND EXISTS (SELECT 1 FROM scan_results sr WHERE sr.id = j.scan_result_id AND sr.library_id != ? AND sr.file_path != ?))`,
			w.id, *lib, own).Scan(&shared); err != nil {
			return false, fmt.Errorf("prune: read other libraries' links: %w", err)
		}
		if shared {
			return true, nil
		}
	}
	return false, nil
}

// replacementOnDisk reads src's directory once and reports whether an age-out
// must be refused: it is unreadable or empty, or an entry that is not a lyric
// sidecar shares src's stem (case-insensitive), i.e. an unindexed replacement.
func (p *Pruner) replacementOnDisk(src string, seen map[string][]os.DirEntry) bool {
	entries, ok := seen[filepath.Dir(src)]
	if !ok { // read once per sweep; a failed read refuses whatever it returned
		if l, err := p.readDir(filepath.Dir(src)); err == nil {
			entries = l
		}
		seen[filepath.Dir(src)] = entries
	}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".lrc" && ext != ".elrc" && ext != ".txt" && strings.EqualFold(stemOf(e.Name()), stemOf(src)) {
			return true
		}
	}
	return len(entries) == 0
}

// AgingCounts reports how many gone sources the periodic sweep is holding for
// its grace period, how many are past it (deleted once two sweeps at least
// goneConfirmGap apart both still find them gone), and how many the tripped
// breaker holds (heldMark): no sweep deletes those, however few remain held,
// only the attended run (`scan reconcile-paths --yes`).
func (p *Pruner) AgingCounts(ctx context.Context) (aging, due, held int, err error) {
	// A mark whose rows another pass already removed is not counted.
	err = p.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(first_seen <= ?1 AND confirmed_at IS NOT ?2), 0),
               COALESCE(SUM(confirmed_at IS ?2), 0) FROM prune_gone_since g
        WHERE EXISTS (SELECT 1 FROM scan_results WHERE file_path = g.path)
           OR EXISTS (SELECT 1 FROM work_queue WHERE source_path = g.path)`,
		p.now().Add(-goneGrace).Unix(), heldMark).Scan(&aging, &due, &held)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("prune: count aging sources: %w", err)
	}
	return aging - due - held, due, held, nil
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
// for a destructive operation. offline are the configured roots that are not.
func (p *Pruner) availableRoots(ctx context.Context) (roots, offline []string, _ error) {
	if err := queryRows(ctx, p.db, `SELECT path FROM libraries WHERE path != ''`, nil, func(rows *sql.Rows) error {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		if dirPopulated(path) {
			roots = append(roots, path)
		} else {
			offline = append(offline, path)
		}
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("prune: load library roots: %w", err)
	}
	return roots, offline, nil
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
// source paths so link-less rows are covered. The int result counts work_queue
// rows skipped because their output_paths is not valid JSON (#937): such a row
// is left untouched rather than aborting the sweep for every other row.
func (p *Pruner) gatherCandidates(ctx context.Context, sc scope, libraryID *int64) (map[string]*candidate, int, error) {
	malformed := 0
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
		return nil, 0, fmt.Errorf("prune: gather scan_results: %w", err)
	}

	const stateCols = `, COALESCE(wq.outcome_type, ''), COALESCE(wq.sync_tier, ''), COALESCE(wq.timing_outcome, ''), COALESCE(wq.lyric_edited_at, '')` // for the backup record
	wqQuery := `SELECT id, artist, title, outdir, filename, source_path, output_paths, status, isrc, mbid, last_error` + stateCols + ` FROM work_queue wq WHERE source_path != ''`
	var wqArgs []any
	if libraryID != nil {
		// Library-scope work_queue through the junction so a scoped sweep only
		// prunes queue rows belonging to that library.
		wqQuery = `SELECT DISTINCT wq.id, wq.artist, wq.title, wq.outdir, wq.filename, wq.source_path, wq.output_paths, wq.status, wq.isrc, wq.mbid, wq.last_error` + stateCols + `
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
		var artist, title, outdir, filename, source, outputPaths, status string
		var isrc, mbid, lastError sql.NullString
		var st WorkState
		if err := rows.Scan(&id, &artist, &title, &outdir, &filename, &source, &outputPaths, &status, &isrc, &mbid, &lastError, &st.OutcomeType, &st.SyncTier, &st.TimingOutcome, &st.LyricEditedAt); err != nil {
			return err
		}
		if !sc.matches(source) {
			return nil
		}
		var paths []models.OutputPath
		if outputPaths != "" {
			if err := json.Unmarshal([]byte(outputPaths), &paths); err != nil {
				malformed++
				slog.Warn("prune: skipping work_queue row with malformed output_paths", "work_queue_id", id, "error", err)
				return nil
			}
		}
		c := ensureCandidate(bySource, source)
		if st.Status = status; status == "processing" {
			c.processing = true
			return nil
		}
		// Count settled vs total linked work items rather than latching a flag. A
		// source is not constrained to one work_queue row (there is no UNIQUE on
		// source_path), so a mix of {done, failed} is representable -- and a latched
		// "any row is done" would classify the whole candidate as settled, dropping
		// it from the sweep while its still-eligible sibling kept being worked,
		// unretired AND unreported. Invisible is worse than permanent.
		//
		// 'unavailable' (#477) counts here too: it is RetireMiss's terminal state
		// for an exhausted benign miss, already out of the dequeue-eligible set, so
		// there is nothing left to retire. (It is still relinkable; see
		// holdsUnavailable.)
		//
		// Settledness is derived once, after the gather completes, from these two
		// counts; see markSettled.
		if status == queue.StatusDone || status == queue.StatusUnavailable {
			c.doneWorkItems++
		}
		c.workItems = append(c.workItems, workRow{
			id: id,
			inputs: models.Inputs{
				Track:       models.Track{ArtistName: artist, TrackName: title},
				Outdir:      outdir,
				Filename:    filename,
				SourcePath:  source,
				OutputPaths: paths,
			},
			lastError:      lastError.String,
			status:         status,
			rawOutputPaths: outputPaths,
			state:          st,
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
		return nil, 0, fmt.Errorf("prune: gather work_queue: %w", err)
	}
	markSettled(bySource)
	return bySource, malformed, nil
}

// markSettled derives candidate.settled once the whole gather is complete.
//
// It cannot be latched during the gather: rows arrive one at a time, so the
// moment a 'done' row is seen there is no way to know whether a later row for
// the same source will be dequeue-eligible. Deriving it here, from the finished
// counts, is what makes "settled" mean EVERY linked work item is done rather
// than "at least one is" -- the weaker reading would drop a candidate that still
// holds eligible work, leaving that row unretired and unreported.
//
// A candidate with no work items at all is NOT settled: there is nothing to
// retire, but nothing has been settled either, and the relink and prune paths
// still need to see it.
func markSettled(bySource map[string]*candidate) {
	for _, c := range bySource {
		c.settled = len(c.workItems) > 0 && c.doneWorkItems == len(c.workItems)
	}
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
// WHY 'done' AND NOT A NEW STATUS. This retirement is for a categorically
// different population than #477's 'unavailable' status: a row here has no
// identity AND its source file is confirmed gone, so it can never be
// relinked, ever -- distinct from an exhausted benign miss, whose source file
// is very much still present and simply never had lyrics found for it. #477
// deliberately scoped its new status to RetireMiss's population only (the
// maintainer's scoping comment: implement the terminal status for
// exhausted-deferred rows and the call-site audit, and leave any wider
// reclassification question untouched); this retirement keeps settling to
// 'done' with its own distinct sentinel (unresolvableGoneError), which is
// already disjoint from missLimitReachedError by text and now also disjoint
// by status.
//
// THE STATUS GUARD IS THE IN-FLIGHT-AND-ALREADY-TERMINAL GUARD.
// `status NOT IN ('processing','done','unavailable')` does two jobs: it never
// retires a row the worker currently owns, and it makes the whole operation
// idempotent, since an already-retired row (by this mechanism OR by
// RetireMiss, #477) no longer matches. 'unavailable' is included here so this
// UPDATE can never clobber a row RetireMiss already retired as an exhausted
// miss -- overwriting its status='unavailable' and last_error=missLimitReached
// with 'done' and this package's own sentinel would erase exactly the
// distinction #477 exists to preserve, for a row that is separately, and
// correctly, already out of the dequeue-eligible set. Idempotence over BOTH
// terminal shapes is the point of the fix -- without it this would relocate
// the non-converging fixed point rather than remove it.
//
// completed_at is stamped because the row IS settled; leaving it null would make
// a retired row look perpetually in-flight to every report that reads it.
// relinkResurrectSQL carries queue.ClearWordRecheckQueued (#1039); a constant,
// so the fragment is joined at compile time. retireUnresolvableSQL deliberately
// does NOT: see retireUnresolvable.
const (
	relinkResurrectSQL = `UPDATE work_queue SET source_path = ?, outdir = ?, filename = ?, output_paths = ?,
                     status = 'pending', attempts = 0, next_attempt_at = ?,
                     completed_at = NULL, last_error = '',
                     outcome_type = NULL, outcome_detail = NULL, timing_outcome = NULL,
                     ` + queue.ClearWordRecheckQueued + `
                 WHERE id = ? AND status != 'processing' AND output_paths IS ?
                   AND lyric_edited_at IS NULL`
	// relinkPathSQL moves only the path columns: an ordinary relink, and (with
	// relinkEditedOnly appended) a hand-edited row a resurrect skipped (#1228).
	relinkPathSQL = `UPDATE work_queue SET source_path = ?, outdir = ?, filename = ?, output_paths = ?
                 WHERE id = ? AND status != 'processing' AND output_paths IS ?`
	relinkEditedOnly      = ` AND lyric_edited_at IS NOT NULL`
	retireUnresolvableSQL = `UPDATE work_queue
             SET status = 'done',
                 completed_at = ?,
                 last_error = ?
             WHERE id = ?
               AND status NOT IN ('processing', 'done', 'unavailable')`
)

func (p *Pruner) retireUnresolvable(ctx context.Context, c *candidate) (bool, error) {
	if c == nil || len(c.workItems) == 0 {
		return false, nil
	}
	now := time.Now().UTC().Format(timeFormat)
	retired := false
	for _, w := range c.workItems {
		// A word-recheck row ('deferred' + 'queued', #982) retired here KEEPS
		// 'queued' (#1039 review): as 'done' it is never dequeued, and 'queued'
		// is the marker the recheck candidate predicate already excludes, so a
		// gone source is never re-flipped. Clearing it would leave done+synced+
		// NULL, a candidate every later --yes wastes provider calls on. The
		// resurrect path (relinkResurrectSQL) clears it when the file returns.
		//
		// Each UPDATE is its own autocommit statement and idempotent (the status
		// guard excludes a row it already retired), so it retries safely on
		// SQLITE_BUSY without disturbing rows an earlier iteration committed (#978).
		var res sql.Result
		err := dbpkg.RetryBatchTx(ctx, "prune retire", func() error {
			var execErr error
			res, execErr = p.db.ExecContext(ctx,
				retireUnresolvableSQL,
				now, unresolvableGoneError, w.id)
			return execErr
		})
		if err != nil {
			return false, fmt.Errorf("retire work item %d: %w", w.id, err)
		}
		if rowsAffected(res) > 0 {
			retired = true
		}
	}
	return retired, nil
}

// deletePruned deletes the planned rows and reports what it deleted. A row
// that is not aged out is reported after its delete commits. An aged-out row
// (#1262) is BACKUP FIRST: reported before the transaction and left alone when
// that report fails; if the delete then leaves any of its rows, a second report
// (Corrects) replaces the first. A hook that answers ErrNotRecorded for an
// aged-out row keeps the row without failing the sweep.
func (p *Pruner) deletePruned(ctx context.Context, pruned []PrunedRow, report func(PrunedRow) error) (scanDeleted, workDeleted, skipped int, applied []PrunedRow, retErr error) {
	var unwritten int // aged-out records, or their corrections, that failed
	var unwrittenFirst error
	tell := func(row PrunedRow) error {
		if report == nil {
			return nil
		}
		err := report(row)
		if err != nil && !errors.Is(err, ErrNotRecorded) {
			if unwritten++; unwrittenFirst == nil {
				unwrittenFirst = err
			}
		}
		return err
	}
	todo := make([]PrunedRow, 0, len(pruned))
	for _, row := range pruned {
		if !row.AgedOut || tell(row) == nil {
			todo = append(todo, row)
		}
	}
	// Retried whole on SQLITE_BUSY (#978). Every other report runs only after the
	// commit, so a rolled-back attempt has written nothing; each attempt resets its tallies.
	txErr := dbpkg.RetryBatchTx(ctx, "prune delete", func() error {
		var err error
		scanDeleted, workDeleted, skipped, applied, err = p.deletePrunedTx(ctx, todo)
		return err
	})
	if txErr != nil {
		applied = nil
	}
	// An aged-out row's record was written ahead: correct it when a row stayed
	// (in flight, moved, or the transaction failed), listing only the ids deleted.
	lost := make(map[string]PrunedRow, len(applied))
	for _, d := range applied {
		lost[d.SourcePath] = d
	}
	for _, row := range todo {
		d := lost[row.SourcePath]
		if row.AgedOut && len(d.WorkItemIDs)+len(d.ScanResultIDs) < len(row.WorkItemIDs)+len(row.ScanResultIDs) {
			d.SourcePath, d.AgedOut, d.Corrects = row.SourcePath, true, true
			_ = tell(d)
		}
	}
	if txErr != nil {
		return 0, 0, 0, nil, txErr
	}
	// Report only after the deletes are durably committed, so a backup record is
	// never written for a row that survived (skipped mid-tx or rolled back).
	// One failure never stops the rest being reported: the deletes stand. The
	// error names no source path, since the unattended sweep logs it.
	var failed, reported int
	var first error
	if report != nil {
		for _, row := range applied {
			if row.AgedOut {
				continue // recorded before its delete
			}
			reported++
			if err := report(row); err != nil {
				if failed++; first == nil {
					first = err
				}
			}
		}
	}
	if failed > 0 {
		retErr = fmt.Errorf("prune: report: %d of %d deleted source(s) not recorded, first failure: %w", failed, reported, first)
	}
	if unwritten > 0 {
		retErr = errors.Join(fmt.Errorf("prune: %d backup record(s) could not be written, first: %w", unwritten, unwrittenFirst), retErr)
	}
	return scanDeleted, workDeleted, skipped, applied, retErr
}

// deletePrunedTx is one attempt of deletePruned's transaction, returning the
// committed tallies, the rows not fully deleted, and the rows that actually
// lost at least one row, each listing only the ids deleted. Both deletes name
// the path the row was READ at: a scan's Enqueue or queue.RepointGoneSource
// may since have moved it to the present file that replaced its source (#1262).
func (p *Pruner) deletePrunedTx(ctx context.Context, pruned []PrunedRow) (scanDeleted, workDeleted, skipped int, applied []PrunedRow, retErr error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("prune: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, row := range pruned {
		before, done := scanDeleted+workDeleted, PrunedRow{SourcePath: row.SourcePath, AgedOut: row.AgedOut}
		for i, id := range row.WorkItemIDs {
			res, err := tx.ExecContext(ctx,
				`DELETE FROM work_queue WHERE id = ? AND status != 'processing' AND source_path = ?`, id, row.SourcePath)
			if err != nil {
				return 0, 0, 0, nil, fmt.Errorf("prune: delete work_queue %d: %w", id, err)
			}
			if rowsAffected(res) > 0 {
				done.WorkItemIDs, done.Inputs, done.States = append(done.WorkItemIDs, id), append(done.Inputs, row.Inputs[i]), append(done.States, row.States[i])
			}
			workDeleted += rowsAffected(res)
		}
		for _, id := range row.ScanResultIDs {
			// Skip a scan_results row still linked to an in-flight (processing)
			// work_queue row, so a worker never has its scan_result_id nulled
			// (migration 009) and junction cascaded (010) out from under it.
			// The path guard is defensive and symmetric with the work_queue
			// one: no production statement re-paths an existing scan_results
			// row, so it closes no observed race.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM scan_results WHERE id = ? AND file_path = ?
                 AND NOT EXISTS (
                     SELECT 1 FROM work_queue_scan_results j
                     JOIN work_queue wq ON wq.id = j.work_queue_id
                     WHERE j.scan_result_id = ? AND wq.status = 'processing')`,
				id, row.SourcePath, id)
			if err != nil {
				return 0, 0, 0, nil, fmt.Errorf("prune: delete scan_results %d: %w", id, err)
			}
			if rowsAffected(res) > 0 {
				done.ScanResultIDs = append(done.ScanResultIDs, id)
			}
			scanDeleted += rowsAffected(res)
		}
		if scanDeleted+workDeleted > before {
			applied = append(applied, done)
		}
		if scanDeleted+workDeleted-before < len(row.WorkItemIDs)+len(row.ScanResultIDs) {
			skipped++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, nil, fmt.Errorf("prune: commit tx: %w", err)
	}
	return scanDeleted, workDeleted, skipped, applied, nil
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

// pathExists reports whether p exists. Only a definitive not-exist result counts
// as gone; any other stat error (permissions, I/O) is treated as "exists" so a
// transient error never triggers a destructive prune.
func pathExists(stat func(string) (fs.FileInfo, error), p string) bool {
	if _, err := stat(p); err != nil {
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
