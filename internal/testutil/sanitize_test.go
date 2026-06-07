package testutil

import (
	"strings"
	"testing"
)

// TestSanitizeStripsInvalidFilenameChars verifies that sanitize removes or
// replaces every character that is invalid in a Windows path component, so
// realistic-mode catalog entries such as "(What's the Story) Morning Glory?"
// produce a portable directory/file name.
func TestSanitizeStripsInvalidFilenameChars(t *testing.T) {
	got := sanitize(`a/b\c:d*e?f"g<h>i|j`)
	for _, bad := range []string{`/`, `\`, `:`, `*`, `?`, `"`, `<`, `>`, `|`} {
		if strings.Contains(got, bad) {
			t.Fatalf("sanitize(%q) = %q; still contains invalid char %q", `a/b\c:d*e?f"g<h>i|j`, got, bad)
		}
	}

	// Trailing whitespace is trimmed (Windows rejects trailing spaces/dots).
	if g := sanitize("Morning Glory? "); strings.HasSuffix(g, " ") || strings.Contains(g, "?") {
		t.Fatalf("sanitize trailing/invalid not handled: %q", g)
	}

	// A clean name is left intact.
	if g := sanitize("Adele"); g != "Adele" {
		t.Fatalf("sanitize(%q) = %q; want unchanged", "Adele", g)
	}
}
