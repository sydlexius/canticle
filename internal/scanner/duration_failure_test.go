package scanner

import (
	"errors"
	"strings"
	"testing"
)

// A duration parse failure previously left NO TRACE: it was logged at Debug --
// off in production -- and discarded, so a caller could not distinguish an
// unparsable file from one that legitimately has no duration. Two library
// files recorded duration 0 for months on a third-party parser defect and
// surfaced only when someone inspected an index run by hand (#651).
//
// These tests pin the distinction the fix rests on: a parser that RAN AND
// FAILED is reported upward, while an extension with NO PARSER is not.

// audioDuration must tag the no-parser case with a sentinel, so callers can
// tell "this scanner never times .wma" from "this .mp3 is broken". Without the
// sentinel the two collapse into one string-matched error and the real signal
// is buried in the expected one.
func TestAudioDurationSentinelsUnsupportedExtension(t *testing.T) {
	_, err := audioDuration(strings.NewReader(""), ".wma")
	if err == nil {
		t.Fatal("an unsupported extension must return an error")
	}
	if !errors.Is(err, ErrNoDurationParser) {
		t.Errorf("error %q is not ErrNoDurationParser; callers cannot distinguish it from a real parse failure", err)
	}
	if !strings.Contains(err.Error(), ".wma") {
		t.Errorf("error %q should name the extension", err)
	}
}

// A SUPPORTED extension whose parse fails must NOT carry the sentinel: it is a
// genuine defect, and conflating it with "no parser" is exactly how the
// original bug stayed invisible.
func TestAudioDurationRealFailureIsNotSentinelled(t *testing.T) {
	// Valid extension, garbage content: the parser runs and fails.
	_, err := audioDuration(strings.NewReader("not an mp3 at all"), ".mp3")
	if err == nil {
		t.Fatal("garbage content on a supported extension must return an error")
	}
	if errors.Is(err, ErrNoDurationParser) {
		t.Error("a real parse failure must NOT be tagged ErrNoDurationParser; that would hide it among expected cases")
	}
}
