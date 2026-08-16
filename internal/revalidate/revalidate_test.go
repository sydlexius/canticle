package revalidate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/realign"
	"github.com/sydlexius/canticle/internal/timing"
)

// trackSeconds is the duration every fixture audio file reports. The .lrc
// bodies above are written against it, so a cue past 2:02 (Tolerance) is
// MisSynced and one past 3:00 (CategoricalRatio) is Categorical.
const trackSeconds = 120

// fixedDuration returns a DurationLookup reporting trackSeconds for every file.
func fixedDuration() DurationLookup {
	return func(context.Context, string, int64, int64) (int, bool, error) {
		return trackSeconds, true, nil
	}
}

// missingDuration returns a DurationLookup that always misses -- the cold-cache
// case (#441), which must fail open.
func missingDuration() DurationLookup {
	return func(context.Context, string, int64, int64) (int, bool, error) {
		return 0, false, nil
	}
}

// lib builds a library root holding one audio file and its .lrc sidecar, and
// returns (root, lrcPath).
func lib(t *testing.T, lrcBody string) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	audio := filepath.Join(dir, "track.mp3")
	if err := os.WriteFile(audio, []byte("not really audio"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	lrc := filepath.Join(dir, "track.lrc")
	if err := os.WriteFile(lrc, []byte(lrcBody), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	return root, lrc
}

// newRevalidator wires a Revalidator over root with a quarantine dir alongside.
func newRevalidator(t *testing.T, root string, lookup DurationLookup, mutate func(*Options)) (*Revalidator, string) {
	t.Helper()
	quarantine := filepath.Join(t.TempDir(), "quarantine")
	opts := Options{Roots: []string{root}, QuarantineDir: quarantine}
	if mutate != nil {
		mutate(&opts)
	}
	return New(lookup, opts), quarantine
}

// snapshotTree records every file under root and its bytes, so a test can prove
// a pass wrote NOTHING rather than merely prove one expected file survived.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[p] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %q: %v", root, err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// RAIL 1: dry run writes NOTHING.
// ---------------------------------------------------------------------------

// TestPlanWritesNothing is the dry-run rail. Plan classifies a file that WOULD be
// remediated in every mode, and the tree must be byte-identical afterwards --
// including the absence of a quarantine directory and of any backup file.
func TestPlanWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"mis_synced", "[00:10.00]alpha\n[02:30.00]beta\n"},
		{"categorical", "[00:10.00]alpha\n[05:00.00]beta\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := lib(t, tc.body)
			before := snapshotTree(t, root)

			rv, quarantine := newRevalidator(t, root, fixedDuration(), nil)
			plan, err := rv.Plan(t.Context())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.Moves) == 0 {
				t.Fatal("expected a planned move; the fixture must be remediable for this rail to mean anything")
			}
			if after := snapshotTree(t, root); !sameTree(before, after) {
				t.Errorf("Plan MUTATED the tree.\nbefore: %v\nafter:  %v", before, after)
			}
			if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
				t.Errorf("Plan created the quarantine directory %q; a dry run must write nothing", quarantine)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RAIL 2: unknown duration fails open.
// ---------------------------------------------------------------------------

// TestUnknownDurationFailsOpen is the fail-open rail. The .lrc is grossly
// over-running (categorical against any plausible duration), but the duration
// cache misses, so NO move may be planned and the file must survive an apply.
func TestUnknownDurationFailsOpen(t *testing.T) {
	root, lrc := lib(t, "[00:10.00]alpha\n[59:00.00]beta\n")
	rv, _ := newRevalidator(t, root, missingDuration(), nil)

	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Counts.UnknownDuration != 1 {
		t.Errorf("UnknownDuration count = %d, want 1", plan.Counts.UnknownDuration)
	}
	if len(plan.Moves) != 0 {
		t.Fatalf("planned %d move(s) on an UNKNOWN duration; nothing may ever be remediated without a duration: %+v", len(plan.Moves), plan.Moves)
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("the .lrc must survive an unknown-duration pass: %v", err)
	}
}

// TestNilLookupFailsOpen: no duration store at all degrades the same way.
func TestNilLookupFailsOpen(t *testing.T) {
	root, _ := lib(t, "[00:10.00]alpha\n[59:00.00]beta\n")
	rv, _ := newRevalidator(t, root, nil, nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("planned %d move(s) with no duration store at all", len(plan.Moves))
	}
}

// TestDurationStoreErrorDoesNotRemediate: a broken store counts as an error and
// still plans nothing.
func TestDurationStoreErrorDoesNotRemediate(t *testing.T) {
	root, _ := lib(t, "[00:10.00]alpha\n[59:00.00]beta\n")
	broken := DurationLookup(func(context.Context, string, int64, int64) (int, bool, error) {
		return 0, false, errors.New("store is down")
	})
	rv, _ := newRevalidator(t, root, broken, nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("planned %d move(s) despite a duration-store failure", len(plan.Moves))
	}
	if plan.Counts.Errored != 1 {
		t.Errorf("Errored = %d, want 1", plan.Counts.Errored)
	}
}

// ---------------------------------------------------------------------------
// RAIL 3: the trailing decorative marker is NOT remediated.
// ---------------------------------------------------------------------------

// TestTrailingDecorativeMarkerIsNotRemediated is the ~33%-of-the-flagged-tail
// case from Investigation-0 on #438: a perfectly-synced lyric whose only
// past-duration timestamp is a decorative music-note marker. A naive max
// timestamp calls this categorical and destroys a good file; consuming
// timing.Evaluate's corrected max calls it Ok and leaves it alone.
func TestTrailingDecorativeMarkerIsNotRemediated(t *testing.T) {
	for _, marker := range []string{"♪", "♫", "♪ Instrumental ♪"} {
		root, lrc := lib(t, "[00:10.00]alpha\n[01:50.00]beta\n[09:00.00]"+marker+"\n")
		rv, _ := newRevalidator(t, root, fixedDuration(), nil)

		plan, err := rv.Plan(t.Context())
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if plan.Counts.Ok != 1 {
			t.Errorf("marker %q: Ok = %d, want 1 (counts=%+v)", marker, plan.Counts.Ok, plan.Counts)
		}
		if len(plan.Moves) != 0 {
			t.Fatalf("marker %q: planned %d move(s) for a lyric whose only overrun is a DECORATIVE marker", marker, len(plan.Moves))
		}
		if _, err := os.Stat(lrc); err != nil {
			t.Errorf("marker %q: the .lrc was removed: %v", marker, err)
		}
	}
}

// ---------------------------------------------------------------------------
// RAIL 4: removal is a MOVE, not a delete.
// ---------------------------------------------------------------------------

// TestCategoricalQuarantinesRatherThanDeletes is the reversibility rail: after an
// apply the .lrc is gone from the library but PRESENT and byte-identical under
// the quarantine root, so the operator can move it back.
func TestCategoricalQuarantinesRatherThanDeletes(t *testing.T) {
	body := "[00:10.00]alpha\n[05:00.00]beta\n"
	root, lrc := lib(t, body)
	rv, quarantine := newRevalidator(t, root, fixedDuration(), nil)

	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Counts.Categorical != 1 {
		t.Fatalf("Categorical = %d, want 1", plan.Counts.Categorical)
	}
	if got := plan.Moves[0].Kind; got != realign.KindQuarantine {
		t.Fatalf("kind = %q, want %q -- removal must be reversible by default", got, realign.KindQuarantine)
	}

	applyPlan(t, plan)

	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the .lrc is still in the library after quarantine: %v", err)
	}
	quarantined := filepath.Join(quarantine, "album", "track.lrc")
	got, err := os.ReadFile(quarantined)
	if err != nil {
		t.Fatalf("the quarantined copy is missing -- the file was DELETED, not moved aside: %v", err)
	}
	if string(got) != body {
		t.Errorf("quarantined content = %q, want the original %q", got, body)
	}
}

// TestPurgeIsOptInAndDeletes: --purge is the explicit escape hatch.
func TestPurgeIsOptInAndDeletes(t *testing.T) {
	root, lrc := lib(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	rv, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) { o.Purge = true })

	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := plan.Moves[0].Kind; got != realign.KindPurge {
		t.Fatalf("kind = %q, want %q", got, realign.KindPurge)
	}
	applyPlan(t, plan)
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("--purge must actually delete: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Demotion.
// ---------------------------------------------------------------------------

// TestDemotionKeepsWordsAsTxt: the words are content-correct, so they are kept
// as .txt and the .lrc is quarantined -- never deleted outright.
func TestDemotionKeepsWordsAsTxt(t *testing.T) {
	root, lrc := lib(t, "[00:10.00]alpha\n[00:20.00]♪\n[02:30.00]beta\n")
	rv, quarantine := newRevalidator(t, root, fixedDuration(), nil)

	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Counts.MisSynced != 1 {
		t.Fatalf("MisSynced = %d, want 1 (counts=%+v)", plan.Counts.MisSynced, plan.Counts)
	}
	if got := plan.Moves[0].Kind; got != realign.KindDemote {
		t.Fatalf("kind = %q, want %q", got, realign.KindDemote)
	}
	applyPlan(t, plan)

	txt := filepath.Join(root, "album", "track.txt")
	body, err := os.ReadFile(txt)
	if err != nil {
		t.Fatalf("demoted .txt missing: %v", err)
	}
	if got, want := string(body), "alpha\nbeta\n"; got != want {
		t.Errorf("demoted body = %q, want %q (the decorative cue must be dropped)", got, want)
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the mistimed .lrc should have been moved aside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(quarantine, "album", "track.lrc")); err != nil {
		t.Errorf("the demoted .lrc is not recoverable from quarantine: %v", err)
	}
}

// TestDemoteNeverOverwritesSettledTxt: an existing .txt outranks anything this
// pass would write, matching the accept-time guard's settled-sidecar rule.
func TestDemoteNeverOverwritesSettledTxt(t *testing.T) {
	root, _ := lib(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	txt := filepath.Join(root, "album", "track.txt")
	if err := os.WriteFile(txt, []byte("settled words\n"), 0o600); err != nil {
		t.Fatalf("write settled txt: %v", err)
	}
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	applyPlan(t, plan)

	got, err := os.ReadFile(txt)
	if err != nil {
		t.Fatalf("read txt: %v", err)
	}
	if string(got) != "settled words\n" {
		t.Errorf("settled .txt was overwritten: %q", got)
	}
}

// TestOnFailDeleteSkipsTheTxt: --on-fail=delete quarantines the .lrc with no
// .txt written in its place.
func TestOnFailDeleteSkipsTheTxt(t *testing.T) {
	root, _ := lib(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	rv, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) { o.OnFail = Delete })
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := plan.Moves[0].Kind; got != realign.KindQuarantine {
		t.Fatalf("kind = %q, want %q", got, realign.KindQuarantine)
	}
	applyPlan(t, plan)
	if _, err := os.Stat(filepath.Join(root, "album", "track.txt")); !os.IsNotExist(err) {
		t.Errorf("--on-fail=delete must not write a .txt")
	}
}

// TestAllDecorativeOverrunIsNotRemediated: nothing worth keeping means no
// empty .txt is written.
func TestAllDecorativeOverrunIsNotRemediated(t *testing.T) {
	root, _ := lib(t, "[00:10.00]♪\n[02:30.00]♪\n")
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// An all-decorative lyric carries no timing evidence at all, so the verdict
	// is Ok and nothing is planned.
	if len(plan.Moves) != 0 {
		t.Fatalf("planned %d move(s) for an all-decorative lyric", len(plan.Moves))
	}
	if plan.Counts.Ok != 1 {
		t.Errorf("Ok = %d, want 1", plan.Counts.Ok)
	}
}

// ---------------------------------------------------------------------------
// Walk behavior.
// ---------------------------------------------------------------------------

// TestLrcWithoutCompanionAudioIsNotRemediated: an orphan is realign's problem.
func TestLrcWithoutCompanionAudioIsNotRemediated(t *testing.T) {
	root := t.TempDir()
	lrc := filepath.Join(root, "orphan.lrc")
	if err := os.WriteFile(lrc, []byte("[00:10.00]alpha\n[59:00.00]beta\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Counts.NoAudio != 1 {
		t.Errorf("NoAudio = %d, want 1", plan.Counts.NoAudio)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("planned %d move(s) for a sidecar with no companion audio", len(plan.Moves))
	}
}

// TestSymlinkedSidecarIsNeverRemediated: a link must not redirect a move or a
// delete out of the library root.
func TestSymlinkedSidecarIsNeverRemediated(t *testing.T) {
	root, lrc := lib(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	outside := filepath.Join(t.TempDir(), "elsewhere.lrc")
	if err := os.WriteFile(outside, []byte("[00:10.00]alpha\n[05:00.00]beta\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Remove(lrc); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, lrc); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("planned %d move(s) for a SYMLINKED sidecar", len(plan.Moves))
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Errorf("the symlink target was touched: %v", err)
	}
}

// TestTxtSidecarsAreIgnored: this pass judges .lrc timing only.
func TestTxtSidecarsAreIgnored(t *testing.T) {
	root, _ := lib(t, "[00:10.00]alpha\n[01:00.00]beta\n")
	if err := os.WriteFile(filepath.Join(root, "album", "other.txt"), []byte("words\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Counts.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1 (.txt must not be scanned)", plan.Counts.Scanned)
	}
}

// TestMissingRootIsNotFatal: a configured root that no longer exists is skipped.
func TestMissingRootIsNotFatal(t *testing.T) {
	rv := New(fixedDuration(), Options{Roots: []string{filepath.Join(t.TempDir(), "gone")}, QuarantineDir: t.TempDir()})
	if _, err := rv.Plan(t.Context()); err != nil {
		t.Errorf("a missing root must not fail the run: %v", err)
	}
}

// TestPlanHonorsContextCancellation.
func TestPlanHonorsContextCancellation(t *testing.T) {
	root, _ := lib(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)
	if _, err := rv.Plan(ctx); err == nil {
		t.Error("want an error from a canceled context")
	}
}

// ---------------------------------------------------------------------------
// Options validation.
// ---------------------------------------------------------------------------

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"default demote with quarantine", Options{QuarantineDir: "/tmp/q"}, false},
		{"delete with quarantine", Options{OnFail: Delete, QuarantineDir: "/tmp/q"}, false},
		{"purge needs no quarantine", Options{Purge: true}, false},
		{"no quarantine, no purge", Options{}, true},
		{"unknown on-fail", Options{OnFail: "shred", QuarantineDir: "/tmp/q"}, true},
		// A quarantine root inside a scanned root re-walks its own output on the
		// next pass. Rejected up front rather than left to grow quietly.
		{"quarantine inside a root", Options{
			Roots:         []string{"/tmp/lib"},
			QuarantineDir: "/tmp/lib/quarantine",
		}, true},
		{"quarantine IS a root", Options{
			Roots:         []string{"/tmp/lib"},
			QuarantineDir: "/tmp/lib",
		}, true},
		// A sibling whose name merely SHARES A PREFIX with the root is fine --
		// the check must compare path components, not raw strings.
		{"quarantine is a prefix-sharing sibling", Options{
			Roots:         []string{"/tmp/lib"},
			QuarantineDir: "/tmp/library-quarantine",
		}, false},
		{"quarantine outside every root", Options{
			Roots:         []string{"/tmp/lib"},
			QuarantineDir: "/tmp/q",
		}, false},
		// --purge writes nothing to a quarantine dir, so the containment rule
		// does not apply and must not fire.
		{"purge ignores an inside-root quarantine", Options{
			Purge:         true,
			Roots:         []string{"/tmp/lib"},
			QuarantineDir: "/tmp/lib/quarantine",
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.opts.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestOutcomeVocabularyMatchesTiming pins the report to internal/timing's own
// constants, so a bucket rename there cannot silently mislabel this report.
func TestOutcomeVocabularyMatchesTiming(t *testing.T) {
	root, _ := lib(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Findings[0].Outcome != timing.MisSynced {
		t.Errorf("outcome = %q, want the timing constant %q", plan.Findings[0].Outcome, timing.MisSynced)
	}
}

// applyPlan runs a plan's moves through realign's shared Apply path and fails on
// any per-move error.
func applyPlan(t *testing.T, plan Plan) []realign.Applied {
	t.Helper()
	backup := filepath.Join(t.TempDir(), "backup.jsonl")
	r := realign.New(nil, config.RealignConfig{})
	applied, err := r.Apply(plan.Moves, backup, realign.Policy{AllowHeuristic: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, a := range applied {
		if a.Err != nil {
			t.Fatalf("apply %s: %v", a.Move.Kind, a.Err)
		}
	}
	b, rerr := os.ReadFile(backup)
	if rerr != nil {
		t.Fatalf("a mutating apply wrote no backup trail: %v", rerr)
	}
	if len(plan.Moves) > 0 && !strings.Contains(string(b), plan.Moves[0].Kind) {
		t.Errorf("backup record does not name the action kind: %s", b)
	}
	return applied
}

// TestDegenerateOnDiskIsDemoted covers the #673 repair path: a degenerate .lrc
// ALREADY on disk (every cue at one timestamp) is found by this sweep and
// demoted to .txt, keeping the words.
//
// The existing corpus is the reason this arm exists. The accept-time guard stops
// new ones, but five such files were found on a production library before the
// predicate existed, and nothing else walks .lrc files re-judging them. Before
// this arm the outcome fell to classify's default, which counts an unrecognized
// verdict as Errored and deliberately never remediates -- so the population
// would have stayed exactly as it was, silently.
func TestDegenerateOnDiskIsDemoted(t *testing.T) {
	// Every cue at 00:00.00 -- the production shape. Well inside trackSeconds,
	// so the overrun predicate reports Ok and cannot be what catches it.
	root, lrc := lib(t, "[00:00.00]alpha\n[00:00.00]beta\n[00:00.00]gamma\n")
	rv, quarantine := newRevalidator(t, root, fixedDuration(), nil)

	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Counts.Degenerate != 1 {
		t.Fatalf("Degenerate = %d, want 1 (counts=%+v)", plan.Counts.Degenerate, plan.Counts)
	}
	// It must NOT be miscounted as an error: an error is "we could not judge
	// this", which would hide a file the sweep actually understands.
	if plan.Counts.Errored != 0 {
		t.Errorf("Errored = %d, want 0 -- a degenerate file is judged, not unreadable", plan.Counts.Errored)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("moves = %d, want 1", len(plan.Moves))
	}
	if got := plan.Moves[0].Kind; got != realign.KindDemote {
		t.Fatalf("kind = %q, want %q -- the words are correct, only the timing is fake", got, realign.KindDemote)
	}

	applyPlan(t, plan)

	body, err := os.ReadFile(filepath.Join(root, "album", "track.txt"))
	if err != nil {
		t.Fatalf("demoted .txt missing: %v", err)
	}
	if got, want := string(body), "alpha\nbeta\ngamma\n"; got != want {
		t.Errorf("demoted body = %q, want %q", got, want)
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the degenerate .lrc should have been moved aside: %v", err)
	}
	// Recoverable: this pass is backup-first like every other remediation here.
	if _, err := os.Stat(filepath.Join(quarantine, "album", "track.lrc")); err != nil {
		t.Errorf("the demoted .lrc is not recoverable from quarantine: %v", err)
	}
}

// TestGenuinelySyncedIsNotDemotedAsDegenerate is the control this arm needs, and
// an explicit #673 acceptance criterion: a real synced file must be untouched.
// Without it the test above would also pass on a predicate that demoted
// everything.
func TestGenuinelySyncedIsNotDemotedAsDegenerate(t *testing.T) {
	root, lrc := lib(t, "[00:10.00]alpha\n[00:25.00]beta\n[00:40.00]gamma\n")
	rv, _ := newRevalidator(t, root, fixedDuration(), nil)

	plan, err := rv.Plan(t.Context())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Counts.Degenerate != 0 {
		t.Errorf("Degenerate = %d, want 0 -- distinct timestamps are a real sync", plan.Counts.Degenerate)
	}
	if plan.Counts.Ok != 1 {
		t.Errorf("Ok = %d, want 1 (counts=%+v)", plan.Counts.Ok, plan.Counts)
	}
	if len(plan.Moves) != 0 {
		t.Fatalf("moves = %d, want 0 -- a synced file must never be remediated", len(plan.Moves))
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("the synced .lrc must be left alone: %v", err)
	}
}
