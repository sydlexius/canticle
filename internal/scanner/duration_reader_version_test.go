package scanner_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/scanner"
)

// requireLine matches the audioduration require line in go.mod, capturing the
// module path and its version separately. It deliberately does NOT pin the
// owner (lizc2003 today, sydlexius after the fork swap): the point is to notice
// the swap, not to presume which side of it we are on.
var audiodurationRequire = regexp.MustCompile(`(?m)^\s*(\S*audioduration)\s+(v\S+)`)

// replaceLine matches a `replace` directive whose TARGET is an audioduration
// module, in either the single-line or the block form.
var replaceLine = regexp.MustCompile(`(?m)^\s*(replace\s+)?\S*audioduration\S*\s+(\S+\s+)?=>`)

// THE ONE FAILURE THIS TEST EXISTS TO CAUSE. scanner.DurationReaderVersion is
// hand-set, and internal/audiodur invalidates its duration cache by comparing
// it. Forgetting to bump it while changing the parser is SILENT and destroys
// user data: stale durations keep reading as cache HITS, timing.Evaluate judges
// synced lyrics against them, and internal/revalidate demotes a correct .lrc to
// .txt or quarantines it outright (#711).
//
// The imminent instance is the swap to github.com/sydlexius/audioduration
// v0.9.0, whose VBR handling moves derived durations by up to 10x. This test
// turns "forgot to bump the constant" from a silent data-loss bug into a red
// build, by requiring the constant to name the module and version go.mod
// actually declares.
//
// WHAT IT DOES NOT CATCH, stated plainly so nobody trusts it further than it
// goes: a canticle-side change to audioDuration's dispatch or to
// audioFileTypeForExt changes the derivation while go.mod sits still, and
// nothing cheap detects that. The doc comment on the constant remains the only
// guard there. This covers the dependency swap, which is the high-risk case,
// not the whole class.
func TestDurationReaderVersionMatchesGoMod(t *testing.T) {
	goMod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	// A `replace` directive silently redirects the module the require line names,
	// so checking only `require` leaves a hole: `go mod edit -replace` can point
	// audioduration at a DIFFERENT parser (an older version, or back to the
	// upstream fork) while this test stays green and the cache keeps serving
	// durations that parser never produced. That is exactly the silent
	// data-destruction path #711 exists to close, so any replace targeting this
	// module fails here rather than being reasoned about.
	if replaceLine.Match(goMod) {
		t.Fatalf("go.mod contains a `replace` directive for audioduration.\n" +
			"A replace redirects the parser without changing the require line this test reads, " +
			"so the reader identity can no longer be trusted to describe the code that actually runs (#711). " +
			"Remove the replace, or extend this test to resolve it and bump DurationReaderVersion to match.")
	}

	m := audiodurationRequire.FindSubmatch(goMod)
	if m == nil {
		t.Fatal("no audioduration require line found in go.mod; if the duration parser was replaced or vendored, update this test AND scanner.DurationReaderVersion together")
	}
	modulePath, version := string(m[1]), string(m[2])

	// The constant must name the version, or a bump was missed.
	if !strings.Contains(scanner.DurationReaderVersion, version) {
		t.Errorf("DurationReaderVersion = %q does not mention go.mod's audioduration version %q.\n"+
			"If you just changed the duration parser, BUMP THE CONSTANT IN THIS SAME COMMIT: a stale value keeps "+
			"serving cached durations the new parser would not produce, and revalidate demotes or quarantines "+
			"correct sidecars on the strength of them (#711).",
			scanner.DurationReaderVersion, version)
	}

	// And it must name the module, so swapping OWNER (lizc2003 -> sydlexius) at
	// an unchanged version number cannot slip through the version check above.
	owner := modulePath
	if i := strings.LastIndex(strings.TrimSuffix(modulePath, "/audioduration"), "/"); i >= 0 {
		owner = strings.TrimSuffix(modulePath, "/audioduration")[i+1:]
	}
	if owner != "" && !strings.Contains(scanner.DurationReaderVersion, owner) {
		t.Errorf("DurationReaderVersion = %q does not mention the audioduration module owner %q (go.mod requires %q).\n"+
			"A fork swap changes the PARSER even when the version string does not, so the constant must move with it (#711).",
			scanner.DurationReaderVersion, owner, modulePath)
	}
}
