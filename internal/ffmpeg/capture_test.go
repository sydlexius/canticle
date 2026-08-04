package ffmpeg

import (
	"strings"
	"testing"
)

func TestBoundOutputShortInputIsUnchanged(t *testing.T) {
	in := "exit status 1: no such file"
	if got := BoundOutput(in); got != in {
		t.Fatalf("short input was altered:\n got %q\nwant %q", got, in)
	}
}

func TestBoundOutputTrimsSurroundingSpace(t *testing.T) {
	if got := BoundOutput("  \n padded \n\n"); got != "padded" {
		t.Fatalf("surrounding space not trimmed: got %q", got)
	}
}

func TestBoundOutputEmptyStaysEmpty(t *testing.T) {
	if got := BoundOutput("   \n\t "); got != "" {
		t.Fatalf("whitespace-only input should reduce to empty, got %q", got)
	}
}

// The head and the tail are the two ends that carry diagnostic value: the first
// lines name the failing decoder, the last lines carry the cause that actually
// terminated the run. A plain head truncation would keep the former and discard
// the latter, which is the wrong half to lose -- so assert BOTH survive.
func TestBoundOutputKeepsHeadAndTail(t *testing.T) {
	const (
		first = "FIRST-LINE-MARKER decoder opened"
		last  = "LAST-LINE-MARKER decode error rate exceeds maximum"
	)
	var b strings.Builder
	b.WriteString(first + "\n")
	for range 20000 {
		b.WriteString("Header missing\n")
	}
	b.WriteString(last)

	got := BoundOutput(b.String())

	// A HARD LITERAL, deliberately not maxCapturedOutput. Asserting against the
	// constant makes every size check a tautology -- raising the cap raises the
	// assertion with it, and the suite stays green while the defect this exists to
	// prevent comes back. The literal is the contract; the constant is an
	// implementation detail that must satisfy it.
	if len(got) > 4096 {
		t.Fatalf("bounded output exceeds the cap: got %d bytes, want <= 4096", len(got))
	}
	if !strings.Contains(got, first) {
		t.Errorf("head was dropped; want the opening line %q retained", first)
	}
	if !strings.Contains(got, last) {
		t.Errorf("tail was dropped; want the terminating line %q retained", last)
	}
}

// The elision marker is what tells a reader the text is partial. Without it a
// truncated capture reads as a complete one, and the omitted-byte count is the
// signal that the run was pathological rather than merely noisy.
func TestBoundOutputAnnouncesTheElision(t *testing.T) {
	got := BoundOutput(strings.Repeat("x", maxCapturedOutput*3))

	if !strings.Contains(got, "omitted") {
		t.Fatalf("truncated output does not announce the elision: %q", head(got))
	}
}

// A cap that only holds for multi-line input would miss exactly the case that
// motivated it if a subprocess ever emits one enormous line.
func TestBoundOutputCapsSingleEnormousLine(t *testing.T) {
	got := BoundOutput(strings.Repeat("y", 600_000))
	if len(got) > 4096 {
		t.Fatalf("single-line input evaded the cap: got %d bytes, want <= 4096", len(got))
	}
}

// The cap is a BYTE cap, and multi-byte input is where a length-vs-bytes mix-up
// hides: the migration's first draft measured characters and let a 12,000-byte
// value through as "4,096". Pin the byte ceiling across encodings so the Go side
// can never acquire the same defect.
func TestBoundOutputCapsBytesNotCharacters(t *testing.T) {
	for _, tc := range []struct{ name, r string }{
		{"two-byte runes", "é"},
		{"three-byte runes", "€"},
		{"four-byte runes", "😀"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := BoundOutput(strings.Repeat(tc.r, 20000))
			if len(got) > 4096 {
				t.Errorf("multi-byte input exceeded the BYTE cap: got %d bytes, want <= 4096", len(got))
			}
			if strings.ContainsRune(got, '�') {
				t.Error("bounding produced U+FFFD, so it cut through a rune")
			}
		})
	}
}

// Bounding must not corrupt multi-byte characters: a byte-wise cut through a
// rune would put invalid UTF-8 into last_error and then into the HTML report.
func TestBoundOutputDoesNotSplitARune(t *testing.T) {
	got := BoundOutput(strings.Repeat("é", maxCapturedOutput))
	if strings.ContainsRune(got, '�') {
		t.Fatal("bounding split a multi-byte rune, producing U+FFFD")
	}
}

// The small-cap guard exists to keep boundTo total if maxCapturedOutput is ever
// lowered. It is unreachable through BoundOutput at the current constant, which
// is exactly why boundTo takes the limit as a parameter -- an invariant asserted
// only by unreachable code is an invariant nothing protects.
//
// What must hold: the result still honors the limit it was given, and it still
// ANNOUNCES the elision. Silently returning a bare prefix here would make a
// truncated value read as a complete one, which is the property every caller
// depends on.
func TestBoundToSmallCapStillAnnouncesTheElision(t *testing.T) {
	got := boundTo(strings.Repeat("x", 1000), 30)

	if !strings.Contains(got, "omitted") {
		t.Errorf("small-cap path dropped the elision marker, so the result reads as complete: %q", got)
	}
	if strings.Contains(got, "xxxxxxxxxx") {
		t.Errorf("small-cap path leaked a run of payload it had no budget for: %q", got)
	}
}

// Both rune-walking helpers short-circuit when the string already fits. That
// branch is what keeps an ordinary, in-budget capture from being copied through
// a needless boundary walk, so it is worth pinning directly rather than only
// through BoundOutput.
func TestTruncateHelpersReturnShortInputUnchanged(t *testing.T) {
	const s = "short"
	if got := truncateRunes(s, 100); got != s {
		t.Errorf("truncateRunes altered an in-budget string: got %q, want %q", got, s)
	}
	if got := truncateRunesFromEnd(s, 100); got != s {
		t.Errorf("truncateRunesFromEnd altered an in-budget string: got %q, want %q", got, s)
	}
}

func head(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "..."
}
