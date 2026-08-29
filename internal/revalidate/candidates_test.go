package revalidate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/timing"
)

// overrunBody (action_test.go) is the shared MisSynced fixture: its last cue
// runs past trackSeconds + Tolerance.

// candidateFor builds the Candidate a sweep would derive for a sidecar sitting
// under root, exactly as the sweep derives it: from the AUDIO path.
func candidateFor(id int64, root, lrc string) Candidate {
	return Candidate{
		ID:        id,
		AudioPath: filepath.Join(filepath.Dir(lrc), "track.mp3"),
		Root:      root,
	}
}

// ---------------------------------------------------------------------------
// THE CONVERGENCE RAIL: every candidate produces a finding carrying its ID.
// ---------------------------------------------------------------------------

// TestPlanCandidatesAlwaysFindsEveryCandidate is the convergence rail, and it is
// the most important test in this file.
//
// A row leaves the backlog by being STAMPED, and the sweep stamps what
// PlanCandidates returns a finding for. So a candidate silently dropped here is
// a row that returns in the next batch -- and because the backlog is ordered
// oldest-first, a handful of undroppable rows would sit at the head of every
// cycle forever, and the sweep would never reach the rest of the backlog nor
// idle. This asserts the property directly: whatever the input, EVERY candidate
// comes back with its ID, including the ones no verdict could be reached for.
func TestPlanCandidatesAlwaysFindsEveryCandidate(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	dir := filepath.Dir(lrc)

	// A sidecar-less row: the audio exists, the .lrc was deleted by hand.
	bare := filepath.Join(dir, "gone.mp3")
	if err := os.WriteFile(bare, []byte("not really audio"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	// An audio-less row: the .lrc is there, its companion is not.
	orphanLRC := filepath.Join(dir, "orphan.lrc")
	if err := os.WriteFile(orphanLRC, []byte(overrunBody), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}

	candidates := []Candidate{
		candidateFor(1, root, lrc),
		{ID: 2, AudioPath: bare, Root: root},
		{ID: 3, AudioPath: filepath.Join(dir, "orphan.mp3"), Root: root},
		{ID: 4, AudioPath: "", Root: root},
		{ID: 5, AudioPath: filepath.Join(dir, "never-existed.mp3"), Root: root},
	}

	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionOff
		o.CategoricalAction = ActionOff
	})
	plan, err := r.PlanCandidates(context.Background(), candidates)
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}

	seen := map[int64]bool{}
	for _, f := range plan.Findings {
		if f.ID == 0 {
			t.Errorf("finding for %q carries no ID; the sweep could never stamp it", f.Path)
			continue
		}
		if seen[f.ID] {
			t.Errorf("candidate %d produced more than one finding", f.ID)
		}
		seen[f.ID] = true
	}
	for _, c := range candidates {
		if !seen[c.ID] {
			t.Errorf("candidate %d produced NO finding, so its row would never be stamped and would head-of-line the batch forever", c.ID)
		}
	}
}

// TestPlanCandidatesJudgesWithoutWalking proves the candidate path reaches the
// same verdict as the walk for the same file. If it did not, an operator's
// `revalidate` preview would not predict what the unattended sweep does.
func TestPlanCandidatesJudgesWithoutWalking(t *testing.T) {
	root, lrc := lib(t, overrunBody)

	// The walk's verdict, for comparison.
	rWalk, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionOff
		o.CategoricalAction = ActionOff
	})
	walked, err := rWalk.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(walked.Findings) != 1 || walked.Findings[0].Outcome != timing.MisSynced {
		t.Fatalf("walk did not produce the expected single MisSynced finding: %+v", walked.Findings)
	}

	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionOff
		o.CategoricalAction = ActionOff
	})
	plan, err := r.PlanCandidates(context.Background(), []Candidate{candidateFor(7, root, lrc)})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(plan.Findings))
	}
	got := plan.Findings[0]
	if got.Outcome != walked.Findings[0].Outcome {
		t.Errorf("candidate mode reached %q where the walk reached %q; the CLI preview would not predict the sweep",
			got.Outcome, walked.Findings[0].Outcome)
	}
	if got.Path != lrc {
		t.Errorf("sidecar path = %q, want %q (derived from the audio stem)", got.Path, lrc)
	}
	if got.ID != 7 {
		t.Errorf("finding ID = %d, want 7", got.ID)
	}
	if plan.Counts.MisSynced != 1 {
		t.Errorf("MisSynced count = %d, want 1", plan.Counts.MisSynced)
	}
}

// TestPlanCandidatesWritesNothing is the dry-run rail for candidate mode. It
// must hold as firmly as it does for the walk: PlanCandidates plans, Apply acts.
func TestPlanCandidatesWritesNothing(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	before := snapshotTree(t, root)

	r, quarantine := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := r.PlanCandidates(context.Background(), []Candidate{candidateFor(1, root, lrc)})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Moves) == 0 {
		t.Fatal("expected a planned move for a MisSynced file; without one this test proves nothing")
	}
	if !sameTree(before, snapshotTree(t, root)) {
		t.Error("PlanCandidates modified the library tree")
	}
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Error("PlanCandidates created the quarantine directory")
	}
}

// TestPlanCandidatesQuarantineIsRelativeToTheCandidateRoot pins the per-candidate
// root. One batch spans every library, so a root taken from Options (as the walk
// does) would flatten two libraries' identically-named sidecars into one
// quarantine path -- where the clobber-safe rename would then refuse the second.
func TestPlanCandidatesQuarantineIsRelativeToTheCandidateRoot(t *testing.T) {
	rootA, lrcA := lib(t, overrunBody)
	rootB, lrcB := lib(t, overrunBody)

	r, quarantine := newRevalidator(t, rootA, fixedDuration(), func(o *Options) {
		// Both roots are configured, as they would be in serve mode.
		o.Roots = []string{rootA, rootB}
		o.MisSyncedAction = ActionQuarantine
	})
	plan, err := r.PlanCandidates(context.Background(), []Candidate{
		{ID: 1, AudioPath: filepath.Join(filepath.Dir(lrcA), "track.mp3"), Root: rootA, LibraryID: 11},
		{ID: 2, AudioPath: filepath.Join(filepath.Dir(lrcB), "track.mp3"), Root: rootB, LibraryID: 22},
	})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Moves) != 2 {
		t.Fatalf("want 2 moves, got %d", len(plan.Moves))
	}
	// Both sidecars are album/track.lrc under their own root, so a root-relative
	// layout gives them the SAME target -- which is the collision this test
	// exists to characterize, and which realign's clobber-safe rename refuses.
	// What must NOT happen is one of them being computed against the other's
	// root, which would put a file from library B under library A's tree.
	for _, mv := range plan.Moves {
		rel, rerr := filepath.Rel(quarantine, mv.Target)
		if rerr != nil || rel == "" {
			t.Fatalf("target %q is not under the quarantine root %q", mv.Target, quarantine)
		}
	}
	if plan.Moves[0].LibraryID != 11 || plan.Moves[1].LibraryID != 22 {
		t.Errorf("library ids = %d/%d, want 11/22: a backup record stamped with the wrong library cannot be restored by scope",
			plan.Moves[0].LibraryID, plan.Moves[1].LibraryID)
	}
}

// TestPlanCandidatesUnknownDurationNeverRemediates is the fail-open rail. A cold
// duration cache must leave every file alone -- in an unattended pass even more
// than in the CLI, since nobody is watching it run.
func TestPlanCandidatesUnknownDurationNeverRemediates(t *testing.T) {
	root, lrc := lib(t, overrunBody)

	r, _ := newRevalidator(t, root, missingDuration(), nil)
	plan, err := r.PlanCandidates(context.Background(), []Candidate{candidateFor(1, root, lrc)})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("unknown duration planned %d move(s); it must fail open", len(plan.Moves))
	}
	if plan.Counts.UnknownDuration != 1 {
		t.Errorf("UnknownDuration count = %d, want 1", plan.Counts.UnknownDuration)
	}
	// Still a finding, so the row is stamped and does not come back forever.
	if len(plan.Findings) != 1 || plan.Findings[0].ID != 1 {
		t.Errorf("unknown-duration candidate produced no stampable finding: %+v", plan.Findings)
	}
}

// TestPlanCandidatesSkipsASymlinkedSidecar pins the symlink rail. A link could
// redirect a move or a delete out of the library root entirely.
func TestPlanCandidatesSkipsASymlinkedSidecar(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	dir := filepath.Dir(lrc)

	outside := filepath.Join(t.TempDir(), "elsewhere.lrc")
	if err := os.WriteFile(outside, []byte(overrunBody), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	audio := filepath.Join(dir, "linked.mp3")
	if err := os.WriteFile(audio, []byte("not really audio"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.lrc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := r.PlanCandidates(context.Background(), []Candidate{{ID: 1, AudioPath: audio, Root: root}})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("a symlinked sidecar planned %d move(s); a link must never be a remediation target", len(plan.Moves))
	}
	if plan.Counts.NoSidecar != 1 {
		t.Errorf("NoSidecar count = %d, want 1", plan.Counts.NoSidecar)
	}
}

// TestPlanCandidatesAbandonsTheCycleOnADurationStoreFailure separates the two
// failure lifetimes. A broken duration store is TRANSIENT and says nothing about
// any particular file, so the cycle must abandon rather than stamp a batch of
// rows with verdicts a working store would have judged differently -- the stamp
// is one-way, so a wrong one is not something a later cycle revisits.
func TestPlanCandidatesAbandonsTheCycleOnADurationStoreFailure(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	boom := errors.New("duration store is down")

	r, _ := newRevalidator(t, root, func(context.Context, string, int64, int64) (int, bool, error) {
		return 0, false, boom
	}, nil)
	_, err := r.PlanCandidates(context.Background(), []Candidate{candidateFor(1, root, lrc)})
	if err == nil {
		t.Fatal("a duration-store failure returned no error; the sweep would stamp rows it never judged")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap %v", err, boom)
	}
}

// TestPlanCandidatesHonorsContextCancellation keeps a cycle interruptible: serve
// mode cancels this goroutine on shutdown and wg.Wait must unblock.
func TestPlanCandidatesHonorsContextCancellation(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, _ := newRevalidator(t, root, fixedDuration(), nil)
	_, err := r.PlanCandidates(ctx, []Candidate{candidateFor(1, root, lrc)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// TestQuarantineTargetKeepsRootlessSidecarsDistinct is the head-of-line
// regression test for the rootless collision.
//
// The serve sweep hands an EMPTY root for any candidate under no configured
// library (a removed library row, an edited mount path, a relative
// source_path). The old fallback mapped every such sidecar to
// <quarantineDir>/<basename>, so the first moved and every later one collided.
// The clobber-safe rename correctly refuses a collision, but the sweep reads
// that refusal as a FAILED ACTION and leaves the row unstamped to retry -- so
// the row returns at the head of the oldest-first batch on every cycle forever,
// starving the rest of the backlog. Distinct sources must map to distinct
// targets.
func TestQuarantineTargetKeepsRootlessSidecarsDistinct(t *testing.T) {
	const q = "/var/quarantine"
	a := quarantineTarget(q, "", "/music/albumA/track.lrc")
	b := quarantineTarget(q, "", "/music/albumB/track.lrc")
	if a == b {
		t.Fatalf("two rootless sidecars mapped to one target %q; the second collides, its row is never stamped, and it head-of-lines every future cycle", a)
	}
	for _, got := range []string{a, b} {
		if !strings.HasPrefix(got, q+string(filepath.Separator)) {
			t.Errorf("target %q escaped the quarantine root %q", got, q)
		}
		if strings.Contains(got, "..") {
			t.Errorf("target %q contains a parent traversal", got)
		}
	}
}

// TestQuarantineTargetContainsEveryPathShape keeps the fallback from walking out
// of the quarantine root. A stored path can be absolute, relative, or carry
// traversal segments; none may produce a target outside quarantineDir.
func TestQuarantineTargetContainsEveryPathShape(t *testing.T) {
	const q = "/var/quarantine"
	for _, path := range []string{
		"/music/album/track.lrc",
		"music/album/track.lrc",
		"../../etc/passwd.lrc",
		"/music/../../etc/track.lrc",
		"/",
		"track.lrc",
	} {
		got := quarantineTarget(q, "", path)
		if !strings.HasPrefix(got, q+string(filepath.Separator)) {
			t.Errorf("path %q produced target %q, which is outside the quarantine root", path, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("path %q produced target %q, which contains a parent traversal", path, got)
		}
	}
}

// TestPlanCandidatesArbitratesASharedSidecar is the regression test for the
// worst defect this feature has had: it DESTROYED a correct file.
//
// The sidecar is derived per ROW (stem + ".lrc"), so a directory holding two
// same-stem audio files -- a lossless and a lossy copy of one track, an ordinary
// library shape -- produces TWO backlog rows deriving the SAME .lrc. Judged
// against each row's own duration, the shorter copy's row reads categorical on a
// sidecar correctly timed for the longer one and quarantines it; under
// on_categorical = "purge" it is deleted outright. The verdict would be decided
// by which copy happened to be enqueued, which is not a judgment about the lyric.
//
// The walk never had this defect (companionAudio resolves ONE companion per
// sidecar), so it was introduced by candidate mode and must stay fixed here.
func TestPlanCandidatesArbitratesASharedSidecar(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"track.flac", "track.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not really audio"), 0o600); err != nil {
			t.Fatalf("write audio: %v", err)
		}
	}
	lrc := filepath.Join(dir, "track.lrc")
	// Last cue at 4:50 (290s): correct for the 300s copy, wildly past the 99s one.
	if err := os.WriteFile(lrc, []byte("[00:10.00]alpha\n[04:50.00]beta\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}

	durations := map[string]int{
		filepath.Join(dir, "track.flac"): 300,
		filepath.Join(dir, "track.mp3"):  99,
	}
	r, _ := newRevalidator(t, root, func(_ context.Context, p string, _, _ int64) (int, bool, error) {
		return durations[p], durations[p] > 0, nil
	}, nil)

	plan, err := r.PlanCandidates(context.Background(), []Candidate{
		{ID: 1, AudioPath: filepath.Join(dir, "track.flac"), Root: root},
		{ID: 2, AudioPath: filepath.Join(dir, "track.mp3"), Root: root},
	})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}

	if len(plan.Moves) != 0 {
		t.Errorf("planned %d move(s) against a sidecar that is correctly timed for its own companion; the sweep would destroy it", len(plan.Moves))
	}
	// companionAudio picks the lexicographically-first base name, so track.flac
	// owns the sidecar and track.mp3's row contributes no verdict.
	var judged, disowned int
	for _, f := range plan.Findings {
		switch f.Outcome {
		case timing.Ok:
			judged++
		case "no_sidecar":
			disowned++
		default:
			t.Errorf("unexpected outcome %q for id %d", f.Outcome, f.ID)
		}
	}
	if judged != 1 || disowned != 1 {
		t.Errorf("judged=%d disowned=%d, want 1 and 1: exactly one row owns a sidecar", judged, disowned)
	}
	// BOTH rows still retire, or the disowned one head-of-lines every cycle.
	if len(plan.Findings) != 2 {
		t.Fatalf("want 2 findings, got %d", len(plan.Findings))
	}
	for _, f := range plan.Findings {
		if f.ID == 0 {
			t.Error("a finding carries no ID; its row could never be stamped")
		}
	}
}

// TestPlanCandidatesClaimsASidecarOnce covers the other way two rows reach one
// file: the SAME source_path twice, which the queue's uniqueness constraint
// admits under different artist/title keys.
//
// Both rows agree on the verdict, so this is not a wrong judgment -- but
// planning the move twice means the first consumes the file and the second fails
// "no such file", which the sweep records as a genuine failed action, warns
// about, and which leaves BOTH rows unstamped for a cycle. One move per file.
func TestPlanCandidatesClaimsASidecarOnce(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	audio := filepath.Join(filepath.Dir(lrc), "track.mp3")

	r, _ := newRevalidator(t, root, fixedDuration(), nil)
	plan, err := r.PlanCandidates(context.Background(), []Candidate{
		{ID: 1, AudioPath: audio, Root: root},
		{ID: 2, AudioPath: audio, Root: root},
	})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Errorf("planned %d moves for one sidecar; every move past the first fails and is logged as a real fault", len(plan.Moves))
	}
	// Both rows still retire.
	if len(plan.Findings) != 2 {
		t.Fatalf("want 2 findings, got %d", len(plan.Findings))
	}
	for _, f := range plan.Findings {
		if f.ID == 0 {
			t.Error("a finding carries no ID; its row could never be stamped")
		}
	}
}

// TestPlanCandidatesJudgesAgainstTheOwnerNotTheRow is the arbitration's OWN
// test, and it is deliberately built so the claim map cannot satisfy it: there
// is exactly ONE candidate, so nothing is ever claimed by an earlier row.
//
// This is the shape the previous revision got wrong in both directions. work_queue
// is unique on (artist_key, title_key), NOT on source_path, so two same-stem
// copies of one track collapse to ONE row -- and that row may well name the copy
// the resolver did not pick. Judging against the ROW's audio destroys a correct
// sidecar (the shorter copy reads categorical on a lyric timed for the longer);
// DISOWNING the row instead means the sidecar is judged by nobody at all, since
// the owner has no row of its own. Judging against the resolved OWNER is the only
// option with neither failure.
func TestPlanCandidatesJudgesAgainstTheOwnerNotTheRow(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"track.flac", "track.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not really audio"), 0o600); err != nil {
			t.Fatalf("write audio: %v", err)
		}
	}
	lrc := filepath.Join(dir, "track.lrc")
	// Last cue 4:50 (290s): correct for the 300s owner, categorical against the
	// 99s copy the row happens to name.
	if err := os.WriteFile(lrc, []byte("[00:10.00]alpha\n[04:50.00]beta\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	durations := map[string]int{
		filepath.Join(dir, "track.flac"): 300,
		filepath.Join(dir, "track.mp3"):  99,
	}
	r, _ := newRevalidator(t, root, func(_ context.Context, p string, _, _ int64) (int, bool, error) {
		return durations[p], durations[p] > 0, nil
	}, nil)

	// ONE row, and it names the NON-owner (companionAudio picks track.flac).
	plan, err := r.PlanCandidates(context.Background(), []Candidate{
		{ID: 1, AudioPath: filepath.Join(dir, "track.mp3"), Root: root},
	})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Errorf("planned %d move(s) against a sidecar correctly timed for its owner; judging against the row's own audio destroys it", len(plan.Moves))
	}
	if len(plan.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(plan.Findings))
	}
	if got := plan.Findings[0].Outcome; got != timing.Ok {
		t.Errorf("outcome = %q, want %q: the verdict must be the owner's", got, timing.Ok)
	}
	if got := plan.Findings[0].Duration; got != 300 {
		t.Errorf("judged against %ds, want the owner's 300s", got)
	}
}

// TestPlanCandidatesJudgesASidecarWhoseOwnerHasNoRow is the other half, and it
// is the regression test for a defect that traded a destroyed file for an
// invisible one: the previous revision DISOWNED a row whose audio was not the
// resolver's pick, stamped it, and retired it. With work_queue unique on
// (artist_key, title_key) the owner frequently has no row of its own, so a
// sidecar holding a different song's words stayed on disk forever while the
// sweep reported convergence. It must reach the same verdict the CLI walk does.
func TestPlanCandidatesJudgesASidecarWhoseOwnerHasNoRow(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"track.flac", "track.mp3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not really audio"), 0o600); err != nil {
			t.Fatalf("write audio: %v", err)
		}
	}
	// Last cue 10:00 against trackSeconds (120): another song's words entirely.
	if err := os.WriteFile(filepath.Join(dir, "track.lrc"), []byte("[00:10.00]alpha\n[10:00.00]beta\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	r, _ := newRevalidator(t, root, fixedDuration(), nil)

	// The only row names track.mp3; companionAudio picks track.flac.
	plan, err := r.PlanCandidates(context.Background(), []Candidate{
		{ID: 42, AudioPath: filepath.Join(dir, "track.mp3"), Root: root},
	})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if plan.Counts.Categorical != 1 {
		t.Errorf("Categorical = %d, want 1: a sidecar whose owner has no row would be judged by nobody, forever", plan.Counts.Categorical)
	}
	if len(plan.Moves) != 1 {
		t.Errorf("planned %d move(s), want 1: the bad sidecar must be remediated", len(plan.Moves))
	}

	// And the sweep must agree with the CLI walk on this exact tree -- that
	// agreement is what the "revalidate previews what the sweep will do" promise
	// in the README rests on.
	walk, werr := r.Plan(context.Background())
	if werr != nil {
		t.Fatalf("Plan: %v", werr)
	}
	if len(walk.Moves) != len(plan.Moves) {
		t.Errorf("walk planned %d move(s) but the sweep planned %d on the same tree; the CLI preview no longer predicts the sweep",
			len(walk.Moves), len(plan.Moves))
	}
}

// TestPlanCandidatesReadsNoDirectoryForTheCommonCases pins the I/O SHAPE of a
// cycle, which is the property that makes an unattended sweep safe on a
// spun-down array (#684/#685).
//
// The ordering inside judgeCandidate is what earns it: the sidecar is stat'ed
// FIRST, so a candidate whose sidecar is already gone -- the ordinary
// moved-by-a-reorg case, and the bulk of a stale backlog -- returns before any
// owner is resolved. Resolving the owner first would pay a full os.ReadDir per
// candidate for exactly the population with nothing to judge, since
// companionAudio falls through to a listing precisely when its stem probes all
// miss, and they all miss when the audio is gone.
func TestPlanCandidatesReadsNoDirectoryForTheCommonCases(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A candidate with a sidecar AND its companion present.
	if err := os.WriteFile(filepath.Join(dir, "present.mp3"), []byte("not really audio"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "present.lrc"), []byte("[00:01.00]alpha\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}

	for _, tc := range []struct {
		name  string
		audio string
	}{
		{"sidecar already gone", filepath.Join(dir, "vanished.mp3")},
		{"sidecar and companion present", filepath.Join(dir, "present.mp3")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newDirListingCache()
			r, _ := newRevalidator(t, root, fixedDuration(), nil)
			var plan Plan
			if err := r.judgeCandidate(context.Background(), Candidate{ID: 1, AudioPath: tc.audio, Root: root}, &plan, cache, map[string]bool{}); err != nil {
				t.Fatalf("judgeCandidate: %v", err)
			}
			if got := cache.Reads(); got != 0 {
				t.Errorf("issued %d directory read(s); a batch of these would wake the library array every cycle", got)
			}
		})
	}
}
