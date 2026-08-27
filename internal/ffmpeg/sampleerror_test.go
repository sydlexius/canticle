package ffmpeg

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestSampleErrorRendersExpectedFormat pins the exact rendered string. Both
// call sites (internal/verification, internal/detector) built this error
// longhand with an identical format before this helper existed; the literal
// here is that pre-refactor text with the subsystem substituted in, so a
// change to the template shape is caught here rather than absorbed silently.
func TestSampleErrorRendersExpectedFormat(t *testing.T) {
	cause := errors.New("exit status 1")
	got := SampleError("verification", cause, "decoder opened\nHeader missing").Error()
	want := "verification: sample audio with ffmpeg: exit status 1: decoder opened\nHeader missing"
	if got != want {
		t.Fatalf("SampleError rendered:\n got %q\nwant %q", got, want)
	}
}

// A second subsystem prefix, to prove the prefix is actually a parameter and
// not a hardcoded "verification" that happens to match the first test.
func TestSampleErrorRendersExpectedFormatForDetector(t *testing.T) {
	cause := errors.New("exit status 69")
	got := SampleError("detector", cause, "trailing output").Error()
	want := "detector: sample audio with ffmpeg: exit status 69: trailing output"
	if got != want {
		t.Fatalf("SampleError rendered:\n got %q\nwant %q", got, want)
	}
}

// The %w wrapping must survive the helper: callers rely on errors.Is/As
// reaching the underlying ffmpeg exec error.
func TestSampleErrorWrapsUnderlyingError(t *testing.T) {
	sentinel := errors.New("sentinel exec failure")
	err := SampleError("verification", sentinel, "output")
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, sentinel) = false; want true (the %%w wrap was lost)")
	}
}

// BoundOutput must be applied internally, so a caller cannot forget it. Feed
// output well past the cap and confirm the rendered error is bounded and
// carries the elision marker, not the raw unbounded text.
func TestSampleErrorAppliesBoundOutputInternally(t *testing.T) {
	huge := strings.Repeat("x", maxCapturedOutput*3)
	err := SampleError("detector", errors.New("boom"), huge)
	msg := err.Error()
	if len(msg) >= len(huge) {
		t.Fatalf("SampleError did not bound the output: rendered message is %d bytes against %d bytes of input", len(msg), len(huge))
	}
	if !strings.Contains(msg, "bytes omitted") {
		t.Errorf("bounded output missing the elision marker; got %q", msg)
	}
}

// Positive control for TestSampleErrorAppliesBoundOutputInternally: short
// output must NOT be bounded/altered beyond the trim BoundOutput already
// does, proving the length assertion above can actually distinguish "bounded"
// from "not bounded" rather than trivially passing for any output.
func TestSampleErrorShortOutputIsNotElided(t *testing.T) {
	err := SampleError("detector", errors.New("boom"), "short output")
	if strings.Contains(err.Error(), "bytes omitted") {
		t.Fatalf("short output was elided; want it to pass through BoundOutput untouched: %q", err.Error())
	}
}

// Output surrounding whitespace is trimmed by BoundOutput; assert that
// behavior is reachable through the helper too (not bypassed by a caller
// pre-formatting anything itself).
func TestSampleErrorTrimsOutputWhitespace(t *testing.T) {
	err := SampleError("verification", errors.New("boom"), "  \n padded \n\n")
	want := "verification: sample audio with ffmpeg: boom: padded"
	if err.Error() != want {
		t.Fatalf("SampleError = %q; want %q", err.Error(), want)
	}
}

// fmt.Sprintf sanity check documenting the exact template this test suite
// pins against, so a future reader can see the literal and the constant
// together rather than trusting two independently-typed strings agree.
func TestSampleErrorTemplateDocumentedLiteral(t *testing.T) {
	cause := errors.New("c")
	got := SampleError("s", cause, "o").Error()
	want := fmt.Sprintf("%s: sample audio with ffmpeg: %s: %s", "s", "c", "o")
	if got != want {
		t.Fatalf("SampleError = %q; want %q", got, want)
	}
}
