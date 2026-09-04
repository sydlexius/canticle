package orchestrator

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
)

// This file is the durable guard for #748.
//
// ClassifyOutcome enumerates provider sentinels by hand and falls to
// `default: OutcomeTransport` for anything it does not recognize. Transport
// outranks a benign miss in precedence(), so an UNENUMERATED miss wins the
// cross-lane ranking on an ordinary double-miss -- the common case for a
// saturated queue, where the fallback lane only ever sees what the primary
// already missed. The worker then records a queue FAILURE against the row:
// attempts++, geometric backoff, marching toward retirement. Ordinary misses
// burn retry budget.
//
// That is invisible from either end. The provider really did return nothing, so
// a miss is the honest outcome, and the row is still retried -- just against a
// budget it should never have spent. Nothing logs a misclassification. It also
// only bites when BOTH lanes miss, so any single-provider test classifies
// correctly and stays green.
//
// It shipped exactly once (petitlyrics, fixed in #607). The guard below makes
// the NEXT provider fail at test time instead of silently on prod.

// providerPackages returns the lyric-provider package directories whose
// exported error sentinels must be accounted for by ClassifyOutcome.
//
// The paths are derived from THIS FILE's location via runtime.Caller rather
// than written relative to the working directory. `go test` happens to run each
// package in its own source directory, so a bare "../musixmatch" works today --
// but that is a property of the harness, not a guarantee: a compiled test binary
// run from elsewhere, or any harness that sets a different cwd, would resolve
// those paths somewhere else. This guard's whole value is failing when a
// sentinel goes unclassified, and a scan that cannot find the source files
// fails for the wrong reason.
//
// Returns a fresh slice per call so no test can mutate what another test reads.
func providerPackages(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not resolve this test file's path; the AST scan cannot locate the provider packages")
	}
	internalDir := filepath.Dir(filepath.Dir(thisFile)) // .../internal
	return []string{
		filepath.Join(internalDir, "musixmatch"),
		filepath.Join(internalDir, "petitlyrics"),
	}
}

// classifiedSentinels maps every exported provider sentinel to its value, so
// the test can classify it rather than merely grep for its name. A name-only
// scan would be wrong in both directions here: musixmatch's misses are
// classified INDIRECTLY through IsBenignMiss(err) and never appear literally in
// ClassifyOutcome (a false positive), while a sentinel could be named in a
// comment without being classified (a false negative). Only calling
// ClassifyOutcome settles it.
//
// This map is deliberately hand-maintained. The AST scan below is the
// authoritative name set; anything it finds that is missing HERE fails the
// test, so a newly added sentinel cannot slip through by being forgotten in
// two places at once.
//
// Built fresh per call rather than held in a package variable, so one test
// cannot mutate the set another test reads.
func classifiedSentinels() map[string]error {
	return map[string]error{
		"musixmatch.ErrUnauthorized":           musixmatch.ErrUnauthorized,
		"musixmatch.ErrRateLimited":            musixmatch.ErrRateLimited,
		"musixmatch.ErrNotFound":               musixmatch.ErrNotFound,
		"musixmatch.ErrNoLyrics":               musixmatch.ErrNoLyrics,
		"musixmatch.ErrTruncatedResponse":      musixmatch.ErrTruncatedResponse,
		"musixmatch.ErrUnparsableSubtitleBody": musixmatch.ErrUnparsableSubtitleBody,
		"musixmatch.ErrMatchMismatch":          musixmatch.ErrMatchMismatch,
		"musixmatch.ErrTokenRenewalRequired":   musixmatch.ErrTokenRenewalRequired,
		"musixmatch.ErrTokenMintRefused":       musixmatch.ErrTokenMintRefused,
		"petitlyrics.ErrUnauthorized":          petitlyrics.ErrUnauthorized,
		"petitlyrics.ErrRateLimited":           petitlyrics.ErrRateLimited,
		"petitlyrics.ErrForbidden":             petitlyrics.ErrForbidden,
		"petitlyrics.ErrNotFound":              petitlyrics.ErrNotFound,
		"petitlyrics.ErrProviderUnavailable":   petitlyrics.ErrProviderUnavailable,
	}
}

// transportExemptions are sentinels that are CORRECT to classify as
// OutcomeTransport, each with the reason it is not the #748 defect. An
// exemption must be a deliberate decision recorded somewhere in the tree, never
// a way to quiet this test.
//
// Built fresh per call, for the same reason as classifiedSentinels: an
// exemption list a test could mutate is an exemption list that can be widened
// by accident.
func transportExemptions() map[string]string {
	return map[string]string{
		// A 403 is a refused request SHAPE, not a credential or throttle condition,
		// and no amount of waiting or rotation fixes it. Bucketing it with the
		// auth/throttle signals would repeat the #495 misdiagnosis, where a
		// User-Agent denylist rejection read as a phantom rate limit. Transport is
		// the correct class; internal/orchestrator/errors.go says so explicitly.
		"petitlyrics.ErrForbidden": "a refused request shape, deliberately not an auth/throttle signal (#495)",
		// Bootstrap-path only: returned by the token mint in internal/musixmatch/
		// token.go and handled in internal/commands/token_bootstrap.go. It is never
		// produced by a lyric lookup, so it cannot reach a lane and therefore cannot
		// reach ClassifyOutcome at all.
		"musixmatch.ErrTokenMintRefused": "token-bootstrap path only; never returned by a lookup, so it never reaches a lane",
	}
}

// TestEveryProviderSentinelIsClassified is the #748 guard: every exported error
// sentinel a provider package declares must classify as something other than
// the default transport bucket, or carry a documented exemption.
//
// The failure message names the missing sentinel so the fix is obvious from the
// failure alone (an explicit acceptance criterion of #748).
func TestEveryProviderSentinelIsClassified(t *testing.T) {
	found := exportedSentinelsFromSource(t)
	classified := classifiedSentinels()
	exemptions := transportExemptions()

	for _, name := range found {
		sentinel, ok := classified[name]
		if !ok {
			t.Errorf("provider sentinel %s is not covered by this test.\n"+
				"Add it to classifiedSentinels, then make sure ClassifyOutcome handles it.\n"+
				"An unclassified sentinel falls to OutcomeTransport, which outranks a benign\n"+
				"miss -- so an ordinary double-miss is recorded as a queue failure (#748).", name)
			continue
		}

		// Wrap it, the way a lane returns it in production: ClassifyOutcome uses
		// errors.Is throughout, so a bare sentinel would under-test the real path.
		got := ClassifyOutcome(fmt.Errorf("lane: %w", sentinel))
		if got != OutcomeTransport {
			continue
		}
		if reason, exempt := exemptions[name]; exempt {
			t.Logf("%s classifies as transport by design: %s", name, reason)
			continue
		}
		t.Errorf("provider sentinel %s classifies as OutcomeTransport (the default arm).\n"+
			"Transport outranks OutcomeBenignMiss in precedence(), so on an ordinary\n"+
			"double-miss the worker records a queue FAILURE against the row: attempts++,\n"+
			"geometric backoff, eventual retirement (#748).\n"+
			"Add a case for it in ClassifyOutcome, or add a documented entry to\n"+
			"transportExemptions saying why transport is correct.", name)
	}
}

// TestSentinelExemptionsAreLive keeps the exemption lists honest: an entry
// naming a sentinel that no longer exists is a stale carve-out that would go on
// silently excusing a name nothing declares.
func TestSentinelExemptionsAreLive(t *testing.T) {
	live := map[string]bool{}
	for _, name := range exportedSentinelsFromSource(t) {
		live[name] = true
	}
	for name := range transportExemptions() {
		if !live[name] {
			t.Errorf("transportExemptions names %s, which no provider package declares; remove the stale entry", name)
		}
	}
	for name := range classifiedSentinels() {
		if !live[name] {
			t.Errorf("classifiedSentinels names %s, which no provider package declares; remove the stale entry", name)
		}
	}
}

// exportedSentinelsFromSource parses the provider packages and returns every
// exported package-level error sentinel as "<pkg>.<Name>". Parsing the source
// is what makes this a real enumeration: reflection cannot list a package's
// variables, so a hand-written list would drift exactly the way ClassifyOutcome
// itself drifted.
//
// It recognizes both declaration forms the provider packages actually use:
// `errors.New(...)` and `fmt.Errorf(...)` (petitlyrics.ErrProviderUnavailable
// wraps ErrNotFound, so it must be an Errorf), and it accepts a var with an
// explicit `error` type as well as an inferred one.
func exportedSentinelsFromSource(t *testing.T) []string {
	t.Helper()

	var out []string
	for _, dir := range providerPackages(t) {
		pkgName := filepath.Base(dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		fset := token.NewFileSet()
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			// ParseFile rather than the deprecated ParseDir (SA1019). Walking the
			// directory ourselves is equivalent here and avoids pulling
			// golang.org/x/tools/go/packages in for a test that only needs
			// top-level var declarations.
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
			}
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, ident := range vs.Names {
						// Exported-ness is the only NAME condition. An earlier
						// version also required an "Err" prefix, which was a hole:
						// an exported `NoLyrics = errors.New(...)` would have been
						// skipped on its name alone and could restore the very
						// unclassified-transport fallback this guard exists to
						// prevent. declaresAnError below is the real filter -- it
						// checks the TYPE and INITIALIZER, which is what actually
						// makes something a sentinel.
						if !ident.IsExported() {
							continue
						}
						if !declaresAnError(vs, i) {
							continue
						}
						out = append(out, pkgName+"."+ident.Name)
					}
				}
			}
		}
	}

	// Self-check: a scan that silently finds nothing would make every assertion
	// above vacuously true, which is the failure mode this whole file exists to
	// prevent. The threshold is deliberately low -- it catches a broken scan, not
	// a shrinking sentinel set.
	if len(out) < 5 {
		t.Fatalf("found only %d provider sentinels (%v); the AST scan is broken, so the #748 guard is not actually testing anything", len(out), out)
	}
	return out
}

// declaresAnError reports whether the i-th name in vs is an error-typed value.
// It accepts an explicit `error` type annotation, or an initializer calling
// errors.New / fmt.Errorf.
func declaresAnError(vs *ast.ValueSpec, i int) bool {
	if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "error" {
		return true
	}
	if i >= len(vs.Values) {
		return false
	}
	call, ok := vs.Values[i].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return (pkg.Name == "errors" && sel.Sel.Name == "New") ||
		(pkg.Name == "fmt" && sel.Sel.Name == "Errorf")
}

// TestUnenumeratedProviderMissLosesToTransport documents the ranking that makes
// an unclassified sentinel harmful, so the guard above cannot be weakened
// without this failing too. It asserts the precedence relationship #748 turns
// on, using a stand-in for a provider whose sentinels were never enumerated.
func TestUnenumeratedProviderMissLosesToTransport(t *testing.T) {
	unenumerated := errors.New("someprovider: no results found")

	if got := ClassifyOutcome(unenumerated); got != OutcomeTransport {
		t.Fatalf("an unrecognized provider error classifies as %v; want OutcomeTransport (the default arm)", got)
	}
	if OutcomeTransport.precedence() <= OutcomeBenignMiss.precedence() {
		t.Fatalf("transport precedence (%d) no longer outranks benign miss (%d); the #748 mechanism has changed and this guard needs revisiting",
			OutcomeTransport.precedence(), OutcomeBenignMiss.precedence())
	}
}
