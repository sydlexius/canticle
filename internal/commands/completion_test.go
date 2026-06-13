package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/library"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

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
