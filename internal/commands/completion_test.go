package commands

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// subcommandName returns the NAME from an `arg:"subcommand:NAME"` struct tag, or
// "" if the tag declares no subcommand.
func subcommandName(argTag string) string {
	for _, part := range strings.Split(argTag, ",") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(part), "subcommand:"); ok {
			return rest
		}
	}
	return ""
}

// subcommandNames returns every `arg:"subcommand:..."` name declared on the
// fields of struct type t -- the real command tree as go-arg sees it.
func subcommandNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		if n := subcommandName(t.Field(i).Tag.Get("arg")); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// TestCompletionSubcommandsMatchCommandTree guards against the drift that this
// change fixes: every top-level subcommand declared on Args must appear in the
// hand-maintained completionSubcommands table, so a newly added command cannot
// silently go missing from shell completion.
func TestCompletionSubcommandsMatchCommandTree(t *testing.T) {
	known := make(map[string]bool, len(completionSubcommands))
	for _, s := range completionSubcommands {
		known[s] = true
	}
	for _, name := range subcommandNames(reflect.TypeOf(Args{})) {
		if !known[name] {
			t.Errorf("completionSubcommands missing top-level subcommand %q (declared in Args); add it in completion.go", name)
		}
	}
}

// TestCompletionCandidatesCoverNestedSubcommands extends the same guard one level
// down: for each top-level command that has nested subcommands (scan, secrets,
// queue, ...), every declared nested subcommand must appear in its
// completionCandidates slice. Leaf commands (flags only) are skipped.
func TestCompletionCandidatesCoverNestedSubcommands(t *testing.T) {
	argsT := reflect.TypeOf(Args{})
	for i := 0; i < argsT.NumField(); i++ {
		f := argsT.Field(i)
		parent := subcommandName(f.Tag.Get("arg"))
		if parent == "" {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		nested := subcommandNames(ft)
		if len(nested) == 0 {
			continue // leaf command: flags only, nothing nested to mirror
		}
		have := make(map[string]bool, len(completionCandidates[parent]))
		for _, c := range completionCandidates[parent] {
			have[c] = true
		}
		for _, name := range nested {
			if !have[name] {
				t.Errorf("completionCandidates[%q] missing nested subcommand %q (declared in %s); add it in completion.go", parent, name, ft.Name())
			}
		}
	}
}

// TestRunComplete_SecretsSubcommand verifies the newly added secrets command
// surfaces at the first word and offers its nested candidates.
func TestRunComplete_SecretsSubcommand(t *testing.T) {
	var buf bytes.Buffer
	runComplete(context.Background(), &buf, []string{"sec"})
	if !strings.Contains(buf.String(), "secrets") {
		t.Fatalf("want 'secrets' for prefix 'sec'; got %q", buf.String())
	}

	buf.Reset()
	runComplete(context.Background(), &buf, []string{"secrets", ""})
	for _, want := range []string{"import", "set", "list"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("want %q in 'secrets' completions; got %q", want, buf.String())
		}
	}
}

// TestRunComplete_ScanReconcile verifies 'scan <TAB>' now offers reconcile.
func TestRunComplete_ScanReconcile(t *testing.T) {
	var buf bytes.Buffer
	runComplete(context.Background(), &buf, []string{"scan", "rec"})
	if !strings.Contains(buf.String(), "reconcile") {
		t.Fatalf("want 'reconcile' for 'scan rec'; got %q", buf.String())
	}
}

func TestRunComplete_TopLevelPrefix(t *testing.T) {
	var buf bytes.Buffer
	runComplete(context.Background(), &buf, []string{"sc"})
	out := buf.String()
	if !strings.Contains(out, "scan") {
		t.Fatalf("want 'scan' for prefix 'sc'; got %q", out)
	}
	if strings.Contains(out, "fetch") {
		t.Fatalf("did not expect 'fetch' for prefix 'sc'; got %q", out)
	}
}

func TestRunComplete_SubcommandFlags(t *testing.T) {
	var buf bytes.Buffer
	runComplete(context.Background(), &buf, []string{"scan", "--em"})
	if !strings.Contains(buf.String(), "--embedded-lyrics") {
		t.Fatalf("want --embedded-lyrics for 'scan --em'; got %q", buf.String())
	}
}

func TestRunComplete_LibraryNamesFromDB(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, "/music", "MyMusic", models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}
	_ = sqlDB.Close()

	var buf bytes.Buffer
	runComplete(ctx, &buf, []string{"library", ""})
	if !strings.Contains(buf.String(), "MyMusic") {
		t.Fatalf("completion for 'library' missing configured library name; got %q", buf.String())
	}
}

func TestRunCompletion_Scripts(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		var buf bytes.Buffer
		if code := runCompletion(&buf, CompletionCmd{Shell: sh}); code != 0 {
			t.Fatalf("%s: code=%d want 0", sh, code)
		}
		if !strings.Contains(buf.String(), "__complete") {
			t.Fatalf("%s script missing __complete invocation:\n%s", sh, buf.String())
		}
	}
	var buf bytes.Buffer
	if code := runCompletion(&buf, CompletionCmd{Shell: "powershell"}); code != 2 {
		t.Fatalf("unsupported shell: code=%d want 2", code)
	}
}

// TestUsesSubcommandCoversCommandTree guards the registration point that shipped
// a broken command in v1.20.0.
//
// Adding a subcommand requires touching FOUR places: the Args struct, the
// dispatch switch, the completion tables, and the usesSubcommand map that
// decides whether to parse the modern subcommand tree or the legacy CLI. The
// `admin` subcommand was added to the first three and missed here, so
// `canticle admin set-password --user x` fell through to the legacy parser and
// died with "unknown argument --user" -- a command that could not be invoked at
// all, while every unit test passed because they called the handlers directly
// and never went through the parser.
//
// The completion tables had a guard like this one and it caught their omission
// immediately. This map had none, which is the only reason the bug shipped.
func TestUsesSubcommandCoversCommandTree(t *testing.T) {
	for _, name := range subcommandNames(reflect.TypeOf(Args{})) {
		if !usesSubcommand([]string{name}) {
			t.Errorf("usesSubcommand does not recognize %q (declared in Args); add it to the commands map in commands.go, "+
				"or the command silently falls through to the legacy parser and cannot be invoked", name)
		}
	}
}
