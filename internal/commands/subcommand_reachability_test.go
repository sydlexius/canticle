package commands

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// legacyUsageMarker is the one string unique to the pre-subcommand CLI's usage
// line: a flag bracket immediately after the program name. A subcommand always
// renders "Usage: canticle <name> [...", so seeing this means the parser fell
// through to LegacyArgs.
//
// Deliberately not matching on a flag like "[--outdir OUTDIR]" -- fetch and
// serve declare their own --outdir, so that marker produces false positives.
const legacyUsageMarker = "Usage: canticle ["

// TestEverySubcommandIsReachableThroughRun is the test whose absence let a
// broken command ship in v1.20.0.
//
// Every existing test drove the handlers directly (runAdmin, runKeys, ...),
// which proves a handler works and says nothing about whether anything can
// REACH it. `admin` was declared on Args, wired into the dispatch switch, and
// listed in the completion tables, but missing from the subcommand-recognition
// set -- so `canticle admin set-password --user x` fell through to the legacy
// parser and failed with "unknown argument --user". The command could not be
// invoked at all, and the whole suite passed.
//
// This drives the real entry point with real argv, and asserts the legacy
// parser never claims a declared subcommand. It is deliberately behavioral
// rather than a table lookup: a future refactor of the recognition mechanism
// still has to keep every command reachable.
func TestEverySubcommandIsReachableThroughRun(t *testing.T) {
	names := subcommandNames(reflect.TypeOf(Args{}))
	if len(names) == 0 {
		t.Fatal("no subcommands discovered on Args; the reflection helper is broken")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			// --help makes every subcommand terminate immediately without doing
			// any work: no config load, no database, no network. What matters is
			// only which parser claimed the argv.
			Run(t.Context(), []string{name, "--help"}, &out, Deps{})

			if strings.Contains(out.String(), legacyUsageMarker) {
				t.Errorf("subcommand %q fell through to the LEGACY parser and is unreachable.\n"+
					"It is declared on Args but the subcommand-recognition set does not know it.\n"+
					"got:\n%s", name, out.String())
			}
		})
	}
}

// TestUnknownCommandStillUsesLegacyParser is the other half of the contract:
// the fallback must keep working. A bare song argument is the legacy CLI's
// positional form and must NOT be mistaken for a subcommand.
func TestUnknownCommandStillUsesLegacyParser(t *testing.T) {
	if usesSubcommand([]string{"Some Artist,Some Title"}) {
		t.Error("a positional song argument was treated as a subcommand; the legacy CLI would break")
	}
	if usesSubcommand([]string{"definitely-not-a-command"}) {
		t.Error("an unknown word was treated as a subcommand")
	}
	if !usesSubcommand([]string{"--help"}) {
		t.Error("--help should route to the subcommand parser so the full tree is shown")
	}
	if usesSubcommand(nil) {
		t.Error("empty argv should use the legacy parser")
	}
}
