package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigContent writes arbitrary TOML content to a temp config file and
// returns its path. Used by the comment-stamping tests, which need fixtures
// with specific existing comment blocks.
func writeConfigContent(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// saveWithDescription round-trips a single SetValue+WriteAtomic against path and
// returns the resulting file content.
func saveWithDescription(t *testing.T, path, fieldPath string, ftype FieldType, value, desc string) string {
	t.Helper()
	doc, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}
	if err := SetValue(doc, fieldPath, ftype, value, desc); err != nil {
		t.Fatalf("SetValue(%s): %v", fieldPath, err)
	}
	if err := WriteAtomic(path, doc); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	return string(out)
}

// TestStampComment_AddsWhenAbsent verifies a managed "# mxlrc: <desc>" comment
// is stamped above a key that has no existing block comment.
func TestStampComment_AddsWhenAbsent(t *testing.T) {
	path := writeConfigContent(t, "[logging]\nlevel = \"info\"\n")
	out := saveWithDescription(t, path, "logging.level", TypeString, "info", "Log verbosity: debug, info, warn, or error.")

	want := "# mxlrc: Log verbosity: debug, info, warn, or error."
	if !strings.Contains(out, want) {
		t.Errorf("managed comment not stamped; want %q in:\n%s", want, out)
	}
	// The comment must sit immediately above the key.
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "level =") {
			if i == 0 || strings.TrimSpace(lines[i-1]) != want {
				t.Errorf("comment is not directly above the key; line above is %q", lines[i-1])
			}
		}
	}
}

// TestStampComment_PreservesUserBlock verifies an operator's hand-written
// comment above a key is never overwritten by the managed stamp.
func TestStampComment_PreservesUserBlock(t *testing.T) {
	const fixture = "[api]\n# operator note: do not touch this\ncooldown = 30\n"
	path := writeConfigContent(t, fixture)
	out := saveWithDescription(t, path, "api.cooldown", TypeInt, "45", "Minimum seconds between Musixmatch requests.")

	if !strings.Contains(out, "# operator note: do not touch this") {
		t.Errorf("operator comment was lost:\n%s", out)
	}
	if strings.Contains(out, "# mxlrc:") {
		t.Errorf("managed comment overwrote/added beside an operator comment:\n%s", out)
	}
	if !strings.Contains(out, "cooldown = 45") {
		t.Errorf("value not updated:\n%s", out)
	}
}

// TestStampComment_ReplacesManagedBlock verifies a previously-stamped managed
// comment is replaced when the description changes, with no duplicate left.
func TestStampComment_ReplacesManagedBlock(t *testing.T) {
	const fixture = "[api]\n# mxlrc: old description text\ncooldown = 30\n"
	path := writeConfigContent(t, fixture)
	out := saveWithDescription(t, path, "api.cooldown", TypeInt, "45", "Minimum seconds between Musixmatch requests.")

	if strings.Contains(out, "old description text") {
		t.Errorf("stale managed comment not replaced:\n%s", out)
	}
	if !strings.Contains(out, "# mxlrc: Minimum seconds between Musixmatch requests.") {
		t.Errorf("new managed comment missing:\n%s", out)
	}
	if n := strings.Count(out, "# mxlrc:"); n != 1 {
		t.Errorf("expected exactly one managed comment, found %d:\n%s", n, out)
	}
}

// TestStampComment_IdempotentResave verifies re-saving the same field with the
// same description leaves the file byte-identical (no comment churn).
func TestStampComment_IdempotentResave(t *testing.T) {
	path := writeConfigContent(t, "[logging]\nlevel = \"info\"\n")
	const desc = "Log verbosity: debug, info, warn, or error."

	first := saveWithDescription(t, path, "logging.level", TypeString, "info", desc)
	second := saveWithDescription(t, path, "logging.level", TypeString, "info", desc)

	if first != second {
		t.Errorf("re-save was not idempotent:\n---first---\n%s\n---second---\n%s", first, second)
	}
}

// TestStampComment_LeavesHeadingBlockAndTrailers verifies stamping a key's block
// comment does not disturb a section heading's own block comment or another
// key's trailing inline comment.
func TestStampComment_LeavesHeadingBlockAndTrailers(t *testing.T) {
	const fixture = `[api]
token = "secret-abc"  # the Musixmatch token
cooldown = 30

# logging block comment
[logging]
level = "info"
`
	path := writeConfigContent(t, fixture)
	out := saveWithDescription(t, path, "logging.level", TypeString, "warn", "Log verbosity.")

	for _, must := range []string{
		"# the Musixmatch token",  // value trailer untouched
		"# logging block comment", // heading block untouched
		"# mxlrc: Log verbosity.", // new managed stamp present
		`level = "warn"`,
	} {
		if !strings.Contains(out, must) {
			t.Errorf("expected %q in output:\n%s", must, out)
		}
	}
}

// TestStampComment_EmptyDescriptionNoComment verifies an empty description never
// stamps a comment (the value is written bare).
func TestStampComment_EmptyDescriptionNoComment(t *testing.T) {
	path := writeConfigContent(t, "[logging]\nlevel = \"info\"\n")
	out := saveWithDescription(t, path, "logging.level", TypeString, "warn", "")

	if strings.Contains(out, "# mxlrc:") {
		t.Errorf("empty description should not stamp a comment:\n%s", out)
	}
}

// TestStampComment_InsertBranchStampsManaged verifies inserting an absent key
// (into an existing section) carries the managed comment.
func TestStampComment_InsertBranchStampsManaged(t *testing.T) {
	path := writeConfigContent(t, "[api]\ncooldown = 30\n")
	out := saveWithDescription(t, path, "api.max_miss_attempts", TypeInt, "5", "Retire a queue row after this many misses.")

	if !strings.Contains(out, "# mxlrc: Retire a queue row after this many misses.") {
		t.Errorf("managed comment not stamped on inserted key:\n%s", out)
	}
	if !strings.Contains(out, "max_miss_attempts = 5") {
		t.Errorf("inserted value missing:\n%s", out)
	}
}

// TestStampComment_CreateSectionBranchStampsManaged verifies creating an absent
// section carries the managed comment on the new key.
func TestStampComment_CreateSectionBranchStampsManaged(t *testing.T) {
	path := writeConfigContent(t, "[api]\ncooldown = 30\n")
	out := saveWithDescription(t, path, "enrichment.enabled", TypeBool, "true", "Look up recording IDs before fetching.")

	if !strings.Contains(out, "[enrichment]") {
		t.Errorf("absent section not created:\n%s", out)
	}
	if !strings.Contains(out, "# mxlrc: Look up recording IDs before fetching.") {
		t.Errorf("managed comment not stamped in created section:\n%s", out)
	}
}
