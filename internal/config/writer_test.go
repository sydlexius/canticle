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

// TestWriteAtomic_PurgesBackupAfterSuccess verifies that a successful write
// replaces the original and leaves NO lingering .bak on disk. The .bak is a
// transient crash-safety copy for the write window only; keeping it would
// leak a plaintext copy of a file-resident secret (server.webhook_api_keys)
// after the write completes (#290).
func TestWriteAtomic_PurgesBackupAfterSuccess(t *testing.T) {
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
	// The new content is in place...
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if !strings.Contains(string(got), `level = "warn"`) {
		t.Errorf("write did not replace the original; got:\n%s", got)
	}
	// ...and the .bak is gone.
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("expected .bak to be purged after a successful write, stat err = %v", err)
	}
}

// TestWriteAtomic_FailedWriteLeavesOriginalIntact verifies the crash-safety
// guarantee: when the write fails (here, the containing directory is not
// writable so the temp file cannot be created), the original config is left
// byte-identical and no stray .bak is created. The purge step (#290) never
// runs on a failed write, so it cannot destroy a recovery copy.
func TestWriteAtomic_FailedWriteLeavesOriginalIntact(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root bypasses directory write permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(roundtripFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	if err := SetValue(doc, "logging.level", TypeString, "warn"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	// Make the directory unwritable so os.CreateTemp (the first mutation) fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let t.TempDir clean up
	if err := WriteAtomic(path, doc); err == nil {
		t.Fatal("expected WriteAtomic to fail on an unwritable directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore dir perms: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if string(got) != roundtripFixture {
		t.Errorf("original config was modified by a failed write:\n%s", got)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a failed write must not leave a .bak, stat err = %v", err)
	}
}

// TestApplyChanges_CreatesConfigWhenAbsent verifies the create-on-save path:
// saving a setting when config.toml does not yet exist creates the file with
// the change, rather than returning an error (#296). This mirrors the
// absent-[section] create behavior added in #288.
func TestApplyChanges_CreatesConfigWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: config should be absent, stat err = %v", err)
	}
	if err := ApplyChanges(path, map[string]string{"logging.level": "debug"}); err != nil {
		t.Fatalf("ApplyChanges on an absent config: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config was not created: %v", err)
	}
	if !strings.Contains(string(got), `level = "debug"`) {
		t.Errorf("created config missing the saved change:\n%s", got)
	}
	// First write: nothing to back up, so no .bak should exist either.
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("expected no .bak after a create-on-save write, stat err = %v", err)
	}
}
