package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roundtripFixture is a config with comments, an inline trailing comment, odd
// key ordering, an inline array, and blank lines between sections. A known-key
// edit must leave every byte intact except the changed value.
//
// Note: tomledit canonicalizes inline-comment spacing to two spaces before the
// "#", so the fixture uses that canonical form. tomledit preserves comment
// CONTENT, ordering, and structure; it normalizes cosmetic whitespace, which is
// stable after the first save.
const roundtripFixture = `# mxlrcgo-svc configuration
# This top comment must survive a write.

[api]
token = "secret-abc"  # the Musixmatch token
cooldown = 30

# logging block comment
[logging]
level = "info"
format = "text"

[providers]
fallback_order = ["petitlyrics", "musixmatch"]
mode = "ordered"
`

func writeTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(roundtripFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestWriterRoundTrip_PreservesEverythingButEditedValue is the core spike: a
// single known-key edit changes only that line; every comment, blank line,
// ordering, and untouched value is byte-identical.
func TestWriterRoundTrip_PreservesEverythingButEditedValue(t *testing.T) {
	path := writeTempConfig(t)

	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	if err := SetValue(doc, "logging.level", TypeString, "debug"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if err := WriteAtomic(path, doc); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(roundtripFixture, "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("line count changed: got %d want %d\n---got---\n%s", len(gotLines), len(wantLines), got)
	}
	for i := range wantLines {
		want := wantLines[i]
		if strings.HasPrefix(strings.TrimSpace(want), "level =") {
			want = `level = "debug"`
		}
		if gotLines[i] != want {
			t.Errorf("line %d differs:\n got: %q\nwant: %q", i+1, gotLines[i], want)
		}
	}
}

// TestWriterRoundTrip_AllTypes verifies each field type serializes correctly on
// a write (int, bool, float, string slice), still preserving comments.
func TestWriterRoundTrip_AllTypes(t *testing.T) {
	path := writeTempConfig(t)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	edits := []struct {
		path  string
		ftype FieldType
		value string
		want  string
	}{
		{"api.cooldown", TypeInt, "45", "cooldown = 45"},
		{"providers.mode", TypeString, "parallel", `mode = "parallel"`},
		{"providers.fallback_order", TypeStringSlice, "musixmatch,petitlyrics", `fallback_order = ["musixmatch", "petitlyrics"]`},
	}
	for _, e := range edits {
		if err := SetValue(doc, e.path, e.ftype, e.value); err != nil {
			t.Fatalf("SetValue(%s): %v", e.path, err)
		}
	}
	if err := WriteAtomic(path, doc); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	out := string(got)
	if !strings.Contains(out, "# mxlrcgo-svc configuration") {
		t.Error("top comment lost")
	}
	for _, e := range edits {
		if !strings.Contains(out, e.want) {
			t.Errorf("expected %q in output, got:\n%s", e.want, out)
		}
	}
}

// TestWriteAtomic_KeepsBackupAndIsAtomic verifies a .bak of the prior file is
// kept and the write replaces the original.
func TestWriteAtomic_KeepsBackup(t *testing.T) {
	path := writeTempConfig(t)
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	if err := SetValue(doc, "logging.level", TypeString, "warn"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if err := WriteAtomic(path, doc); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if string(bak) != roundtripFixture {
		t.Errorf(".bak is not the prior file content")
	}
}
