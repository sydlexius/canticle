package realign

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/models"
)

// fakeLibs is a minimal LibraryLister for the owner-resolution paths.
type fakeLibs struct{ libs []models.Library }

func (f fakeLibs) List(context.Context) ([]models.Library, error) { return f.libs, nil }

// tempRoot returns a temp dir with symlinks resolved (macOS /var -> /private/var),
// so test paths match the resolver's EvalSymlinks-canonicalized library root.
func tempRoot(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	return resolved
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func defaultCfg() config.RealignConfig {
	return config.RealignConfig{
		IdentityKeys:  []string{"mbid", "isrc"},
		MinConfidence: 0.75,
	}
}

// newRealigner builds a Realigner over one library rooted at root, with an
// injected provenance reader mapping audio path -> isrc.
func newRealigner(root string, cfg config.RealignConfig, isrcByPath map[string]string) (*Realigner, models.Library) {
	lib := models.Library{ID: 1, Path: root, Name: "lib"}
	r := New(fakeLibs{libs: []models.Library{lib}}, cfg)
	r.readProv = func(path string) (isrc, mbid, artist, title string, err error) {
		return isrcByPath[path], "", "", "", nil
	}
	return r, lib
}

// TestPlanLibrary_ExactMatchViaProvenance: an orphan .lrc carrying an [isrc:] that
// matches an audio file's embedded ISRC is planned as an exact-tier move.
func TestPlanLibrary_ExactMatchViaProvenance(t *testing.T) {
	root := tempRoot(t)
	audio := filepath.Join(root, "Artist", "Album", "01. new-name.flac")
	orphan := filepath.Join(root, "Artist", "Album", "01. old-name.lrc")
	write(t, audio, "audio")
	write(t, orphan, "[isrc:USABC1234567]\n[00:01.00]hi\n")

	r, lib := newRealigner(root, defaultCfg(), map[string]string{audio: "USABC1234567"})
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 1 || res.Moves[0].Method != "exact" || !res.Moves[0].Eligible {
		t.Fatalf("moves = %+v; want 1 eligible exact move", res.Moves)
	}
	if got := res.Moves[0].Target; filepath.Base(got) != "01. new-name.lrc" {
		t.Errorf("target = %q; want the audio's stem + .lrc", got)
	}
}

// TestReactiveDir_AppliesExactSkipsHeuristicByDefault: reactive apply moves an
// exact match but leaves a heuristic match in place when AutoApplyHeuristic is off.
func TestReactiveDir_AppliesExactSkipsHeuristicByDefault(t *testing.T) {
	root := tempRoot(t)
	// Exact pair: orphan carries an isrc matching the audio.
	exAudio := filepath.Join(root, "A", "01. exact.flac")
	exOrphan := filepath.Join(root, "A", "01. exact-old.lrc")
	write(t, exAudio, "x")
	write(t, exOrphan, "[isrc:USEX00000001]\n[00:01.00]x\n")
	// Heuristic pair: single orphan + single sidecar-less audio, names match, no isrc.
	heAudio := filepath.Join(root, "B", "Song Title.flac")
	heOrphan := filepath.Join(root, "B", "Song Titl.lrc")
	write(t, heAudio, "y")
	write(t, heOrphan, "[ti:Song Title]\n[00:01.00]y\n")

	cfg := defaultCfg() // AutoApplyHeuristic defaults false
	r, lib := newRealigner(root, cfg, map[string]string{exAudio: "USEX00000001"})
	backup := filepath.Join(t.TempDir(), "b.jsonl")

	if _, _, err := r.ReactiveDir(lib, filepath.Join(root, "A"), backup); err != nil {
		t.Fatalf("ReactiveDir A: %v", err)
	}
	if _, _, err := r.ReactiveDir(lib, filepath.Join(root, "B"), backup); err != nil {
		t.Fatalf("ReactiveDir B: %v", err)
	}
	// Exact move applied: orphan gone, target present.
	if _, err := os.Stat(exOrphan); !os.IsNotExist(err) {
		t.Errorf("exact orphan should have moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "A", "01. exact.lrc")); err != nil {
		t.Errorf("exact target missing: %v", err)
	}
	// Heuristic move NOT applied (auto_apply_heuristic false): orphan still there.
	if _, err := os.Stat(heOrphan); err != nil {
		t.Errorf("heuristic orphan should remain (auto_apply_heuristic off): %v", err)
	}
}

// TestReactiveDir_AppliesHeuristicWhenEnabled: with AutoApplyHeuristic set, a
// heuristic match is auto-applied.
func TestReactiveDir_AppliesHeuristicWhenEnabled(t *testing.T) {
	root := tempRoot(t)
	audio := filepath.Join(root, "B", "Song Title.flac")
	orphan := filepath.Join(root, "B", "Song Titl.lrc")
	write(t, audio, "y")
	write(t, orphan, "[ti:Song Title]\n[00:01.00]y\n")

	cfg := defaultCfg()
	cfg.AutoApplyHeuristic = true
	r, lib := newRealigner(root, cfg, nil)
	backup := filepath.Join(t.TempDir(), "b.jsonl")

	if _, _, err := r.ReactiveDir(lib, filepath.Join(root, "B"), backup); err != nil {
		t.Fatalf("ReactiveDir: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("heuristic orphan should have moved with auto_apply_heuristic on: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "B", "Song Title.lrc")); err != nil {
		t.Errorf("heuristic target missing: %v", err)
	}
}

// TestCrossDirectory_RelocatesStrandedSidecar: the strand-on-move case -- audio
// moved to a different directory, its sidecar orphaned in the old one. With
// cross_directory on, an exact provenance match relocates the sidecar across dirs.
func TestCrossDirectory_RelocatesStrandedSidecar(t *testing.T) {
	root := tempRoot(t)
	newAudio := filepath.Join(root, "NewArtist", "Album", "01. track.flac")
	strandedOrphan := filepath.Join(root, "OldArtist", "Album", "01. track.lrc")
	write(t, newAudio, "x")
	write(t, strandedOrphan, "[isrc:USMOVED00001]\n[00:01.00]x\n")

	cfg := defaultCfg()
	cfg.CrossDirectory = true
	r, lib := newRealigner(root, cfg, map[string]string{newAudio: "USMOVED00001"})
	backup := filepath.Join(t.TempDir(), "b.jsonl")

	// Realign the OLD directory (where the sidecar stranded).
	res, applied, err := r.ReactiveDir(lib, filepath.Join(root, "OldArtist", "Album"), backup)
	if err != nil {
		t.Fatalf("ReactiveDir: %v", err)
	}
	moved, _, _ := CountApplied(applied)
	if len(res.Moves) != 1 || moved != 1 {
		t.Fatalf("moves=%d applied-moved=%d; want 1/1 (cross-dir relocation)", len(res.Moves), moved)
	}
	if _, err := os.Stat(filepath.Join(root, "NewArtist", "Album", "01. track.lrc")); err != nil {
		t.Errorf("sidecar not relocated next to moved audio: %v", err)
	}
}

// TestResolveAndRealignDir_OwnedDirRealigns: a directory under a configured
// library resolves to that library and applies an exact-tier move (the webhook's
// happy path).
func TestResolveAndRealignDir_OwnedDirRealigns(t *testing.T) {
	root := tempRoot(t)
	audio := filepath.Join(root, "Artist", "01. new.flac")
	orphan := filepath.Join(root, "Artist", "01. old.lrc")
	write(t, audio, "x")
	write(t, orphan, "[isrc:USOWN00000001]\n[00:01.00]x\n")
	r, _ := newRealigner(root, defaultCfg(), map[string]string{audio: "USOWN00000001"})
	backup := filepath.Join(t.TempDir(), "b.jsonl")

	res, applied, err := r.ResolveAndRealignDir(context.Background(), filepath.Join(root, "Artist"), backup)
	if err != nil {
		t.Fatalf("ResolveAndRealignDir: %v", err)
	}
	moved, _, _ := CountApplied(applied)
	if len(res.Moves) != 1 || moved != 1 {
		t.Fatalf("moves=%d applied-moved=%d; want 1/1", len(res.Moves), moved)
	}
}

// TestReactiveDir_NoOrphansIsNoop: a directory whose sidecars all pair with audio
// yields no moves and a nil apply slice (the common steady-state case).
func TestReactiveDir_NoOrphansIsNoop(t *testing.T) {
	root := tempRoot(t)
	write(t, filepath.Join(root, "Artist", "01. song.flac"), "x")
	write(t, filepath.Join(root, "Artist", "01. song.lrc"), "[00:01.00]x\n") // stem matches -> not orphaned
	r, lib := newRealigner(root, defaultCfg(), nil)
	res, applied, err := r.ReactiveDir(lib, filepath.Join(root, "Artist"), filepath.Join(t.TempDir(), "b.jsonl"))
	if err != nil {
		t.Fatalf("ReactiveDir: %v", err)
	}
	if len(res.Moves) != 0 || len(applied) != 0 {
		t.Errorf("steady-state dir should be a no-op; moves=%d applied=%d", len(res.Moves), len(applied))
	}
}

// TestResolveAndRealignDir_UnownedDirIsNoop: a directory under no configured
// library is a no-op (the webhook's confined-but-unlisted case).
func TestResolveAndRealignDir_UnownedDirIsNoop(t *testing.T) {
	root := tempRoot(t)
	r, _ := newRealigner(root, defaultCfg(), nil)
	res, applied, err := r.ResolveAndRealignDir(context.Background(), filepath.Join(t.TempDir(), "elsewhere"), "")
	if err != nil {
		t.Fatalf("ResolveAndRealignDir: %v", err)
	}
	if len(res.Moves) != 0 || len(applied) != 0 {
		t.Errorf("unowned dir should be a no-op; got moves=%d applied=%d", len(res.Moves), len(applied))
	}
}

// TestClassify_AmbiguousMultipleOrphans: two orphans with no provenance in a
// directory cannot be paired positionally, so both are reported ambiguous.
func TestClassify_AmbiguousMultipleOrphans(t *testing.T) {
	root := tempRoot(t)
	write(t, filepath.Join(root, "D", "a.lrc"), "[00:01.00]a\n")
	write(t, filepath.Join(root, "D", "b.lrc"), "[00:01.00]b\n")
	r, lib := newRealigner(root, defaultCfg(), nil)
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 0 || len(res.Skips) != 2 {
		t.Fatalf("moves=%d skips=%d; want 0 moves, 2 ambiguous skips", len(res.Moves), len(res.Skips))
	}
	for _, s := range res.Skips {
		if s.Kind != "ambiguous" {
			t.Errorf("skip kind = %q; want ambiguous", s.Kind)
		}
	}
}

// TestClassify_ConflictSharedISRC: an orphan whose ISRC matches two audio files
// is a conflict (never guessed).
func TestClassify_ConflictSharedISRC(t *testing.T) {
	root := tempRoot(t)
	a1 := filepath.Join(root, "D", "a1.flac")
	a2 := filepath.Join(root, "D", "a2.flac")
	orphan := filepath.Join(root, "D", "orphan.lrc")
	write(t, a1, "x")
	write(t, a2, "y")
	write(t, orphan, "[isrc:USDUP00000001]\n[00:01.00]x\n")
	r, lib := newRealigner(root, defaultCfg(), map[string]string{a1: "USDUP00000001", a2: "USDUP00000001"})
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 0 || len(res.Skips) != 1 || res.Skips[0].Kind != "conflict" {
		t.Fatalf("moves=%d skips=%+v; want 0 moves, 1 conflict", len(res.Moves), res.Skips)
	}
}

// TestClassify_ConflictDestinationExists: an exact match whose target sidecar
// already exists is a conflict, never a clobber.
func TestClassify_ConflictDestinationExists(t *testing.T) {
	root := tempRoot(t)
	audio := filepath.Join(root, "D", "new.flac")
	orphan := filepath.Join(root, "D", "old.lrc")
	existing := filepath.Join(root, "D", "new.lrc") // target already occupied
	write(t, audio, "x")
	write(t, orphan, "[isrc:USEXIST00001]\n[00:01.00]x\n")
	write(t, existing, "[00:01.00]existing\n")
	r, lib := newRealigner(root, defaultCfg(), map[string]string{audio: "USEXIST00001"})
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 0 || len(res.Skips) != 1 || res.Skips[0].Kind != "conflict" {
		t.Fatalf("moves=%d skips=%+v; want 0 moves, 1 conflict (destination exists)", len(res.Moves), res.Skips)
	}
}

// TestClassify_ExactHeuristicDisagree: when the lone in-directory pair (heuristic)
// and the exact provenance match point at different audio, it is a conflict.
func TestClassify_ExactHeuristicDisagree(t *testing.T) {
	root := tempRoot(t)
	orphan := filepath.Join(root, "D", "orphan.lrc")
	audioA := filepath.Join(root, "D", "audioA.flac") // already has a sidecar
	audioASide := filepath.Join(root, "D", "audioA.lrc")
	audioB := filepath.Join(root, "D", "audioB.flac") // the lone sidecar-less audio
	write(t, orphan, "[isrc:USDIS00000001]\n[00:01.00]x\n")
	write(t, audioA, "a")
	write(t, audioASide, "[00:01.00]a\n")
	write(t, audioB, "b")
	// The orphan's ISRC matches audioA (which is not the positional heuristic pick,
	// audioB), so exact and heuristic disagree.
	r, lib := newRealigner(root, defaultCfg(), map[string]string{audioA: "USDIS00000001"})
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 0 || len(res.Skips) != 1 || res.Skips[0].Kind != "conflict" {
		t.Fatalf("moves=%d skips=%+v; want 0 moves, 1 conflict (exact/heuristic disagree)", len(res.Moves), res.Skips)
	}
}

// TestPlanDir_OutsideRootErrors: a directory outside the library root is rejected.
func TestPlanDir_OutsideRootErrors(t *testing.T) {
	root := tempRoot(t)
	write(t, filepath.Join(root, "Artist", "01. song.flac"), "x")
	r, lib := newRealigner(root, defaultCfg(), nil)
	if _, err := r.PlanDir(lib, tempRoot(t)); err == nil {
		t.Fatal("PlanDir on a directory outside the library root = nil error; want failure")
	}
}

// TestApply_BackupOpenFailure: a backup path whose parent directory does not exist
// makes Apply fail before renaming, leaving the orphan in place.
func TestApply_BackupOpenFailure(t *testing.T) {
	root := tempRoot(t)
	audio := filepath.Join(root, "D", "new.flac")
	orphan := filepath.Join(root, "D", "old.lrc")
	write(t, audio, "x")
	write(t, orphan, "[isrc:USBK00000001]\n[00:01.00]x\n")
	r, lib := newRealigner(root, defaultCfg(), map[string]string{audio: "USBK00000001"})
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	badBackup := filepath.Join(root, "no", "such", "dir", "b.jsonl")
	if _, aerr := r.Apply(res.Moves, badBackup, Policy{AllowHeuristic: true}); aerr == nil {
		t.Fatal("Apply with an unopenable backup path = nil error; want failure")
	}
	if _, serr := os.Stat(orphan); serr != nil {
		t.Errorf("orphan should remain after a failed-backup apply: %v", serr)
	}
}

// TestApply_RequireProvenanceGatesHeuristic: with require_provenance, a heuristic
// move is reported ineligible and never applied even by the CLI policy.
func TestApply_RequireProvenanceGatesHeuristic(t *testing.T) {
	root := tempRoot(t)
	audio := filepath.Join(root, "B", "Song Title.flac")
	orphan := filepath.Join(root, "B", "Song Titl.lrc")
	write(t, audio, "y")
	write(t, orphan, "[ti:Song Title]\n[00:01.00]y\n")

	cfg := defaultCfg()
	cfg.RequireProvenance = true
	r, lib := newRealigner(root, cfg, nil)
	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 1 || res.Moves[0].Eligible {
		t.Fatalf("moves = %+v; want 1 ineligible heuristic move", res.Moves)
	}
	applied, err := r.Apply(res.Moves, filepath.Join(t.TempDir(), "b.jsonl"), Policy{AllowHeuristic: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	moved, skipped, _ := CountApplied(applied)
	if moved != 0 || skipped != 1 {
		t.Fatalf("moved=%d skipped=%d; want 0/1 (require_provenance gates the heuristic move)", moved, skipped)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("gated orphan should remain in place: %v", err)
	}
}

// TestApply_RefusesClobberAcrossMergedPlans: two eligible moves that target the
// same destination -- as can happen when per-library plans (each with its own
// claimed map) are concatenated before a single Apply -- must not clobber. Apply
// re-checks the destination just before rename, applies the first, and refuses
// the second with an error, leaving the second orphan and the first move's
// content untouched.
func TestApply_RefusesClobberAcrossMergedPlans(t *testing.T) {
	root := tempRoot(t)
	target := filepath.Join(root, "D", "Song.lrc")
	orphan1 := filepath.Join(root, "A", "first.lrc")
	orphan2 := filepath.Join(root, "B", "second.lrc")
	write(t, orphan1, "FIRST")
	write(t, orphan2, "SECOND")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}

	r, _ := newRealigner(root, defaultCfg(), nil)
	moves := []Move{
		{Orphan: orphan1, Target: target, Method: "exact", LibraryID: 1, Eligible: true},
		{Orphan: orphan2, Target: target, Method: "exact", LibraryID: 2, Eligible: true},
	}
	applied, err := r.Apply(moves, filepath.Join(t.TempDir(), "b.jsonl"), Policy{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied len = %d; want 2", len(applied))
	}
	if applied[0].Err != nil {
		t.Errorf("first move should apply cleanly, got err: %v", applied[0].Err)
	}
	if applied[1].Err == nil {
		t.Error("second move onto an existing destination should be refused with an error")
	}
	if _, serr := os.Stat(orphan2); serr != nil {
		t.Errorf("refused second orphan should remain in place: %v", serr)
	}
	got, rerr := os.ReadFile(target) //nolint:gosec // G304: test-controlled path under a temp root
	if rerr != nil {
		t.Fatalf("read target: %v", rerr)
	}
	if string(got) != "FIRST" {
		t.Errorf("target content = %q; want %q (second move must not clobber the first)", got, "FIRST")
	}
}
