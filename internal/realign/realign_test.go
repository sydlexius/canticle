package realign

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/lyrics"
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

// audioProv is a tiny per-path provenance fixture for the N:M name-match tests
// below, letting each test wire artist/title (not just isrc) per audio path.
type audioProv struct {
	artist, title string
}

// withAudioProv overrides r's provenance reader to return artist/title from
// the given map (isrc/mbid left empty), for tests that need name signals
// rather than exact-tier identity.
func withAudioProv(r *Realigner, prov map[string]audioProv) {
	r.readProv = func(path string) (isrc, mbid, artist, title string, err error) {
		p := prov[path]
		return "", "", p.artist, p.title, nil
	}
}

// TestClassify_NameMatch_MultiOrphanMultiCandidate: a directory with two
// orphaned sidecars and two sidecar-less audio files, each pair unambiguously
// closest by artist/title, resolves via the N:M matcher when name_match is on.
func TestClassify_NameMatch_MultiOrphanMultiCandidate(t *testing.T) {
	root := tempRoot(t)
	audioA := filepath.Join(root, "D", "01-renamed-a.flac")
	audioB := filepath.Join(root, "D", "02-renamed-b.flac")
	orphanA := filepath.Join(root, "D", "old-a.lrc")
	orphanB := filepath.Join(root, "D", "old-b.lrc")
	write(t, audioA, "a")
	write(t, audioB, "b")
	write(t, orphanA, "[ar:Aardvark Band]\n[ti:Alpha Song]\n[00:01.00]x\n")
	write(t, orphanB, "[ar:Bumblebee Band]\n[ti:Beta Song]\n[00:01.00]x\n")

	cfg := defaultCfg()
	cfg.NameMatch = true
	cfg.MinMargin = 0.05
	r, lib := newRealigner(root, cfg, nil)
	withAudioProv(r, map[string]audioProv{
		audioA: {artist: "Aardvark Band", title: "Alpha Song"},
		audioB: {artist: "Bumblebee Band", title: "Beta Song"},
	})

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Skips) != 0 {
		t.Fatalf("skips = %+v; want none", res.Skips)
	}
	if len(res.Moves) != 2 {
		t.Fatalf("moves = %+v; want 2", res.Moves)
	}
	gotTargets := map[string]string{}
	for _, mv := range res.Moves {
		if mv.Method != "heuristic-nm" {
			t.Errorf("move method = %q; want heuristic-nm", mv.Method)
		}
		if !mv.Eligible {
			t.Errorf("move for %s not eligible: %s", mv.Orphan, mv.GateReason)
		}
		gotTargets[mv.Orphan] = mv.Target
	}
	if got := gotTargets[orphanA]; filepath.Base(got) != "01-renamed-a.lrc" {
		t.Errorf("orphanA target = %q; want it paired with audioA", got)
	}
	if got := gotTargets[orphanB]; filepath.Base(got) != "02-renamed-b.lrc" {
		t.Errorf("orphanB target = %q; want it paired with audioB", got)
	}
}

// TestClassify_NameMatch_NearTieIsAmbiguous: two candidates near-tied for one
// orphan must be reported ambiguous, never guessed, even though a different
// orphan resolves cleanly.
func TestClassify_NameMatch_NearTieIsAmbiguous(t *testing.T) {
	root := tempRoot(t)
	audioTie1 := filepath.Join(root, "D", "tie1.flac")
	audioTie2 := filepath.Join(root, "D", "tie2.flac")
	audioClear := filepath.Join(root, "D", "clear.flac")
	orphanTie := filepath.Join(root, "D", "orphan-tie.lrc")
	orphanClear := filepath.Join(root, "D", "orphan-clear.lrc")
	write(t, audioTie1, "a")
	write(t, audioTie2, "b")
	write(t, audioClear, "c")
	write(t, orphanTie, "[ar:The Artist]\n[ti:The Song]\n[00:01.00]x\n")
	write(t, orphanClear, "[ar:Zephyr Ensemble]\n[ti:Zeta Tune]\n[00:01.00]x\n")

	cfg := defaultCfg()
	cfg.NameMatch = true
	cfg.MinMargin = 0.05
	r, lib := newRealigner(root, cfg, nil)
	withAudioProv(r, map[string]audioProv{
		// Two near-identical candidates for orphanTie: same score, no margin.
		audioTie1:  {artist: "The Artist", title: "The Song"},
		audioTie2:  {artist: "The Artist", title: "The Song"},
		audioClear: {artist: "Zephyr Ensemble", title: "Zeta Tune"},
	})

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	var moved, skippedTie bool
	for _, mv := range res.Moves {
		if mv.Orphan == orphanClear {
			moved = true
		}
		if mv.Orphan == orphanTie {
			t.Errorf("orphanTie must not resolve to a Move when tied: %+v", mv)
		}
	}
	if !moved {
		t.Errorf("orphanClear should resolve despite the unrelated tie: moves=%+v", res.Moves)
	}
	for _, s := range res.Skips {
		if s.Path == orphanTie {
			skippedTie = true
			if s.Kind != "ambiguous" {
				t.Errorf("orphanTie skip kind = %q; want ambiguous", s.Kind)
			}
		}
	}
	if !skippedTie {
		t.Errorf("orphanTie should be reported ambiguous: skips=%+v", res.Skips)
	}
}

// TestClassify_NameMatch_ContestedTargetResolvesOneSkipsOther: two orphans
// both best-matching the same candidate resolve to exactly one Move (the
// stronger pairing); the other is reported ambiguous, never clobbered.
func TestClassify_NameMatch_ContestedTargetResolvesOneSkipsOther(t *testing.T) {
	root := tempRoot(t)
	audioShared := filepath.Join(root, "D", "shared.flac")
	audioOther := filepath.Join(root, "D", "other.flac")
	orphanStrong := filepath.Join(root, "D", "orphan-strong.lrc")
	orphanWeak := filepath.Join(root, "D", "orphan-weak.lrc")
	write(t, audioShared, "a")
	write(t, audioOther, "b")
	// orphanStrong is an exact textual match for audioShared's name.
	write(t, orphanStrong, "[ar:Exact Match Band]\n[ti:Exact Match Song]\n[00:01.00]x\n")
	// orphanWeak is a rough, partial match for audioShared's name and a much
	// worse match for audioOther, so it, too, prefers audioShared -- contested.
	write(t, orphanWeak, "[ar:Exact Match Ban]\n[ti:Exact Match Son]\n[00:01.00]x\n")

	cfg := defaultCfg()
	cfg.NameMatch = true
	cfg.MinMargin = 0.05
	r, lib := newRealigner(root, cfg, nil)
	withAudioProv(r, map[string]audioProv{
		audioShared: {artist: "Exact Match Band", title: "Exact Match Song"},
		audioOther:  {artist: "Zzznope Totally Unrelated", title: "Qqqnothing Alike"},
	})

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	for _, mv := range res.Moves {
		if mv.Target != filepath.Join(root, "D", "shared.lrc") && mv.Target != filepath.Join(root, "D", "other.lrc") {
			t.Errorf("unexpected target %q", mv.Target)
		}
	}
	if len(res.Moves) != 1 {
		t.Fatalf("moves = %+v; want exactly 1 (contested target resolves once)", res.Moves)
	}
	if res.Moves[0].Orphan != orphanStrong {
		t.Errorf("resolved orphan = %q; want the stronger match %q", res.Moves[0].Orphan, orphanStrong)
	}
	var sawWeakSkip bool
	for _, s := range res.Skips {
		if s.Path == orphanWeak {
			sawWeakSkip = true
		}
	}
	if !sawWeakSkip {
		t.Errorf("orphanWeak should be reported (ambiguous or conflict), not silently dropped: skips=%+v", res.Skips)
	}
}

// TestClassify_NameMatch_OffByDefaultStaysAmbiguous: with name_match disabled
// (the default), a directory with multiple orphans and multiple candidates
// stays generically ambiguous -- the N:M tier is strictly opt-in.
func TestClassify_NameMatch_OffByDefaultStaysAmbiguous(t *testing.T) {
	root := tempRoot(t)
	audioA := filepath.Join(root, "D", "01-renamed-a.flac")
	audioB := filepath.Join(root, "D", "02-renamed-b.flac")
	orphanA := filepath.Join(root, "D", "old-a.lrc")
	orphanB := filepath.Join(root, "D", "old-b.lrc")
	write(t, audioA, "a")
	write(t, audioB, "b")
	write(t, orphanA, "[ar:Aardvark Band]\n[ti:Alpha Song]\n[00:01.00]x\n")
	write(t, orphanB, "[ar:Bumblebee Band]\n[ti:Beta Song]\n[00:01.00]x\n")

	cfg := defaultCfg() // NameMatch defaults false
	r, lib := newRealigner(root, cfg, nil)
	withAudioProv(r, map[string]audioProv{
		audioA: {artist: "Aardvark Band", title: "Alpha Song"},
		audioB: {artist: "Bumblebee Band", title: "Beta Song"},
	})

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 0 {
		t.Fatalf("moves = %+v; want none (name_match off)", res.Moves)
	}
	if len(res.Skips) != 2 {
		t.Fatalf("skips = %+v; want 2 ambiguous", res.Skips)
	}
	for _, s := range res.Skips {
		if s.Kind != "ambiguous" {
			t.Errorf("skip kind = %q; want ambiguous", s.Kind)
		}
	}
}

// TestApply_NameMatchMove_GatedByAllowHeuristic: a heuristic-nm Move is gated
// the same way a heuristic Move is -- suppressed under
// Policy{AllowHeuristic: false}, applied under Policy{AllowHeuristic: true}.
func TestApply_NameMatchMove_GatedByAllowHeuristic(t *testing.T) {
	root := tempRoot(t)
	orphan := filepath.Join(root, "D", "orphan.lrc")
	target := filepath.Join(root, "D", "target.lrc")
	write(t, orphan, "content")

	r, _ := newRealigner(root, defaultCfg(), nil)
	mv := Move{Orphan: orphan, Target: target, Method: "heuristic-nm", LibraryID: 1, Eligible: true, Confidence: 0.9}

	applied, err := r.Apply([]Move{mv}, filepath.Join(t.TempDir(), "b1.jsonl"), Policy{AllowHeuristic: false})
	if err != nil {
		t.Fatalf("Apply (gated): %v", err)
	}
	if len(applied) != 1 || !applied[0].GatedSkipped {
		t.Fatalf("applied = %+v; want 1 GatedSkipped move under AllowHeuristic=false", applied)
	}
	if _, serr := os.Stat(orphan); serr != nil {
		t.Errorf("orphan should remain when gated: %v", serr)
	}

	applied, err = r.Apply([]Move{mv}, filepath.Join(t.TempDir(), "b2.jsonl"), Policy{AllowHeuristic: true})
	if err != nil {
		t.Fatalf("Apply (allowed): %v", err)
	}
	if len(applied) != 1 || applied[0].GatedSkipped || applied[0].Err != nil {
		t.Fatalf("applied = %+v; want 1 clean move under AllowHeuristic=true", applied)
	}
	if _, serr := os.Stat(target); serr != nil {
		t.Errorf("target should exist after applied move: %v", serr)
	}
}

// TestClassify_NameMatch_ClaimedCandidateHidesAmbiguity is a COUNTEREXAMPLE to
// the "recompute the runner-up against only unclaimed candidates" margin
// semantics in resolveNameMatch.
//
// Matrix (scores from normalize.MatchConfidence, min_confidence 0.75,
// min_margin 0.05):
//
//	                 C1 "Nova Drifter"   C2 "Nova Drifting"
//	O1 "Nova Drifter"      1.0000              0.9205
//	O2 "Nova Drift"        0.9667              0.9538
//
// Descending processing order is O1-C1 (1.0000), O2-C1 (0.9667),
// O2-C2 (0.9538), O1-C2 (0.9205).
//
//   - O1-C1 is accepted: runner-up 0.9205, margin 0.0795 >= 0.05. C1 is claimed.
//   - O2-C1 -- which is O2's OWN best pairing -- is silently `continue`d because
//     C1 is claimed. No ambiguity is recorded.
//   - O2-C2 is then evaluated. Its only rival, C1, is claimed and therefore
//     excluded from the runner-up scan, so runnerUp stays -1 and the margin
//     check is skipped entirely. O2 is confidently paired to C2.
//
// But O2 cannot actually tell C1 from C2: 0.9667 vs 0.9538 is a margin of
// 0.0129, far below min_margin. Against the full original matrix O2 is
// unambiguously ambiguous and MUST be skipped. The claimed-only runner-up
// recomputation converts a genuine near-tie into a confident pairing -- exactly
// the silent misattachment the margin rule exists to prevent. The greedy
// accept of O1 did not displace a stronger match, but it did DESTROY the
// evidence that O2's decision was a coin flip.
func TestClassify_NameMatch_ClaimedCandidateHidesAmbiguity(t *testing.T) {
	root := tempRoot(t)
	c1 := filepath.Join(root, "D", "c1.flac")
	c2 := filepath.Join(root, "D", "c2.flac")
	o1 := filepath.Join(root, "D", "o1.lrc")
	o2 := filepath.Join(root, "D", "o2.lrc")
	write(t, c1, "a")
	write(t, c2, "b")
	write(t, o1, "[ar:Nova]\n[ti:Drifter]\n[00:01.00]x\n")
	write(t, o2, "[ar:Nova]\n[ti:Drift]\n[00:01.00]x\n")

	cfg := defaultCfg()
	cfg.NameMatch = true
	cfg.MinMargin = 0.05
	r, lib := newRealigner(root, cfg, nil)
	withAudioProv(r, map[string]audioProv{
		c1: {artist: "Nova", title: "Drifter"},
		c2: {artist: "Nova", title: "Drifting"},
	})

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	for _, mv := range res.Moves {
		if mv.Orphan == o2 {
			t.Errorf("o2 was paired to %s (confidence %.4f), but its top two candidates "+
				"score 0.9667 and 0.9538 -- a margin of 0.0129, below min_margin 0.05. "+
				"It must be reported ambiguous, not guessed.", mv.Target, mv.Confidence)
		}
	}
	var o2Skipped bool
	for _, s := range res.Skips {
		if s.Path == o2 {
			o2Skipped = true
			if s.Kind != "ambiguous" {
				t.Errorf("o2 skip kind = %q; want ambiguous", s.Kind)
			}
		}
	}
	if !o2Skipped {
		t.Errorf("o2 should be reported ambiguous: moves=%+v skips=%+v", res.Moves, res.Skips)
	}
}

// TestResolveNameMatch_NeverPairsTwoOrphansToOneCandidate pins the claimed-candidate
// guard INSIDE resolveNameMatch directly. The classify-level contested test only
// goes red when BOTH that guard and classifyDir's outer `claimed` map are removed,
// so on its own it cannot tell a working inner guard from a broken one backstopped
// by the outer map. This asserts the double-move property at the source: no two
// accepted pairings may name the same audio candidate.
func TestResolveNameMatch_NeverPairsTwoOrphansToOneCandidate(t *testing.T) {
	orphans := []string{"o1.lrc", "o2.lrc"}
	tags := map[string]lyrics.ProvenanceTags{
		"o1.lrc": {Artist: "Nova", Title: "Drifter"},
		"o2.lrc": {Artist: "Nova", Title: "Drifter"}, // identical: both want the same candidate
	}
	candidates := []string{"c1.flac", "c2.flac"}
	getProv := func(p string) audioProvenance {
		switch p {
		case "c1.flac":
			return audioProvenance{artist: "Nova", title: "Drifter"}
		default:
			return audioProvenance{artist: "Zzz Unrelated", title: "Qqq Nothing"}
		}
	}

	pairings, unresolved := resolveNameMatch(orphans, tags, candidates, getProv, 0.75, 0.05)
	seen := map[string]string{}
	for _, p := range pairings {
		if prev, dup := seen[p.Audio]; dup {
			t.Fatalf("candidate %q claimed twice: by %q and %q -- a double move would "+
				"rename one sidecar onto the other", p.Audio, prev, p.Orphan)
		}
		seen[p.Audio] = p.Orphan
	}
	if len(pairings)+len(unresolved) != len(orphans) {
		t.Errorf("every orphan must be either paired or reported: pairings=%+v unresolved=%+v", pairings, unresolved)
	}
}

// TestResolveNameMatch_ProcessesInDescendingScoreOrder pins the whole-matrix
// descending sort that the margin-semantics argument depends on. Both orphans
// best-match the same candidate; the STRONGER pairing must win it regardless of
// insertion order. The orphan filenames are chosen so that insertion order
// (orphans sorted alphabetically) presents the WEAKER orphan first -- so this goes
// red if the sort is dropped and pairs are consumed in matrix order.
func TestResolveNameMatch_ProcessesInDescendingScoreOrder(t *testing.T) {
	orphans := []string{"a-weak.lrc", "z-strong.lrc"} // alphabetical == insertion order
	tags := map[string]lyrics.ProvenanceTags{
		"a-weak.lrc":   {Artist: "Exact Match Ban", Title: "Exact Match Son"},
		"z-strong.lrc": {Artist: "Exact Match Band", Title: "Exact Match Song"},
	}
	candidates := []string{"other.flac", "shared.flac"}
	getProv := func(p string) audioProvenance {
		if p == "shared.flac" {
			return audioProvenance{artist: "Exact Match Band", title: "Exact Match Song"}
		}
		return audioProvenance{artist: "Zzznope Totally Unrelated", title: "Qqqnothing Alike"}
	}

	pairings, _ := resolveNameMatch(orphans, tags, candidates, getProv, 0.75, 0.05)
	if len(pairings) != 1 {
		t.Fatalf("pairings = %+v; want exactly 1 (both orphans contest shared.flac)", pairings)
	}
	if pairings[0].Orphan != "z-strong.lrc" || pairings[0].Audio != "shared.flac" {
		t.Errorf("pairing = %+v; want z-strong.lrc -> shared.flac. The weaker orphan won the "+
			"contested candidate, which means pairs were consumed in matrix/insertion order "+
			"rather than descending score order.", pairings[0])
	}
}

// TestClassify_NameMatch_NothingClearsThreshold: an orphan whose every candidate
// scores below min_confidence is reported ambiguous and skipped cleanly -- never
// paired arbitrarily to the least-bad candidate.
func TestClassify_NameMatch_NothingClearsThreshold(t *testing.T) {
	root := tempRoot(t)
	audioA := filepath.Join(root, "D", "aaa.flac")
	audioB := filepath.Join(root, "D", "bbb.flac")
	orphanA := filepath.Join(root, "D", "orphan-1.lrc")
	orphanB := filepath.Join(root, "D", "orphan-2.lrc")
	write(t, audioA, "a")
	write(t, audioB, "b")
	write(t, orphanA, "[ar:Wholly Unrelated]\n[ti:Nothing Alike]\n[00:01.00]x\n")
	write(t, orphanB, "[ar:Also Unrelated]\n[ti:Equally Different]\n[00:01.00]x\n")

	cfg := defaultCfg()
	cfg.NameMatch = true
	cfg.MinMargin = 0.05
	r, lib := newRealigner(root, cfg, nil)
	withAudioProv(r, map[string]audioProv{
		audioA: {artist: "Qqqzzz Xylophone", title: "Vvvwww Kryptic"},
		audioB: {artist: "Mmmnnn Oboe", title: "Pppqqq Glyph"},
	})

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 0 {
		t.Fatalf("moves = %+v; want none (nothing clears min_confidence)", res.Moves)
	}
	if len(res.Skips) != 2 {
		t.Fatalf("skips = %+v; want 2 (both orphans reported)", res.Skips)
	}
	for _, s := range res.Skips {
		if s.Kind != "ambiguous" {
			t.Errorf("skip kind = %q; want ambiguous", s.Kind)
		}
	}
}

// TestResolveNameMatch_EmptyInputs: zero orphans and/or zero candidates must not
// panic and must not invent a pairing.
func TestResolveNameMatch_EmptyInputs(t *testing.T) {
	getProv := func(string) audioProvenance { return audioProvenance{} }
	for _, tc := range []struct {
		name           string
		orphans, cands []string
		wantUnresolved int
	}{
		{"zero orphans, zero candidates", nil, nil, 0},
		{"zero orphans, one candidate", nil, []string{"a.flac"}, 0},
		{"one orphan, zero candidates", []string{"a.lrc"}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairings, unresolved := resolveNameMatch(tc.orphans, nil, tc.cands, getProv, 0.75, 0.05)
			if len(pairings) != 0 {
				t.Errorf("pairings = %+v; want none", pairings)
			}
			if len(unresolved) != tc.wantUnresolved {
				t.Errorf("unresolved = %+v; want %d", unresolved, tc.wantUnresolved)
			}
		})
	}
}
