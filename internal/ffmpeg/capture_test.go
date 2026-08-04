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

	if len(got) > maxCapturedOutput {
		t.Fatalf("bounded output exceeds the cap: got %d bytes, cap %d", len(got), maxCapturedOutput)
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
	if strings.Contains(got, "\nx\n") {
		t.Log("marker is present on its own line, as intended")
	}
}

// A cap that only holds for multi-line input would miss exactly the case that
// motivated it if a subprocess ever emits one enormous line.
func TestBoundOutputCapsSingleEnormousLine(t *testing.T) {
	got := BoundOutput(strings.Repeat("y", 600_000))
	if len(got) > maxCapturedOutput {
		t.Fatalf("single-line input evaded the cap: got %d bytes, cap %d", len(got), maxCapturedOutput)
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

func head(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "..."
}
