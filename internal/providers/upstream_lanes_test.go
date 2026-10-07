package providers

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestUpstreamLanesMatchAdapters ties UpstreamLanes to the adapters: a lane is
// listed exactly when its package assigns models.Song.Upstream. It is a source
// check (no adapter can be asked), so it is deliberately narrow: an
// "Upstream:" literal key or ".Upstream =" assignment in a non-test file.
func TestUpstreamLanesMatchAdapters(t *testing.T) {
	assigns := regexp.MustCompile(`\bUpstream:\s|\.Upstream\s*=[^=]`)
	var got []string
	for _, lane := range []string{Musixmatch, PetitLyrics, InnerTube} {
		dir := filepath.Join("..", lane)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if assigns.Match(b) {
				got = append(got, lane)
				break
			}
		}
	}
	want := UpstreamLanes()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("lanes whose adapter sets Song.Upstream = %v, UpstreamLanes() = %v; update the list (and the reconcile-upstream backfill follows)", got, want)
	}
}
