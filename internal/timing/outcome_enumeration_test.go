package timing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This file is the durable guard for the class of defect #673 exposed.
//
// TimingOutcome is consumed by hand-written switches and comparisons across
// five packages, and EVERY ONE of them has a fail-open shape: an unrecognized
// verdict promotes the write (internal/lyrics), skips remediation
// (internal/revalidate), drops out of the operator's report
// (internal/commands), or takes the benign branch (internal/worker,
// internal/scan). That is the correct default for an UNKNOWN value and the
// wrong one for a value someone just added, so adding an outcome without
// visiting each site ships a silent no-op.
//
// Degenerate demonstrated it concretely: before this work it did not exist, and
// the shape it describes classified as Ok, so a file whose every cue shared one
// timestamp was written as a synced .lrc with nothing anywhere objecting.
//
// The guard below cannot verify that each site's HANDLING is correct -- that is
// a judgment call per outcome, made in the code and its comments. What it can do
// is force the question to be asked, by failing when the declared set of
// outcomes drifts from the set this test knows about.

// knownOutcomes is the hand-maintained roster of every TimingOutcome, paired
// with the disposition decided for it. Adding a constant to timing.go without
// adding it here fails the test, which is the point: the failure message is the
// checklist of sites to visit.
//
// Deliberately duplicated rather than derived. A test that reads the same source
// of truth as the code under test proves only that the file parses; the value
// here is that a human had to write the second copy and, in doing so, decide
// what the new outcome means at each consumer.
var knownOutcomes = map[TimingOutcome]string{
	Ok:              "compliant: promote, never remediate, no warn",
	MisSynced:       "words correct, timing drifted: demote to .txt, do not suppress re-enqueue",
	Categorical:     "timed to a different recording: quarantine, write nothing, suppress re-enqueue",
	UnknownDuration: "no verdict possible: fail open everywhere, never remediate",
	Degenerate:      "every cue at one timestamp, not synced at all: demote to .txt like MisSynced (#673)",
}

// TestEveryTimingOutcomeIsAccountedFor fails when timing.go declares an outcome
// this test does not know about, or when the roster names one that no longer
// exists.
//
// The failure message deliberately lists the consumer sites, because "add it to
// the map" is NOT the fix -- visiting those sites is, and the map is only the
// record that it happened.
func TestEveryTimingOutcomeIsAccountedFor(t *testing.T) {
	declared := declaredOutcomesFromSource(t)

	for name, value := range declared {
		if _, ok := knownOutcomes[value]; !ok {
			t.Errorf("TimingOutcome %s (%q) is not accounted for.\n"+
				"Adding it to knownOutcomes is the LAST step, not the fix. Every consumer\n"+
				"below fails OPEN on an unrecognized verdict, so an unvisited site is a\n"+
				"silent no-op rather than a compile error:\n"+
				"  internal/lyrics/timing_guard.go   DecidePromotion -- default PROMOTES the write\n"+
				"  internal/revalidate/revalidate.go classify        -- default counts Errored, never remediates\n"+
				"  internal/commands/revalidate.go   writeRevalidateTail -- filter drops it from the report\n"+
				"  internal/worker/worker.go         stampTimingOutcome  -- decides warn vs silence, and the wording\n"+
				"  internal/scan/enqueuer.go         shouldSuppress      -- decides re-enqueue suppression",
				name, value)
		}
	}

	declaredValues := map[TimingOutcome]bool{}
	for _, v := range declared {
		declaredValues[v] = true
	}
	for value := range knownOutcomes {
		if !declaredValues[value] {
			t.Errorf("knownOutcomes names %q, which timing.go no longer declares; remove the stale entry", value)
		}
	}
}

// declaredOutcomesFromSource parses timing.go for every constant declared with
// the TimingOutcome type, returning identifier -> value.
//
// Source-parsed rather than hand-listed for the same reason as the #748 sentinel
// guard: Go has no runtime enumeration of a package's constants, so a hand-list
// on BOTH sides could drift together and the test would pass while blind.
func declaredOutcomesFromSource(t *testing.T) map[string]TimingOutcome {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not resolve this test file's path")
	}
	path := filepath.Join(filepath.Dir(thisFile), "timing.go")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	out := map[string]TimingOutcome{}
	for _, decl := range file.Decls {
		gen, isGen := decl.(*ast.GenDecl)
		if !isGen || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, isVS := spec.(*ast.ValueSpec)
			if !isVS {
				continue
			}
			// Only constants explicitly typed TimingOutcome. The Tolerance /
			// CategoricalRatio block in the same file is untyped and must not
			// be swept in.
			id, isIdent := vs.Type.(*ast.Ident)
			if !isIdent || id.Name != "TimingOutcome" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, isLit := vs.Values[i].(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					continue
				}
				out[name.Name] = TimingOutcome(strings.Trim(lit.Value, `"`))
			}
		}
	}

	// Self-check: a scan that silently finds nothing would make every assertion
	// above vacuously true, which is the exact failure this file exists to
	// prevent. The threshold catches a broken scan, not a shrinking set.
	if len(out) < 4 {
		t.Fatalf("found only %d TimingOutcome constants (%v); the AST scan is broken, so this guard is not testing anything", len(out), out)
	}
	return out
}
