package realign

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/identity"
	"github.com/sydlexius/canticle/internal/lyrics"
)

// resolveExact hands the shared resolver a LAZY sequence, so when the resolver
// stops pulling (a second match already settles the key as a conflict, and a
// conflict carries no ref) the adapter must honor yield's false return and
// stop reading. Ignoring it would leave the verdict correct while silently
// reading every remaining audio file off disk -- the same class of defect as
// the eager-materialize regression, just narrower.
func TestResolveExact_StopsReadingOnceConflictIsSettled(t *testing.T) {
	const poolSize = 20
	pool := make([]string, poolSize)
	prov := map[string]audioProvenance{}
	for i := range pool {
		p := fmt.Sprintf("/lib/t%02d.flac", i)
		pool[i] = p
		prov[p] = audioProvenance{isrc: "SHARED-ISRC"} // every file conflicts
	}

	reads := 0
	getProv := func(p string) audioProvenance {
		reads++
		return prov[p]
	}

	tags := lyrics.ProvenanceTags{ISRC: "SHARED-ISRC"}
	got, status := resolveExact(tags, identity.NormalizeKeys([]string{"isrc"}), pool, getProv)
	if status != "conflict" || got != "" {
		t.Fatalf("resolveExact = (%q,%q), want (\"\",\"conflict\")", got, status)
	}
	if reads != 2 {
		t.Errorf("getProv called %d times to settle a conflict; want 2 -- the adapter "+
			"must return when yield reports the resolver has stopped pulling", reads)
	}
}

// A candidate whose tag read FAILED must never enter the pool. readProv can
// return a partially-populated provenance alongside its error (a truncated or
// malformed tag block), and admitting that half-read identity would let a file
// the reader could not actually vouch for win the exact tier -- silently
// re-attaching a sidecar to the wrong audio.
func TestResolveExact_PartialReadWithErrorIsNeverAMatch(t *testing.T) {
	good := "/lib/good.flac"
	broken := "/lib/broken.flac"
	prov := map[string]audioProvenance{
		// The broken file's half-parsed tags happen to carry the orphan's ISRC.
		broken: {isrc: "USABC1234567", err: errors.New("truncated tag block")},
		good:   {isrc: "USZZZ9999999"},
	}
	getProv := func(p string) audioProvenance { return prov[p] }

	tags := lyrics.ProvenanceTags{ISRC: "USABC1234567"}
	got, status := resolveExact(tags, identity.NormalizeKeys([]string{"isrc"}), []string{broken, good}, getProv)
	if status != "none" || got != "" {
		t.Fatalf("resolveExact = (%q,%q), want (\"\",\"none\"): an errored read must not "+
			"supply a matchable identity", got, status)
	}
}

// End-to-end twin of the above: an unreadable audio file must not be planned
// as an exact-tier move for an orphan whose ISRC its failed read appeared to
// carry. The orphan is left ambiguous rather than moved onto a file whose tags
// were never successfully read.
func TestPlanLibrary_ErroredProvenanceReadIsNotAnExactMatch(t *testing.T) {
	root := tempRoot(t)
	broken := filepath.Join(root, "Album", "broken.flac")
	other := filepath.Join(root, "Album", "other.flac")
	orphan := filepath.Join(root, "Album", "orphan.lrc")
	write(t, broken, "audio")
	write(t, other, "audio")
	write(t, filepath.Join(root, "Album", "other.lrc"), "x") // other is not an orphan target
	write(t, orphan, "[isrc:USABC1234567]\n[00:01.00]hi\n")

	r, lib := newRealigner(root, defaultCfg(), nil)
	r.readProv = func(path string) (isrc, mbid, artist, title string, err error) {
		if path == broken {
			return "USABC1234567", "", "", "", errors.New("truncated tag block")
		}
		return "", "", "", "", nil
	}

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	for _, mv := range res.Moves {
		if mv.Method == "exact" {
			t.Fatalf("planned an exact-tier move %+v from a file whose tag read failed", mv)
		}
	}
}
