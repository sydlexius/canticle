package normalize_test

import (
	"testing"
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/normalize"
)

func FuzzNormalizeKey(f *testing.F) {
	f.Add("")
	f.Add("  H\u00e9llo W\u00f6rld  ")
	f.Add("hell\x80o")
	f.Add("\uff48\uff45\uff4c\uff4c\uff4f")

	f.Fuzz(func(t *testing.T, s string) {
		got := normalize.NormalizeKey(s)
		if !utf8.ValidString(got) {
			t.Fatalf("NormalizeKey returned invalid UTF-8: %q", got)
		}
		if got != normalize.NormalizeKey(got) {
			t.Fatalf("NormalizeKey is not idempotent: %q", got)
		}
	})
}

func FuzzMatchConfidence(f *testing.F) {
	f.Add("", "")
	f.Add("hello", "hello")
	f.Add("H\u00e9llo", "hello")
	f.Add("abc", "xyz")

	f.Fuzz(func(t *testing.T, a string, b string) {
		got := normalize.MatchConfidence(a, b)
		if got < 0 || got > 1 {
			t.Fatalf("MatchConfidence(%q, %q) = %f, want [0,1]", a, b, got)
		}
		if got != normalize.MatchConfidence(b, a) {
			t.Fatalf("MatchConfidence is not symmetric for %q and %q", a, b)
		}
	})
}
