package realign

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestRepro_AdapterReadsWholePoolEvenWithNoOrphanIdentity measures how many
// times readProv (a real per-file tag READ on disk in production) is invoked
// while planning ONE orphan that carries NO identity tags at all.
//
// Pre-extraction, resolveExact looped keys OUTERMOST and called getProv lazily
// inside the per-key candidate loop, so an orphan with no [isrc:]/[mbid:]
// header hit `id == ""` for every key and returned "none" having read ZERO
// audio files; only the heuristic tier's single getProv(missingAudio[0]) call
// followed. The new adapter builds the identity.Candidate pool for the ENTIRE
// pool BEFORE calling identity.ResolveExact, so every pool file is read.
//
// The provenance cache bounds this to once per file per plan() run rather than
// per orphan, so it is an amplification, not an unbounded blowup.
func TestRepro_AdapterReadsWholePoolEvenWithNoOrphanIdentity(t *testing.T) {
	root := tempRoot(t)
	const poolSize = 25

	// One directory holding a single orphan sidecar with no provenance header
	// and its (renamed) audio partner: the positional dirPair case.
	dir := filepath.Join(root, "Pair")
	write(t, filepath.Join(dir, "new-name.flac"), "audio")
	write(t, filepath.Join(dir, "old-name.lrc"), "[00:01.00]hi\n")

	// Unrelated audio elsewhere in the library, each already paired with a
	// sidecar so it is never an orphan target -- pure candidate-pool bulk.
	for i := range poolSize {
		other := filepath.Join(root, "Other", fmt.Sprintf("t%02d.flac", i))
		write(t, other, "audio")
		write(t, filepath.Join(root, "Other", fmt.Sprintf("t%02d.lrc", i)), "x")
	}

	cfg := defaultCfg()
	cfg.CrossDirectory = true // library-wide pool, the reactive-realign shape

	r, lib := newRealigner(root, cfg, nil)
	reads := 0
	r.readProv = func(path string) (isrc, mbid, artist, title string, err error) {
		reads++
		return "", "", "", "", nil
	}

	if _, err := r.PlanLibrary(lib); err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}

	t.Logf("readProv invocations for 1 identity-less orphan, pool of %d: %d", poolSize+1, reads)
	// Pre-extraction behavior was 1 (the heuristic tier's single lookup).
	if reads > 1 {
		t.Errorf("readProv called %d times; the pre-extraction resolver called it 1 time "+
			"(lazy, per-key). The adapter now materializes the whole candidate pool up front.", reads)
	}
}
