package revalidate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// TestDirListingCacheReadsOrphanDirectoryOnce is the fix for the CodeRabbit
// finding on #801: a directory holding several ORPHAN sidecars (no companion
// at all, so every one of them reaches companionAudioByListing) must pay for
// one os.ReadDir of that directory for the whole Plan run, not one per
// orphan -- otherwise the fallback re-introduces the exact per-directory
// O(N) cost #691 removed from the probe's hit path, just on the miss path
// instead. VERIFIED REACHABLE: this is not a hypothetical shape -- #740 (see
// this repo's memory) is a real production incident where a library
// reorganization left many orphans concentrated in a handful of
// directories, so the miss path is not a rare corner here.
func TestDirListingCacheReadsOrphanDirectoryOnce(t *testing.T) {
	dir := t.TempDir()
	const orphans = 5
	var lrcPaths []string
	for i := 0; i < orphans; i++ {
		p := filepath.Join(dir, "orphan"+strconv.Itoa(i)+".lrc")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		lrcPaths = append(lrcPaths, p)
	}

	cache := newDirListingCache()
	for _, p := range lrcPaths {
		if _, ok := companionAudio(p, cache); ok {
			t.Fatalf("companionAudio(%q) = ok, want a miss (no companion was written)", p)
		}
	}
	if got := cache.Reads(); got != 1 {
		t.Errorf("cache.Reads() = %d, want 1 (one os.ReadDir for %d orphans sharing a directory)", got, orphans)
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

// ---------------------------------------------------------------------------
// companionAudio: stem matching (#691).
//
// #691: companionAudio used to os.ReadDir its sidecar's directory once PER
// SIDECAR -- O(N) directory reads for N sidecars sharing a directory. It is
// now a bounded stem probe (os.Stat per candidate audio extension), which
// changes the access pattern but must preserve every existing matching
// invariant exactly: exact stem only, audio gating via scanner's extension
// list, directories skipped.
// ---------------------------------------------------------------------------

// TestCompanionAudioExactStemOnly is the correctness pin #691 requires: a
// same-stem non-audio file and a similar-but-different-stem audio file must
// both be rejected, a directory sharing an audio-like name must be skipped,
// and only the exact-stem audio file is returned. A stem-probe implementation
// that is loose about matching (e.g. a prefix match) would pass this only by
// accident, since song2.mp3 and song.mp3-as-a-directory are both deliberately
// present as traps.
func TestCompanionAudioExactStemOnly(t *testing.T) {
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("song.lrc")
	write("song.txt")  // same stem, not audio -- must NOT match
	write("song2.mp3") // similar but different stem -- must NOT match
	if err := os.MkdirAll(filepath.Join(dir, "song.mp3"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err) // a DIRECTORY named like audio -- must be skipped
	}
	write("song.flac") // the real companion

	got, ok := companionAudio(lrc, newDirListingCache())
	if !ok {
		t.Fatal("companionAudio: ok = false, want true")
	}
	if want := filepath.Join(dir, "song.flac"); got != want {
		t.Errorf("companionAudio = %q, want %q", got, want)
	}
}

// TestCompanionAudioNoMatch: no exact-stem audio present, no false positive
// from the similarly-named file.
func TestCompanionAudioNoMatch(t *testing.T) {
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(lrc, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "song2.mp3"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := companionAudio(lrc, newDirListingCache()); ok {
		t.Error("companionAudio: ok = true, want false (no exact-stem audio present)")
	}
}

// caseSensitiveFS reports whether dir's filesystem distinguishes "a" from "A"
// in a file name, by writing a lowercase-named file and probing for it under
// an uppercased name. macOS (APFS, the default here) and Windows are
// case-insensitive; the production deployment (Linux/Unraid array disks,
// which is the whole motivation for #691) is case-sensitive. The uppercase-
// and mixed-case companion tests below only exercise the interesting branch
// on a case-sensitive filesystem, so they skip rather than assert on the
// wrong platform's semantics.
//
// This is vacuous only on a case-insensitive dev box (macOS/default TMPDIR).
// Every job in .github/workflows/ci.yml -- including the `test` job that runs
// this package -- pins `runs-on: ubuntu-latest`, whose default filesystems
// (ext4, tmpfs under t.TempDir()) are case-sensitive, so both tests run for
// real on CI and would fail there if the ReadDir fallback the mixed-case test
// pins were ever removed. Nothing in this repo runs these packages on a
// macOS or Windows runner.
func caseSensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "casecheck.tmp")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatalf("write case probe: %v", err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASECHECK.tmp"))
	return os.IsNotExist(err)
}

// TestCompanionAudioExtensionUppercase: the old ReadDir loop matched an
// extension via scanner.IsAudioFile, which lowercases before comparing, so it
// recognized a companion of ANY on-disk casing. The stem probe cannot check
// arbitrary casing without listing the directory (the exact cost #691
// removes), so it probes both the lower- and upper-case form of each
// extension -- pinning that an all-caps companion (a legacy Windows-rip
// pattern) is still found. Only meaningful on a case-sensitive filesystem
// (see caseSensitiveFS); on a case-insensitive one the lowercase candidate
// alone would already resolve to the same file, so the test would pass
// vacuously and prove nothing.
func TestCompanionAudioExtensionUppercase(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; this test needs a case-sensitive one to be meaningful")
	}
	lrc := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(lrc, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "song.MP3"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := companionAudio(lrc, newDirListingCache())
	if !ok {
		t.Fatal("companionAudio: ok = false, want true (all-caps extension must match)")
	}
	if want := filepath.Join(dir, "song.MP3"); got != want {
		t.Errorf("companionAudio = %q, want %q", got, want)
	}
}

// TestCompanionAudioExtensionMixedCaseFound pins the invariant #691 names
// explicitly: scanner.IsAudioFile still gates what counts as audio. IsAudioFile
// lowercases before comparing, so it accepts ANY casing -- an extension the stem
// probe's lower/upper pair does not cover must still resolve, via the ReadDir
// fallback on the probe's miss path.
//
// This is the test that fails if the fallback is ever deleted as dead code, and
// it is meaningful ONLY on a case-sensitive filesystem: on macOS/APFS os.Stat
// resolves song.Mp3 through the probe itself, so the fallback is never reached
// and the test would pass with it removed. It runs for real on Linux CI and on
// the production deployment, which is where a regression would actually bite --
// a companion that stops being found is a sidecar that is silently never
// remediated, not a visible error.
func TestCompanionAudioExtensionMixedCaseFound(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; the probe resolves this without the fallback, so the assertion would be vacuous")
	}
	lrc := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(lrc, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := filepath.Join(dir, "song.Mp3")
	if err := os.WriteFile(want, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := companionAudio(lrc, newDirListingCache())
	if !ok {
		t.Fatal("companionAudio: ok = false, want true (IsAudioFile accepts any casing; the ReadDir fallback must find it)")
	}
	if got != want {
		t.Errorf("companionAudio = %s, want %s", got, want)
	}
}

// TestCompanionAudioPicksLexicographicallyFirstFilename pins the pre-#691
// selection rule for a directory holding MORE THAN ONE stem-matching
// companion. os.ReadDir returns entries sorted by filename, so the old
// ReadDir loop always returned the lexicographically-first match; the #691
// probe instead walks scanner.SupportedAudioExtensions() in EXTENSION-LIST
// order (.mp3 before .flac), so on a directory holding both song.mp3 and
// song.flac it silently started returning song.mp3 instead. Which file wins
// is not cosmetic: it is the file whose duration judges the sidecar's
// timing verdict, so a changed winner can move or delete a CORRECT .lrc for
// the wrong reason. "song.flac" < "song.mp3" byte-wise ('f' < 'm'), so the
// old code (and this fix) picks song.flac; the unfixed probe picks
// song.mp3 -- this is the assertion that must redden without the fix.
func TestCompanionAudioPicksLexicographicallyFirstFilename(t *testing.T) {
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	for _, name := range []string{"song.mp3", "song.flac"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, ok := companionAudio(lrc, newDirListingCache())
	if !ok {
		t.Fatal("companionAudio: ok = false, want true")
	}
	if want := filepath.Join(dir, "song.flac"); got != want {
		t.Errorf("companionAudio = %q, want %q (lexicographically-first filename, matching the pre-#691 os.ReadDir order)", got, want)
	}
}

// TestCompanionAudioSameExtensionCaseVariantOrder pins the within-extension
// half of the ordering fix: a directory holding BOTH song.mp3 and song.MP3
// (two genuinely distinct files, only possible on a case-sensitive
// filesystem) must resolve to the same winner os.ReadDir's sort would have
// picked. ASCII uppercase sorts before lowercase ('M'=0x4D < 'm'=0x6D), and
// this is verified directly against os.ReadDir on the case-sensitive
// /tmp/cs-mount volume (not merely asserted): a directory holding song.MP3,
// song.flac, and song.mp3 lists them in that order. So song.MP3 must win over
// song.mp3 here. Skips on a case-insensitive filesystem, where the two names
// alias one file and the case-collision path (os.SameFile) is exercised
// instead, not this cross-file compare.
func TestCompanionAudioSameExtensionCaseVariantOrder(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; song.mp3 and song.MP3 would alias the same file")
	}
	lrc := filepath.Join(dir, "song.lrc")
	for _, name := range []string{"song.mp3", "song.MP3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, ok := companionAudio(lrc, newDirListingCache())
	if !ok {
		t.Fatal("companionAudio: ok = false, want true")
	}
	if want := filepath.Join(dir, "song.MP3"); got != want {
		t.Errorf("companionAudio = %q, want %q (uppercase extension sorts first, matching os.ReadDir order)", got, want)
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
