package commands

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/realign"
	"github.com/sydlexius/canticle/internal/revalidate"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/timing"
)

// Fallback bounds for a Config built in code rather than loaded, where the
// config-layer defaults never ran. They mirror config's own defaults; the config
// package owns the user-facing values.
const (
	defaultTimingSweepBatch = 100
	// defaultTimingSweepInterval is the cadence used when the scan interval is
	// zero (scan-once mode). The sweep still has a backlog to drain there, so it
	// needs a cadence of its own rather than inheriting "never".
	defaultTimingSweepInterval = 6 * time.Hour
)

// runTimingValidationSweep re-judges .lrc files already on disk against their
// companion audio's exact duration, a bounded batch per cycle, until ctx is
// canceled (#443, closing #437).
//
// WHY IT EXISTS. The accept-time guard (#439) only ever sees a NEW fetch, so
// every sidecar written before it landed is unjudged, and the only way to reach
// them is the `revalidate` CLI -- which an unattended install never runs. That is
// the same shape as #708's instrumental backlog: a capability that exists, is
// correct, and drifts forever because nobody invokes it.
//
// WHY IT IS SAFE TO RUN UNATTENDED. It shares the CLI's remediation core rather
// than reimplementing one, so the predicate (internal/timing), the disk-to-Song
// seam (internal/lyrics), and the mutation path (realign.Apply, backup-first and
// clobber-safe) are all the same code an operator can preview with
// `canticle revalidate`. An unknown duration fails open and never remediates.
// Both config flags must be on: `enabled` says the feature is live,
// `revalidate_existing` says an unattended pass may touch pre-existing files --
// the same two-key gate realign uses.
//
// WHY IT CONVERGES INSTEAD OF RE-THRASHING THE ARRAY. It never walks a library.
// The candidate list is queue.ListTimingBacklog -- completed synced rows with no
// timing verdict -- and a judged row leaves that population by being stamped. So
// the backlog drains a batch per cycle and then the query returns nothing,
// forever, and a cycle costs one SQLite read. That matters because the array
// disks are meant to stay asleep (#684/#685): a cycle's cost is proportional to
// the BATCH, never to the size of the library.
func runTimingValidationSweep(ctx context.Context, sqlDB *sql.DB, cfg config.Config, interval time.Duration) {
	job, ok := newTimingSweepJob(ctx, sqlDB, cfg)
	if !ok {
		return
	}
	runTimingSweepLoop(ctx, job, resolveTimingSweepInterval(interval))
}

// timingSweeper is the one-call seam the loop needs, so the loop is testable
// without a database or real audio.
type timingSweeper interface {
	runCycle(ctx context.Context) (timingSweepResult, error)
}

// timingSweepResult is one cycle's outcome, for logging only.
type timingSweepResult struct {
	Counts    revalidate.Counts
	Stamped   int
	Remedied  int
	Failed    int
	Remaining int
}

// timingSweepJob holds one cycle's already-open dependencies. Built once at
// startup, so a cycle opens no database, resolves no config, and re-reads no
// library list.
type timingSweepJob struct {
	q    *queue.DBQueue
	libs *library.Repo
	rev  *revalidate.Revalidator
	// opts is the resolved option set the Revalidator was built from, kept so a
	// cycle can re-run Validate against the CURRENT library roots.
	opts  revalidate.Options
	ra    *realign.Realigner
	batch int
	// backupPath is the JSONL trail every applied action is recorded to before
	// the file is touched. One appended file rather than the CLI's per-run
	// timestamped one: this pass runs forever, so a file per cycle would litter
	// the config directory with thousands of mostly-empty records.
	backupPath string
}

// newTimingSweepJob validates the config and builds the cycle's dependencies,
// reporting whether the sweep should run at all. Split from the loop so every
// guard is reachable without starting a goroutine that blocks on a ticker.
func newTimingSweepJob(ctx context.Context, sqlDB *sql.DB, cfg config.Config) (*timingSweepJob, bool) {
	tv := cfg.TimingValidation
	// BOTH flags, mirroring the realign gate. Refusing to start (rather than
	// starting and finding nothing to do) keeps a disabled deployment from
	// spinning a goroutine that wakes on a ticker forever to do nothing.
	if !tv.Enabled || !tv.RevalidateExisting {
		slog.Debug("timing validation sweep disabled by config",
			"enabled", tv.Enabled, "revalidate_existing", tv.RevalidateExisting)
		return nil, false
	}
	batch := tv.RevalidateBatch
	if batch < 1 {
		// Defensive only: config.Load already re-defaults a non-positive batch.
		// This catches a Config built in code that never ran the loader, where a
		// zero would drain nothing forever while still waking on every tick.
		batch = defaultTimingSweepBatch
	}
	libs := library.New(sqlDB)
	opts := revalidate.Options{
		// ROOTS ARE POPULATED EVEN THOUGH THE SWEEP NEVER WALKS THEM, because
		// Validate reads them for the "quarantine dir is not inside a scanned
		// root" check -- and that check is MORE load-bearing here than for the
		// CLI, not less. QuarantineDir defaults to <db-dir>/quarantine, so an
		// install whose database sits under a library root would have this
		// unattended pass move rejected sidecars INTO the music library, where
		// the watcher then sees them as new files. Leaving Roots empty makes the
		// check vacuously pass and hides exactly that.
		Roots: timingSweepRoots(ctx, libs),
		// The ACTIONS ARE SET EXPLICITLY, never derived from the legacy
		// OnFail/Purge flags. That is what keeps `on_categorical = "purge"` and
		// the CLI's --purge on one vocabulary: serve mode names the action it
		// wants, and revalidate.New leaves an explicitly-set action alone.
		MisSyncedAction:   revalidate.Action(tv.OnMisSynced),
		CategoricalAction: revalidate.Action(tv.OnCategorical),
		QuarantineDir:     timingSweepQuarantineDir(cfg),
	}
	if verr := opts.Validate(); verr != nil {
		// Non-fatal for serve mode: the sweep is a background convenience, and
		// the rest of the daemon is unaffected. But it is an ERROR, not a debug
		// line -- the operator turned this on and it is not going to run.
		slog.Error("timing validation sweep: invalid configuration; sweep disabled for this run", "error", verr)
		return nil, false
	}
	return &timingSweepJob{
		q:          queue.NewDBQueue(sqlDB),
		libs:       libs,
		rev:        revalidate.New(bankingDurationLookup(audiodur.New(sqlDB, scanner.DurationReaderVersion)), opts),
		opts:       opts,
		ra:         realign.New(libs, cfg.Realign),
		batch:      batch,
		backupPath: timingSweepBackupPath(cfg),
	}, true
}

// bankingDurationLookup wraps the duration cache so a MISS RESOLVES ITSELF
// instead of being recorded as a verdict.
//
// WITHOUT THIS THE WHOLE FEATURE IS INERT ON A REAL INSTALL, which is not a
// tuning matter but the difference between working and silently doing nothing.
// audiodur.Lookup is a pure SQL read with no fill path -- Record is the only
// writer, and its callers are the scanner and the worker. Neither reaches the
// population this sweep judges: a file that already carries a sidecar is
// skipped ~200 lines before the scanner's enrichment probe, so by construction
// it was never duration-probed by an older build (#684, measured in prod -- a
// full scan banked 690 durations and moved the unknown-duration count by zero).
// That starved population IS the backlog ListTimingBacklog selects. So a raw
// Lookup misses on essentially every candidate, every candidate fails open, and
// the sweep stamps the entire backlog unknown_duration while remediating
// nothing -- reporting "converged" having judged nothing, with the stamp
// retiring each row permanently.
//
// THE READ IS GATED ON THE MISS, which is what keeps it affordable and is the
// same bargain #684 struck: a given file VERSION is probed ONCE, ever, because
// the banked row satisfies every later Lookup. Ungated this would re-read
// headers for the whole library on every cycle and hold the array awake, the
// exact symptom #684 exists to remove. Bounded further by revalidate_batch, so
// one cycle probes at most that many files.
//
// THAT PROBE IS NOT UNIFORMLY A HEADER READ, and the worst case is worth
// stating plainly: a FLAC costs 42 bytes of STREAMINFO, an ordinary MP3 64-112
// KB, but a VBR MP3 with no Xing header is frame-counted END TO END and reads
// slightly more than its own size (~7% of MP3s, on the measurement recorded in
// scanner.bankDurationForSkippedFile). That is precisely why the gate above is
// load-bearing rather than an optimization: paying it once per file version is
// fine, once per cycle would not be.
//
// EVERY FAILURE DEGRADES TO THE MISS IT ALREADY WAS. A file that cannot be
// opened, has no parser for its extension, or fails to parse returns
// found=false, flows to timing.Evaluate as UnknownDuration, and fails open --
// never remediated. Banking only ever turns an unknown into a known; it can
// never turn a known into a wrong verdict.
func bankingDurationLookup(store *audiodur.Store) revalidate.DurationLookup {
	return func(ctx context.Context, path string, mtimeNano, size int64) (int, bool, error) {
		seconds, found, err := store.Lookup(ctx, path, mtimeNano, size)
		if err != nil {
			// A store failure is TRANSIENT and propagates: the caller abandons
			// the cycle rather than stamping rows it never judged. Probing here
			// instead would convert a database problem into a library-wide
			// header read.
			return 0, false, err
		}
		if found {
			return seconds, true, nil
		}
		// The miss is real, so pay for it once. ReadAudioDuration takes the
		// (duration, mtime, size) tuple from ONE open handle, so the banked row
		// always describes a single inode: a tagger swapping the file mid-call
		// makes the row inert against the replacement (a later miss, the safe
		// direction) rather than a confidently wrong hit nothing invalidates.
		//
		// DURATION-ONLY, NOT ReadAudioFacts, and the difference is a whole
		// population. ReadAudioFacts reads TAGS first and treats their absence as
		// fatal, so a valid, perfectly parsable file carrying no tag block reads
		// as an error and yields no duration -- and untagged files are exactly
		// the ones most likely to carry a hand-made or scraped sidecar, i.e. the
		// backlog this sweep exists to judge. It also gates on the extension
		// before opening, so a row pointing at a non-audio path costs nothing.
		seconds, mtime, bytes, derr := scanner.ReadAudioDuration(path)
		if derr != nil || seconds <= 0 {
			// Genuinely unknown for this file version: unreadable, no parser for
			// the extension, or a parse failure. Report the miss and let the
			// caller fail open.
			slog.Debug("timing validation sweep: could not determine an exact duration; leaving the sidecar unjudged",
				"path", path, "error", derr)
			return 0, false, nil
		}
		if rerr := store.Record(ctx, path, mtime, bytes, seconds); rerr != nil {
			// Non-fatal: the duration is known NOW, so judge with it. Failing to
			// cache costs a re-probe next time, never a wrong verdict.
			slog.Debug("timing validation sweep: duration cache write failed; judging anyway",
				"path", path, "error", rerr)
		}
		return seconds, true, nil
	}
}

// timingSweepRoots lists the configured library roots, for Validate's
// quarantine-containment check only -- the sweep never walks them.
//
// A failure here returns no roots rather than refusing to start. The roots are
// an INPUT TO A SAFETY CHECK, not to the work: losing them weakens that one
// check, which is the same position the CLI is in when an operator passes an
// explicit root, whereas refusing to start would disable a working sweep over a
// transient database read.
func timingSweepRoots(ctx context.Context, libs *library.Repo) []string {
	all, err := libs.List(ctx)
	if err != nil {
		slog.Warn("timing validation sweep: could not list libraries; the quarantine-containment check will not run this cycle", "error", err)
		return nil
	}
	return libraryRoots(all)
}

// libraryRoots projects library rows onto the root list Validate consumes,
// dropping any whose path is blank.
//
// AN EMPTY ROOT DOES NOT FAIL THE CHECK, IT CORRUPTS IT. filepath.Abs("")
// returns the PROCESS WORKING DIRECTORY rather than an error, so a blank row
// would silently have Validate compare the quarantine dir against the CWD. Under
// the container image that is the likeliest arrangement to collide: with the
// working directory at /config and the database beside it, <db-dir>/quarantine
// sits under the CWD, so containment "fails" and the sweep skips -- and since
// the check now runs per cycle, it would skip EVERY cycle, for the life of the
// daemon, over a root that does not exist.
//
// Both writers (library.Add and library.Update) reject a blank path, so this is
// a guard rather than a live defect. It is worth having anyway: the cost is one
// comparison, and the failure it prevents is silent and permanent.
func libraryRoots(all []models.Library) []string {
	roots := make([]string, 0, len(all))
	for _, l := range all {
		if strings.TrimSpace(l.Path) == "" {
			continue
		}
		roots = append(roots, l.Path)
	}
	return roots
}

// timingSweepQuarantineDir is where a removed .lrc is moved to. It mirrors the
// CLI's default (<db-dir>/quarantine) deliberately: an operator who ran
// `canticle revalidate` by hand and then enabled the sweep finds both passes'
// output in one place rather than hunting a second directory.
func timingSweepQuarantineDir(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.DB.Path), "quarantine")
}

// timingSweepBackupPath mirrors realignServeBackupPath: one appended JSONL trail
// for the daemon's lifetime, next to the database.
func timingSweepBackupPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.DB.Path), "revalidate-serve-backup.jsonl")
}

// resolveTimingSweepInterval bounds the cadence. The sweep reuses the scan
// interval (#443 defines no interval knob of its own, and adding one would be an
// unrequested tunable), but that value can be zero -- scan-once mode -- and a
// zero would panic time.NewTicker. A scan-once deployment still has a backlog
// worth draining, so it falls back to a cadence rather than to "never".
func resolveTimingSweepInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return defaultTimingSweepInterval
	}
	return interval
}

// runCycle judges one bounded batch: read the backlog, resolve each row's
// library, plan, apply, stamp.
func (j *timingSweepJob) runCycle(ctx context.Context) (timingSweepResult, error) {
	var res timingSweepResult
	items, err := j.q.ListTimingBacklog(ctx, queue.TimingBacklogOptions{Limit: j.batch})
	if err != nil {
		return res, err
	}
	if len(items) == 0 {
		return res, nil
	}
	candidates, roots, err := j.candidatesFor(ctx, items)
	if err != nil {
		return res, err
	}
	// RE-CHECK CONTAINMENT AGAINST THE LIVE LIBRARY SET, not the startup
	// snapshot. The roots are resolved once at construction, but libraries are
	// mutable at runtime: an operator who adds a root that CONTAINS
	// <db-dir>/quarantine while serve is running would otherwise keep passing a
	// check made against a stale list, and the sweep would move rejected
	// sidecars INTO the music library -- where the watcher sees them as new
	// files and a later cycle re-judges and re-quarantines its own output,
	// deeper each time, until the daemon restarts.
	//
	// A failure SKIPS THE CYCLE rather than remediating anyway: nothing is
	// stamped, so every row stays in the backlog and the next cycle re-judges
	// it once the configuration is sane. Judging costs a re-read; moving a file
	// into the wrong place does not undo.
	live := j.opts
	live.Roots = roots
	if verr := live.Validate(); verr != nil {
		slog.Error("timing validation sweep: the library set changed and the configuration is no longer safe; skipping this cycle", "error", verr)
		return res, nil
	}
	plan, err := j.rev.PlanCandidates(ctx, candidates)
	if err != nil {
		return res, err
	}
	res.Counts = plan.Counts

	// APPLY BEFORE STAMPING, and the order is load-bearing. A stamp says "this
	// row has been judged and acted on"; writing it first would retire the row
	// from the backlog whether or not the file was actually moved, so a failed
	// remediation would be invisible and never retried. Applying first means a
	// failure is still reflected in what gets stamped below.
	failedPaths := j.apply(plan.Moves, &res)

	for _, f := range plan.Findings {
		if f.ID == 0 {
			continue
		}
		if !timingOutcomeIsTerminal(f) {
			// Transient: the attempt failed, not the file. Re-judged next cycle.
			continue
		}
		if _, bad := failedPaths[f.Path]; bad {
			// Leave the row UNSTAMPED so the next cycle tries again. A move can
			// fail transiently (a locked file, a full disk), and that is exactly
			// the case worth retrying -- unlike the verdicts below, which are
			// settled facts about the file.
			continue
		}
		if serr := j.q.SetTimingOutcome(ctx, f.ID, timingRecordFor(f)); serr != nil {
			// Non-fatal per row: the file is already remediated, and an unstamped
			// row is merely re-judged next cycle, which is idempotent.
			slog.Warn("timing validation sweep: could not stamp a judged row", "id", f.ID, "error", serr)
			continue
		}
		res.Stamped++
	}

	remaining, cerr := j.q.CountTimingBacklog(ctx)
	if cerr != nil {
		// Reporting-only, so a failure here must not fail a cycle that did its
		// work. -1 marks it unknown rather than lying with a 0.
		res.Remaining = -1
	} else {
		res.Remaining = remaining
	}
	return res, nil
}

// candidatesFor turns backlog rows into revalidate candidates, resolving each
// row's library root.
//
// THE ROOT IS RESOLVED PER ROW because one batch spans every library: the
// backlog query is ordered oldest-first across the whole queue, not scoped to
// one root. The root anchors the quarantine layout, so getting it wrong would
// flatten two libraries' identically-named sidecars into one directory.
func (j *timingSweepJob) candidatesFor(ctx context.Context, items []queue.WorkItem) ([]revalidate.Candidate, []string, error) {
	libs, err := j.libs.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	roots := libraryRoots(libs)
	candidates := make([]revalidate.Candidate, 0, len(items))
	for _, it := range items {
		c := revalidate.Candidate{ID: it.ID, AudioPath: it.Inputs.SourcePath}
		// Longest matching root wins, so a nested library root (a genre subtree
		// added as its own library) claims its own files rather than losing them
		// to the parent it happens to sit under.
		for _, l := range libs {
			if !strings.HasPrefix(c.AudioPath, strings.TrimSuffix(l.Path, string(filepath.Separator))+string(filepath.Separator)) {
				continue
			}
			if len(l.Path) > len(c.Root) {
				c.Root, c.LibraryID = l.Path, l.ID
			}
		}
		// A row under no configured root keeps an empty Root, which
		// quarantineTarget degrades to a flattened, containment-checked copy of
		// the FULL path for -- inside the quarantine directory, and distinct per
		// source, so two rootless sidecars cannot collide (a base-name fallback
		// did collide, and a refused move is retried forever). Judging it is
		// right: the file exists and the verdict is about its content, not its
		// location.
		candidates = append(candidates, c)
	}
	return candidates, roots, nil
}

// apply runs the planned moves through realign's one apply path and returns the
// paths whose action FAILED, so the caller can leave those rows unstamped.
func (j *timingSweepJob) apply(moves []realign.Move, res *timingSweepResult) map[string]struct{} {
	failed := map[string]struct{}{}
	if len(moves) == 0 {
		return failed
	}
	applied, aerr := j.ra.Apply(moves, j.backupPath, realign.Policy{AllowHeuristic: true})
	if aerr != nil {
		// Apply returns an error only for a backup-file failure, which is
		// backup-FIRST: nothing was touched. Treat every move as failed so no row
		// is stamped for work that did not happen.
		slog.Error("timing validation sweep: could not write the backup trail; no file was touched", "error", aerr)
		for _, mv := range moves {
			failed[mv.Orphan] = struct{}{}
		}
		return failed
	}
	for _, a := range applied {
		if a.Err != nil {
			res.Failed++
			failed[a.Move.Orphan] = struct{}{}
			// Per-file detail goes to the structured log, never to stdout: a
			// sidecar path carries the library's private artist/album/title.
			slog.Warn("timing validation sweep: action failed; leaving the file in place",
				"path", a.Move.Orphan, "kind", a.Move.Kind, "error", a.Err)
			continue
		}
		if a.GatedSkipped {
			// Nothing was done, so the row must not be stamped as if it had been.
			failed[a.Move.Orphan] = struct{}{}
			continue
		}
		res.Remedied++
	}
	return failed
}

// timingOutcomeIsTerminal reports whether a finding settles a row for good.
//
// EVERY OTHER VERDICT HERE IS A FACT ABOUT THE FILE; "errored" IS A FACT ABOUT
// THE ATTEMPT, and conflating the two retires a sidecar that nothing is wrong
// with. judge counts Errored when the .lrc cannot be read or parsed -- which is
// usually permanent (a corrupt file) but is equally what a temporarily
// unavailable mount, a transient I/O error, or a file being rewritten right then
// looks like. Since the stamp is one-way and removes the row from
// ListTimingBacklog forever, stamping that case means a single bad moment
// permanently exempts a sidecar from an unattended pass, recoverable only by
// running the CLI by hand.
//
// Left unstamped it is simply re-judged next cycle, which is idempotent and
// cheap. That does mean a genuinely corrupt .lrc is retried on every cycle
// rather than retired -- the deliberate trade, because a batch slot spent
// re-reading one unparsable file is recoverable, and a wrongly-retired sidecar
// is not. The row does not head-of-line the batch: it is one slot, not a
// growing set, and an operator sees it in the errored count.
func timingOutcomeIsTerminal(f revalidate.Finding) bool {
	return f.Outcome != "errored"
}

// timingRecordFor maps a finding onto the row stamp.
//
// EVERY finding gets a record, including the ones that reached no verdict, and
// that is what makes the sweep converge: the stamp is what retires a row from
// the backlog query, so a row left unstamped returns at the head of the
// oldest-first batch every cycle forever. An un-judgeable row is stamped
// unknown_duration -- honest (no comparison happened) and terminal.
func timingRecordFor(f revalidate.Finding) queue.TimingRecord {
	rec := queue.TimingRecord{Outcome: string(f.Outcome), EvaluatedAt: time.Now().UTC()}
	switch f.Outcome {
	case timing.Ok, timing.MisSynced, timing.Categorical, timing.Degenerate:
		// A real comparison against a known duration happened, so the magnitude
		// columns mean something.
		rec.Magnitude = f.Overrun
		rec.Ratio = f.Ratio
		rec.Measured = true
	default:
		// unknown_duration, and the candidate-only no_audio/no_sidecar/errored
		// verdicts. Those three are not internal/timing values, so they are
		// NORMALIZED to unknown_duration rather than written verbatim: the column
		// is a closed vocabulary that /metrics groups by (#629), and widening it
		// from here would invent label values nothing else in the system emits.
		rec.Outcome = string(timing.UnknownDuration)
		rec.Measured = false
	}
	return rec
}

// runTimingSweepCycle runs one cycle and logs it. A failure is logged and
// swallowed: the sweep is a background pass, so a transient error waits for the
// next interval rather than killing the goroutine.
func runTimingSweepCycle(ctx context.Context, j timingSweeper) {
	res, err := j.runCycle(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("timing validation sweep failed; will retry next interval", "error", err)
		}
		return
	}
	// Silent on a converged install: this runs forever, and a line per cycle
	// saying "nothing to do" is noise an operator learns to filter out. But
	// Failed is checked SEPARATELY from the verdict counts -- a cycle where every
	// action failed still judged files, and keying the log on verdicts alone
	// would let a permanently-failing sweep look like a working one.
	if res.Failed > 0 {
		slog.Warn("timing validation sweep finished with failed actions",
			"failed", res.Failed, "remedied", res.Remedied, "stamped", res.Stamped,
			"mis_synced", res.Counts.MisSynced, "categorical", res.Counts.Categorical,
			"degenerate", res.Counts.Degenerate, "remaining", res.Remaining)
		return
	}
	if res.Counts.Scanned == 0 && res.Stamped == 0 {
		return
	}
	slog.Info("timing validation sweep judged existing sidecars",
		"scanned", res.Counts.Scanned, "ok", res.Counts.Ok,
		"mis_synced", res.Counts.MisSynced, "categorical", res.Counts.Categorical,
		"degenerate", res.Counts.Degenerate, "unknown_duration", res.Counts.UnknownDuration,
		"no_sidecar", res.Counts.NoSidecar, "no_audio", res.Counts.NoAudio,
		"remedied", res.Remedied, "stamped", res.Stamped, "remaining", res.Remaining)
}

// runTimingSweepLoop runs a cycle at startup and then once per interval until
// ctx is canceled.
func runTimingSweepLoop(ctx context.Context, j timingSweeper, interval time.Duration) {
	slog.Info("timing validation sweeper started", "interval", interval)
	// Judge at startup so a fresh backlog begins draining immediately rather than
	// waiting out the first interval.
	runTimingSweepCycle(ctx, j)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runTimingSweepCycle(ctx, j)
		}
	}
}
