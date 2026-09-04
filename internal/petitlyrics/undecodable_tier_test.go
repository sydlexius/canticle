package petitlyrics

import (
	"encoding/binary"
	"errors"
	"testing"
)

// TestUndecodableLineSyncWrapsErrNotFound is a CHARACTERIZATION test: it pins
// what an undecodable tier-2 payload ACTUALLY produces today, through the real
// decode path rather than a stub that manufactures a sentinel.
//
// It exists to make the removal of ErrUnsupportedTier (#765) provably
// behavior-neutral, so it must PASS BOTH BEFORE AND AFTER that change. A test
// that only passes afterwards would prove the new behavior, not the absence of
// a change -- which is the whole question here.
//
// Every refusal below wraps ErrNotFound, never ErrUnsupportedTier. That is why
// the sentinel is unreachable: #763's decoder handles all three tiers, and a
// payload that fails inside the decoder degrades to a clean miss.
//
// Payloads are synthetic byte patterns, never captured provider content.
func TestUndecodableLineSyncWrapsErrNotFound(t *testing.T) {
	// A well-formed header the individual cases mutate. lineCount is at
	// lsyLineCountOff; the payload proper starts at lsyPayloadStart.
	header := func(lineCount uint32, lineLength uint16) []byte {
		b := make([]byte, lsyPayloadStart)
		binary.LittleEndian.PutUint32(b[lsyLineCountOff:], lineCount)
		binary.LittleEndian.PutUint16(b[lsyLineLengthOff:], lineLength)
		return b
	}

	cases := []struct {
		name string
		raw  []byte
	}{
		{"shorter than the minimum header", []byte{0x00, 0x01, 0x02}},
		{"declares zero lines", header(0, 0)},
		{"declares a negative line count", header(^uint32(0), 0)},
		{"declares more lines than the cap", header(uint32(lsyMaxLines)+1, 0)},
		{"truncated before the timestamp array ends", header(64, 0)},
		{"geometry disagrees with the actual length", append(header(2, 40), make([]byte, 4)...)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeLineSyncTimings(c.raw)
			if err == nil {
				t.Fatal("decoded an undecodable payload without error")
			}
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("error = %v; want it to wrap ErrNotFound (the sentinel every "+
					"tier-2 refusal degrades through since #763)", err)
			}
		})
	}
}

// TestClassifyPayloadIsTotal is the load-bearing assertion behind REMOVING
// ErrUnsupportedTier rather than keeping it for forward compatibility.
//
// classifyPayload has three arms and NO unknown-tier fallthrough: every input
// maps to word-sync, line-sync, or unsynced. So no payload -- including a
// hypothetical future tier -- can reach an "unsupported tier" branch. It would
// present as one of these three shapes and fail inside that decoder, wrapping
// ErrNotFound.
//
// That is what makes the forward-compat argument for retaining the sentinel
// illusory: reaching it would require a NEW classifyPayload arm, and whoever
// adds one must choose an outcome class deliberately rather than inherit
// OutcomeBenignMiss from a sentinel nobody re-derived.
func TestClassifyPayloadIsTotal(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want int
	}{
		{"wsy xml root", []byte(`<wsy><line/></wsy>`), tierWordSync},
		{"binary with a NUL byte", []byte{0x41, 0x00, 0x42}, tierLineSync},
		{"invalid utf-8", []byte{0xff, 0xfe, 0xfd}, tierLineSync},
		{"plain utf-8 text", []byte("plain text line"), tierUnsynced},
		{"empty", []byte{}, tierUnsynced},
		{"multibyte utf-8", []byte("日本語"), tierUnsynced},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPayload(c.raw); got != c.want {
				t.Errorf("classifyPayload = %d; want %d", got, c.want)
			}
		})
	}

	// Every arm returns one of exactly three tiers -- there is no fourth value
	// and no default that could mean "unsupported".
	for _, raw := range [][]byte{{}, {0x00}, []byte("x"), []byte(`<wsy>`), {0xff}} {
		switch got := classifyPayload(raw); got {
		case tierWordSync, tierLineSync, tierUnsynced:
		default:
			t.Errorf("classifyPayload returned %d, outside the three known tiers", got)
		}
	}
}
