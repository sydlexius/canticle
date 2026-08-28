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

// Action is what happens to a sidecar the timing predicate rejected. It is the
// SINGLE internal representation of that decision: the CLI's --on-fail/--purge
// flags translate into a pair of Actions in New, and serve mode's
// [timing_validation] config sets them directly. One vocabulary, so the two
// entry points cannot drift into disagreeing about what a given setting does to
// a user's files.
type Action string

const (
	// ActionDemote writes the lyric's plain words as a .txt beside the audio,
	// then moves the .lrc aside. Only meaningful for MisSynced/Degenerate, whose
	// words are content-correct.
	ActionDemote Action = "demote"
	// ActionQuarantine moves the sidecar aside without keeping its words.
	// Recoverable: the file is moved, never unlinked.
	ActionQuarantine Action = "quarantine"
	// ActionPurge unlinks the sidecar. IRREVERSIBLE; the JSONL backup record is
	// the only trail left, which is why it is never a default.
	ActionPurge Action = "purge"
	// ActionOff plans nothing for this verdict. The file is still classified and
	// counted -- that is the point: an operator can see what a pass WOULD do
	// before letting it act.
	ActionOff Action = "off"
)

// misSyncedActions and categoricalActions are the accepted sets per arm. They
// differ by exactly one value and the difference is load-bearing: a Categorical
// lyric is a different song's words, so there is nothing worth demoting to .txt
// and offering the option would be an accepted value with no honest meaning.
func misSyncedActions() []Action {
	return []Action{ActionDemote, ActionQuarantine, ActionPurge, ActionOff}
}

func categoricalActions() []Action {
	return []Action{ActionQuarantine, ActionPurge, ActionOff}
}

func validAction(a Action, allowed []Action) bool {
	for _, x := range allowed {
		if a == x {
			return true
		}
	}
	return false
}

// Options configures a revalidation pass.
type Options struct {
	// Roots are the directory trees to walk. The serve sweep leaves this empty:
	// it judges an explicit candidate list drawn from the timing watermark
	// rather than walking, so it never re-reads the library each cycle.
	Roots []string
	// MisSyncedAction is what happens to a MisSynced (or Degenerate) sidecar.
	// Empty means "derive from the legacy OnFail/Purge flags" -- see New.
	MisSyncedAction Action
	// CategoricalAction is what happens to a Categorical sidecar. Empty means
	// "derive from the legacy Purge flag" -- see New.
	CategoricalAction Action
	// OnFail decides the MisSynced action. Empty means Demote.
	//
	// LEGACY, retained for the CLI's --on-fail flag. It and Purge are translated
	// into the Action pair above by New; nothing downstream reads them.
	OnFail OnFail
	// Purge hard-deletes instead of quarantining. Opt-in and irreversible.
	//
	// LEGACY, retained for the CLI's --purge flag. See OnFail.
	Purge bool
	// QuarantineDir is the root that removed .lrc files are moved under,
	// preserving their path relative to their library root so two same-named
	// sidecars cannot collide.
	//
	// Required whenever the RESOLVED actions can quarantine -- see quarantines().
	// That is not the same as "unless Purge is set": an explicit ActionDemote
	// always moves the .lrc aside and therefore always needs a destination, even
	// with Purge true. Only a demote DERIVED from the legacy flags takes their
	// purge semantics.
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
	Degenerate      int // every cue at one timestamp: not synced at all (#673)
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
	// demoteRemoval is what a demote does with the .lrc after the words are
	// written: quarantine (the default, and always so for an explicit demote) or
	// purge (a demote derived from the legacy --purge flag). Resolved once in
	// New; never re-derived from opts.Purge at plan time.
	demoteRemoval Action
}

// New builds a Revalidator. lookup may be nil, in which case every file reads as
// unknown-duration and the pass reports without ever proposing a mutation -- the
// safe degradation, not an error.
func New(lookup DurationLookup, opts Options) *Revalidator {
	if opts.OnFail == "" {
		opts.OnFail = Demote
	}
	// Translate the legacy CLI flags into the per-arm actions, unless the caller
	// set them explicitly (serve mode does). Doing it ONCE, through the same
	// helper Validate uses, is what keeps the two entry points on one
	// vocabulary: everything downstream reads only the actions, so there is no
	// second place where --purge could come to mean something subtly different
	// from on_categorical = "purge".
	//
	// --on-fail delete resolves to ActionQuarantine, not ActionPurge: it means
	// "do not keep the words", and without --purge the file was always MOVED
	// rather than unlinked. Purge alone decided delete-vs-quarantine before this
	// refactor, and that is preserved exactly.
	var demoteRemoval Action
	opts.MisSyncedAction, opts.CategoricalAction, demoteRemoval = opts.resolvedActions()
	return &Revalidator{lookup: lookup, opts: opts, demoteRemoval: demoteRemoval}
}

// Plan walks every configured root, classifies each .lrc, and returns the
// aggregate counts plus the remediation moves that Apply would perform. It
// MUTATES NOTHING: the only filesystem calls are directory reads, stats, and
// reads of the .lrc files themselves.
func (r *Revalidator) Plan(ctx context.Context) (Plan, error) {
	var plan Plan
	// One cache for the whole Plan call, not one per root and not one per
	// directory read separately -- a directory read once here is never read
	// again anywhere in this run, however many orphaned or unusually-cased
	// sidecars it holds. See dirListingCache's doc comment.
	cache := newDirListingCache()
	for _, root := range r.opts.Roots {
		if err := r.walkRoot(ctx, root, &plan, cache); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// walkRoot walks one root, honoring ctx cancellation between entries.
func (r *Revalidator) walkRoot(ctx context.Context, root string, plan *Plan, cache *dirListingCache) error {
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
		r.classify(ctx, root, path, plan, cache)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("revalidate: walk %q: %w", root, err)
	}
	return nil
}

// classify judges one .lrc and appends its finding (and any move) to plan.
func (r *Revalidator) classify(ctx context.Context, root, path string, plan *Plan, cache *dirListingCache) {
	audio, ok := companionAudio(path, cache)
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
		mv, mok := r.misSyncedMove(root, path, audio)
		if mok {
			f.Action = mv.Kind
			plan.Moves = append(plan.Moves, mv)
		}
	case timing.Degenerate:
		// Every cue shares one timestamp, so the file is not synced (#673).
		// Demote exactly as MisSynced does -- same move, same backup trail --
		// because the defect is identical in kind: the words are fine, the
		// timing is not. Reusing demotionMove is what keeps this arm honest;
		// a bespoke path here would drift from the settled-.txt and
		// all-decorative rules that path already enforces.
		//
		// Note the overrun predicate CANNOT reach these files: an all-zero
		// lyric has max 0, so it never overruns and classifies Ok. Before this
		// arm the verdict fell to default and was counted Errored, which never
		// remediates -- so the existing corpus would have stayed untouched.
		plan.Counts.Degenerate++
		mv, mok := r.misSyncedMove(root, path, audio)
		if mok {
			f.Action = mv.Kind
			plan.Moves = append(plan.Moves, mv)
		}
	case timing.Categorical:
		plan.Counts.Categorical++
		if r.opts.CategoricalAction != ActionOff {
			mv := r.removalMove(root, path, r.opts.CategoricalAction)
			f.Action = mv.Kind
			plan.Moves = append(plan.Moves, mv)
		}
	default:
		// An outcome this package does not recognize must never remediate.
		plan.Counts.Errored++
	}
	plan.Findings = append(plan.Findings, f)
}

// misSyncedMove builds the move for a MisSynced (or Degenerate) .lrc according
// to MisSyncedAction, returning ok=false when nothing is to be done.
//
// Under ActionDemote it reads the cues back and flattens them to the plain
// words via the shared lyrics.PlainBody, so the demoted .txt matches exactly
// what the accept-time guard would have written. A lyric that flattens to
// nothing (all decorative) has no words worth keeping, so it falls through to
// plain removal rather than writing an empty file.
func (r *Revalidator) misSyncedMove(root, path, audio string) (realign.Move, bool) {
	switch r.opts.MisSyncedAction {
	case ActionOff:
		// Classified and counted by the caller; nothing planned. The file is left
		// exactly as it is.
		return realign.Move{}, false
	case ActionQuarantine, ActionPurge:
		return r.removalMove(root, path, r.opts.MisSyncedAction), true
	}
	synced, err := lyrics.ReadSyncedLRC(path)
	if err != nil {
		return realign.Move{}, false
	}
	body := lyrics.PlainBody(synced)
	if body == "" {
		// Nothing worth keeping, so this degenerates to a plain removal -- which
		// honors the legacy --purge by resolving to ActionPurge.
		return r.removalMove(root, path, r.demoteRemoval), true
	}
	// DEMOTE IS TWO HALVES: write the words as .txt, then get rid of the .lrc.
	// Which flavor that second half takes is what --purge decides, and it must be
	// carried on the Kind -- NOT left as a demote with an empty Target. That was
	// the bug: realign's demote arm writes the .txt and then calls moveAside,
	// which refuses an empty target, so the .txt was rolled back and the .lrc
	// left in place. `revalidate --purge --apply` therefore remediated NOTHING on
	// this arm and reported a failure per file.
	mv := r.removalMove(root, path, r.demoteRemoval)
	if mv.Kind == realign.KindPurge {
		// Write the words, then unlink rather than move aside. realign's demote
		// arm is move-based by construction, so the two steps are expressed as a
		// purge that carries a TextPath.
		mv.Method = "revalidate-demote-purge"
	} else {
		mv.Kind = realign.KindDemote
		mv.Method = "revalidate-demote"
	}
	mv.TextPath = strings.TrimSuffix(audio, filepath.Ext(audio)) + ".txt"
	mv.TextBody = body
	return mv, true
}

// removalMove builds the quarantine (default) or purge (opt-in) action for a
// .lrc. Quarantine preserves the file's path relative to root under
// QuarantineDir, so two identically-named sidecars from different albums cannot
// collide there.
func (r *Revalidator) removalMove(root, path string, action Action) realign.Move {
	mv := realign.Move{
		Orphan:    path,
		Method:    "revalidate",
		LibraryID: r.opts.LibraryID,
		Eligible:  true,
		Kind:      realign.KindQuarantine,
	}
	// Keyed on the RESOLVED action, never on opts.Purge. Reading the legacy flag
	// here was the one place that had not been converted, and it is the place
	// that decides move-vs-unlink: a caller who explicitly asked to quarantine
	// got an irreversible delete whenever Purge happened also to be set. New
	// already folds Purge into ActionPurge, so the flag has nothing left to say.
	if action == ActionPurge {
		mv.Kind = realign.KindPurge
		// A purge unlinks in place, so a quarantine destination would be a
		// misleading restore path on the backup record.
		mv.Target = ""
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
//
// #691: this used to os.ReadDir the sidecar's directory on every call -- one
// full directory listing per sidecar, so a directory holding N sidecars cost
// O(N) directory reads of the SAME directory. The companion must share the
// sidecar's exact stem, so instead this probes the bounded candidate set
// <stem>.<ext> for every extension scanner.SupportedAudioExtensions() names --
// os.Stat calls independent of directory size, never a directory listing.
//
// WHY THE PROBE AND NOT #691's OTHER OPTION. The alternative was one ReadDir
// per directory, reused across the sidecars in it; implemented WELL (the
// listing indexed once, so each later sidecar is an O(1) lookup) that option
// is CPU-COMPARABLE to the probe, not worse -- see companion_bench_test.go's
// header for the full measurement (including the naive strawman an earlier
// draft of this comment measured against instead, and overclaimed from).
// Neither implementation dominates; the CPU numbers are a tiebreak that came
// back level.
//
// The probe is chosen on I/O SHAPE, which is the axis #691 actually names: it
// issues NO directory read at all, where the indexed option still pays one full
// ReadDir per directory. On the spun-down array disk that motivated #684/#685
// that is the difference that matters -- a bounded set of stats against known
// names denies the disk less idle time than listing a large directory, and the
// gap widens exactly where libraries are flattest. That is the reason, not the
// CPU tiebreak.
//
// Extension gating is inherent, not a separate check: a candidate is only ever
// built from scanner's own audio extension list, so it classifies identically
// to IsAudioFile (both read the same backing slice) and cannot drift from it. A
// candidate that stats as a directory is rejected, matching the old ReadDir
// loop's e.IsDir() skip.
//
// Case handling, and why the ReadDir fallback below is NOT dead code. The old
// loop matched an extension via scanner.IsAudioFile, which lowercases before
// comparing -- so it recognized a companion with ANY casing on disk (song.mp3,
// song.MP3, song.Mp3, ...). A stat-only probe cannot enumerate arbitrary casing
// without listing the directory, which is the exact cost being removed here, so
// the probe covers the lower- and upper-case forms (the two real libraries
// actually produce: a normal rip and a legacy all-caps Windows rip) and the
// listing is kept as a FALLBACK for everything else.
//
// The fallback runs ONLY when the probe finds nothing, which preserves
// IsAudioFile as the gate on what counts as audio -- an invariant #691 names
// explicitly. Without it a mixed-case companion (song.Mp3) would silently stop
// being found on a CASE-SENSITIVE filesystem, and "not found" here means the
// sidecar is never remediated: a MisSynced .lrc would be skipped rather than
// demoted, silently, which is the opposite of what this pass exists to do. That
// is invisible on macOS/APFS (case-insensitive, so os.Stat finds it anyway) and
// live on the Linux deployment #691 targets.
//
// It costs nothing in the common case: a sidecar WITH a companion -- the case
// the O(N) reads were being paid for -- returns from the probe and never
// reaches the listing. Only an orphan pays a directory read, exactly what it
// paid before this change, so this is a strict improvement over pre-#691 at
// every input rather than a trade.
//
// SELECTION ORDER (CodeRabbit finding on #801). os.ReadDir sorts its entries
// by filename, so the pre-#691 loop -- which returned the first stem match it
// walked -- always resolved a directory holding MORE THAN ONE stem-matching
// companion (song.mp3 AND song.flac beside the same song.lrc, a real if
// unusual library shape) to the lexicographically-FIRST filename. A probe that
// returns on its first hit instead resolves in scanner.SupportedAudioExtensions
// order (.mp3 before .flac), which is a silent behavior change: which
// companion wins is not cosmetic, its duration is what timing.Evaluate judges
// the sidecar against, so a changed winner can move or delete a CORRECT .lrc
// under the wrong verdict. Measured: a directory holding song.flac and
// song.mp3 returned song.mp3 under the first-hit probe and song.flac under
// the old listing.
//
// The fix below still issues no directory read on a hit -- the O(1)-ish win
// #691 exists for -- but no longer stops at the first match. It collects every
// candidate that stats as a real file across every extension and BOTH case
// forms, then picks the one whose FILENAME (not full path; all candidates
// necessarily share lrcPath's directory) sorts first, exactly reproducing
// os.ReadDir's own sort key. Go's string comparison is byte-wise, matching
// ReadDir/fs.ReadDir's sort.Slice on Name(), so this reproduces the pre-#691
// winner exactly, including the lower/upper interaction: on a case-sensitive
// filesystem "MP3" (ASCII 'M'=0x4D) sorts before "mp3" ('m'=0x6D), so
// song.MP3 would have beaten song.mp3 under the old listing too -- verified
// directly with os.ReadDir on a case-sensitive mount, not assumed.
//
// Both the lower- and upper-case spelling of every extension are stat'd, and
// EVERY one that resolves to a real file is a candidate for the compare --
// not just the first hit per extension -- because on a case-sensitive
// filesystem song.mp3 and song.MP3 can be two DIFFERENT files, and the old
// ReadDir loop would have compared both of their names too. But on a
// case-INSENSITIVE filesystem (the macOS dev box; never the case-sensitive
// Linux target #691 is written for) os.Stat resolves BOTH "song.flac" and
// "song.FLAC" successfully against the SAME on-disk song.flac -- an alias,
// not a second companion -- so naively keeping both would spuriously compare
// the uppercase spelling as if it were a distinct, real file (caught by
// TestCompanionAudioExactStemOnly reddening on macOS during development). Each
// pair's two os.Stat results are compared with os.SameFile before being kept
// as (up to) two candidates, so a case-insensitive alias collapses to one
// candidate under its actually-probed name and a genuine case-sensitive
// pair is kept as two.
func companionAudio(lrcPath string, cache *dirListingCache) (string, bool) {
	stem := strings.TrimSuffix(lrcPath, filepath.Ext(lrcPath))
	best := ""
	found := false
	consider := func(candidate string, fi os.FileInfo) {
		if fi.IsDir() {
			return
		}
		if !found || filepath.Base(candidate) < filepath.Base(best) {
			best = candidate
			found = true
		}
	}
	for _, ext := range scanner.SupportedAudioExtensions() {
		lower := stem + ext
		upper := stem + strings.ToUpper(ext)
		lowerFI, lowerErr := os.Stat(lower)
		upperFI, upperErr := os.Stat(upper)
		if lowerErr == nil {
			consider(lower, lowerFI)
		}
		if upperErr == nil && (lowerErr != nil || !os.SameFile(lowerFI, upperFI)) {
			consider(upper, upperFI)
		}
	}
	if found {
		return best, true
	}
	return companionAudioByListing(lrcPath, stem, cache)
}

// dirListingCache remembers each directory's os.ReadDir result for the
// lifetime of ONE Plan run (CodeRabbit finding on #801). Without it, a
// directory holding N orphaned sidecars -- or N companions with an unusual
// extension casing the probe's lower/upper pair does not cover -- reaches
// companionAudioByListing N times, each doing its own full ReadDir of the
// SAME directory: partly re-introducing the exact O(N)-per-directory cost
// #691 removed from the probe's hit path. This is not package-global state:
// it is created fresh by Plan for that one call and threaded down as a
// parameter, so two concurrent or sequential Plan runs never share a cache
// and test isolation is untouched. A failed ReadDir is deliberately NOT
// cached, so a transient error does not permanently blind the rest of the
// run to a directory that might read successfully on a later sidecar.
type dirListingCache struct {
	entries map[string][]os.DirEntry
	reads   int // count of REAL os.ReadDir calls made; test-observable via Reads()
}

func newDirListingCache() *dirListingCache {
	return &dirListingCache{entries: make(map[string][]os.DirEntry)}
}

// Reads reports how many real os.ReadDir calls this cache has made, for
// tests to assert the fallback reads a directory once per run rather than
// once per orphan.
func (c *dirListingCache) Reads() int {
	return c.reads
}

func (c *dirListingCache) list(dir string) ([]os.DirEntry, error) {
	if entries, ok := c.entries[dir]; ok {
		return entries, nil
	}
	entries, err := os.ReadDir(dir)
	c.reads++
	if err != nil {
		return nil, err
	}
	c.entries[dir] = entries
	return entries, nil
}

// companionAudioByListing is the pre-#691 lookup, kept as companionAudio's
// miss-path fallback so an unusually-cased extension still resolves through
// scanner.IsAudioFile. See companionAudio's comment for why this is reached
// only on a probe miss. The listing goes through cache so a directory
// holding several orphans (or unusually-cased companions) in one Plan run
// pays for one ReadDir, not one per sidecar that reaches this fallback.
func companionAudioByListing(lrcPath, stem string, cache *dirListingCache) (string, bool) {
	dir := filepath.Dir(lrcPath)
	entries, err := cache.list(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !scanner.IsAudioFile(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if strings.TrimSuffix(p, filepath.Ext(p)) == stem {
			return p, true
		}
	}
	return "", false
}

// resolvedActions returns the action pair New would derive from these Options,
// so Validate reasons about the SAME values the run will use. Kept in lockstep
// with New by construction: both call this, rather than each deriving the
// defaults separately and drifting.
func (o Options) resolvedActions() (misSynced, categorical, demoteRemoval Action) {
	// demoteRemoval is what the SECOND half of a demote does with the .lrc once
	// the words are safe: move it aside, or unlink it. It is resolved HERE, with
	// the other two, so planning and validation consume one resolved state
	// rather than each re-reading the legacy flag and reaching its own answer.
	//
	// An EXPLICIT ActionDemote always moves aside. Naming the action is a
	// contract -- "write the words, set the file aside" -- and a legacy flag the
	// caller may not even have set must not silently convert that into an
	// irreversible unlink. Only a demote DERIVED from --on-fail/--purge carries
	// those flags' own semantics, which is what preserves the CLI behavior.
	explicitDemote := o.MisSyncedAction == ActionDemote
	misSynced, categorical = o.MisSyncedAction, o.CategoricalAction
	demoteRemoval = ActionQuarantine
	if !explicitDemote && o.Purge {
		demoteRemoval = ActionPurge
	}
	if misSynced == "" {
		switch {
		case o.OnFail == Delete && o.Purge:
			misSynced = ActionPurge
		case o.OnFail == Delete:
			misSynced = ActionQuarantine
		default:
			misSynced = ActionDemote
		}
	}
	if categorical == "" {
		categorical = ActionQuarantine
		if o.Purge {
			categorical = ActionPurge
		}
	}
	return misSynced, categorical, demoteRemoval
}

// quarantines reports whether any resolved action moves a file into the
// quarantine root, and therefore whether QuarantineDir is required.
//
// ActionDemote counts CONDITIONALLY, which is the subtle part. Demoting is two
// halves -- write the words as .txt, then get rid of the .lrc -- and only the
// second half needs a destination. Which flavor that second half takes is what
// the legacy --purge flag decides: with it, the .lrc is unlinked in place and
// no quarantine root is involved at all (removalMove returns KindPurge with no
// Target, and misSyncedMove then relabels it KindDemote). Serve mode never sets
// Purge, so its `demote` always means "move the .lrc aside", exactly as the
// config docs promise.
//
// Getting this wrong in either direction is a real defect, not a nicety:
// demanding a directory for a purging run would reject a working CLI
// invocation, and NOT demanding one for a quarantining run would plan moves to
// a bare relative path.
func (o Options) quarantines() bool {
	misSynced, categorical, demoteRemoval := o.resolvedActions()
	for _, a := range []Action{misSynced, categorical} {
		switch a {
		case ActionQuarantine:
			return true
		case ActionDemote:
			// Needs a destination unless its removal half unlinks in place.
			// Keyed on the RESOLVED removal mode, so an explicit demote (which
			// always moves aside) is never excused by the legacy flag.
			if demoteRemoval != ActionPurge {
				return true
			}
		}
	}
	return false
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
	// The actions are validated when set explicitly. An empty value is legal and
	// means "derive from the legacy flags" (New does that), so it is not checked
	// here -- Validate runs on the caller's Options, before New normalizes them.
	if o.MisSyncedAction != "" && !validAction(o.MisSyncedAction, misSyncedActions()) {
		return fmt.Errorf("revalidate: unknown MisSynced action %q (want demote, quarantine, purge, or off)", o.MisSyncedAction)
	}
	if o.CategoricalAction != "" && !validAction(o.CategoricalAction, categoricalActions()) {
		return fmt.Errorf("revalidate: unknown categorical action %q (want quarantine, purge, or off; a categorical lyric has no words worth demoting)", o.CategoricalAction)
	}
	// A quarantine destination is required only when something can actually be
	// quarantined. Keyed on the RESOLVED actions rather than on the legacy Purge
	// flag alone: a sweep configured to quarantine with no directory would
	// otherwise plan moves whose target is a bare relative path.
	if o.quarantines() && strings.TrimSpace(o.QuarantineDir) == "" {
		return errors.New("revalidate: a quarantine directory is required unless every action is purge or off")
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
	if o.quarantines() {
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
