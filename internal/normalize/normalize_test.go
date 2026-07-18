package normalize_test

import (
	"testing"

	"github.com/sydlexius/canticle/internal/normalize"
)

func TestNormalizeKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "plain ASCII", input: "hello world", want: "hello world"},
		{name: "leading/trailing spaces", input: "  Héllo Wörld  ", want: "hello world"},
		{name: "angstrom", input: "Ångström", want: "angstrom"},
		{name: "already lowercase", input: "beatles", want: "beatles"},
		{name: "uppercase", input: "THE BEATLES", want: "the beatles"},
		{name: "accented composed", input: "café", want: "cafe"},
		{name: "japanese ascii-like", input: "ｈｅｌｌｏ", want: "hello"},        // NFKD normalizes fullwidth
		{name: "invalid utf-8 replaced", input: "hell\x80o", want: "hello"}, // invalid byte stripped
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize.NormalizeKey(tc.input)
			if got != tc.want {
				t.Errorf("NormalizeKey(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDurationBucket(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    int
	}{
		// Sentinel: zero and negatives all return 0.
		{name: "zero", seconds: 0, want: 0},
		{name: "negative one", seconds: -1, want: 0},
		{name: "negative large", seconds: -300, want: 0},
		// Boundary cases.
		{name: "one second", seconds: 1, want: 0},
		{name: "four seconds", seconds: 4, want: 0},
		{name: "five seconds", seconds: 5, want: 1},
		{name: "six seconds", seconds: 6, want: 1},
		// Representative values matching migration 014 comments.
		{name: "180 seconds", seconds: 180, want: 36},
		{name: "210 seconds", seconds: 210, want: 42},
		{name: "240 seconds", seconds: 240, want: 48},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize.DurationBucket(tc.seconds)
			if got != tc.want {
				t.Errorf("DurationBucket(%d) = %d, want %d", tc.seconds, got, tc.want)
			}
		})
	}
}

func fptr(f float64) *float64 { return &f }

func TestMatchConfidence(t *testing.T) {
	tests := []struct {
		name   string
		a, b   string
		wantGt *float64 // got must be > *wantGt; nil to skip
		wantLt *float64 // got must be < *wantLt; nil to skip
		wantEq *float64 // exact expected value; nil to skip
	}{
		{name: "identical", a: "hello", b: "hello", wantEq: fptr(1.0)},
		{name: "both empty", a: "", b: "", wantEq: fptr(1.0)},
		{name: "one empty", a: "hello", b: "", wantEq: fptr(0.0)},
		{name: "near match transposition", a: "hello", b: "helol", wantGt: fptr(0.9), wantLt: fptr(1.0)},
		{name: "completely different", a: "abc", b: "xyz", wantLt: fptr(0.5)},
		{name: "case insensitive", a: "Hello", b: "hello", wantEq: fptr(1.0)},
		{name: "accent insensitive", a: "Héllo", b: "hello", wantEq: fptr(1.0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantEq != nil && (tc.wantGt != nil || tc.wantLt != nil) {
				t.Fatalf("invalid test case %q: wantEq cannot be combined with wantGt/wantLt", tc.name)
			}
			got := normalize.MatchConfidence(tc.a, tc.b)
			if tc.wantEq != nil {
				if got != *tc.wantEq {
					t.Errorf("MatchConfidence(%q, %q) = %f, want exactly %f", tc.a, tc.b, got, *tc.wantEq)
				}
				return
			}
			if tc.wantGt != nil && got <= *tc.wantGt {
				t.Errorf("MatchConfidence(%q, %q) = %f, want > %f", tc.a, tc.b, got, *tc.wantGt)
			}
			if tc.wantLt != nil && got >= *tc.wantLt {
				t.Errorf("MatchConfidence(%q, %q) = %f, want < %f", tc.a, tc.b, got, *tc.wantLt)
			}
		})
	}
}
