package petitlyrics

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// buildLSY constructs a synthetic tier-2 payload with the container geometry of
// a real one. No provider bytes are vendored: the timestamps are supplied by the
// caller and the text region is filler, which is all the decoder reads.
//
// The text region is deliberately filler rather than lorem-ipsum words. The
// decoder does not read it -- tier 2's words come from a separate tier-1
// request -- so putting plausible lyric text here would imply a coupling that
// does not exist.
func buildLSY(t *testing.T, protectionID uint16, switchFlag bool, timingsCS []int) []byte {
	t.Helper()

	const lineLength = 64
	n := len(timingsCS)
	buf := make([]byte, lsyPayloadStart+2*n+lineLength*n)

	copy(buf[0:], "MHDROBJT")
	binary.LittleEndian.PutUint32(buf[0x08:], 44)
	copy(buf[0x0c:], "2.00")
	if switchFlag {
		buf[lsyKeySwitchFlagOff] = 1
	}
	binary.LittleEndian.PutUint16(buf[lsyProtectionIDOff:], protectionID)
	copy(buf[0x2c:], "MLRCOBJT")
	binary.LittleEndian.PutUint32(buf[0x34:], uint32(len(buf)-0x2c))
	binary.LittleEndian.PutUint32(buf[lsyLineCountOff:], uint32(n))
	binary.LittleEndian.PutUint16(buf[lsyLineLengthOff:], lineLength)

	key := deriveLSYKey(protectionID, switchFlag)
	for i, cs := range timingsCS {
		// The wire format stores centiseconds in 16 bits and WRAPS; truncating
		// here is the encoder reproducing that, which is the rollover behavior
		// the decoder is tested against. Masking explicitly rather than
		// converting states that intent and needs no suppression.
		binary.LittleEndian.PutUint16(buf[lsyPayloadStart+2*i:], uint16(cs&0xffff)^key)
	}
	return buf
}

// TestDeriveLSYKey_ReproducesObservedKeys pins the key schedule against values
// measured from real captures.
//
// These four (protection id -> key) pairs are the load-bearing evidence that the
// permutation is right. Each key was FIRST recovered independently from its
// payload's own text region by frequency analysis, then found to equal what this
// schedule computes from the header. Two unrelated derivations agreeing is why
// the schedule is trusted; the issue specifying it flagged the bit manipulation
// as suspect on read, and this is the check that settles it.
//
// The pairs are opaque integers -- no lyric content, no track identity.
func TestDeriveLSYKey_ReproducesObservedKeys(t *testing.T) {
	cases := []struct {
		protectionID uint16
		want         uint16
	}{
		{0xdc18, 0xf424},
		{0xdd1c, 0xf474},
		{0xdc01, 0xf401},
		{0xdc1b, 0xf427},
	}
	for _, tc := range cases {
		if got := deriveLSYKey(tc.protectionID, true); got != tc.want {
			t.Errorf("deriveLSYKey(%#04x, true) = %#04x, want %#04x", tc.protectionID, got, tc.want)
		}
	}

	// With the flag clear the id is used verbatim. No capture exercises this
	// branch, so it is pinned by construction rather than by measurement -- and
	// that gap is deliberate: inventing a permuted expectation here would encode
	// a guess as a fact.
	if got := deriveLSYKey(0xdc18, false); got != 0xdc18 {
		t.Errorf("deriveLSYKey with flag clear = %#04x, want the id unchanged", got)
	}
}

func TestDecodeLineSyncTimings_RoundTrip(t *testing.T) {
	// Exercised across several real protection ids, and with the key-switch flag
	// both set and clear. A single id would let a decoder that ignored the header
	// and hardcoded one key pass -- the ids below derive to four DIFFERENT keys,
	// so only reading the header actually works.
	cases := []struct {
		name         string
		protectionID uint16
		switchFlag   bool
	}{
		{"observed id A, flag set", 0xdc18, true},
		{"observed id B, flag set", 0xdd1c, true},
		{"observed id C, flag set", 0xdc01, true},
		{"flag clear, id used verbatim", 0xdc18, false},
	}
	want := []int{1883, 2110, 2446, 2966, 3413}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeLineSyncTimings(buildLSY(t, tc.protectionID, tc.switchFlag, want))
			if err != nil {
				t.Fatalf("decodeLineSyncTimings: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("got %d timings, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("timing[%d] = %d, want %d", i, got[i], want[i])
				}
			}
		})
	}
}

// TestDecodeLineSyncTimings_UnwrapsRollover is the case the public reference
// implementation gets wrong. Timestamps are u16 centiseconds, so they roll over
// at ~10.9 minutes; a decoder that does not unwrap emits every post-rollover cue
// roughly 10.9 minutes early, which on a long track is silently, confidently
// wrong output rather than a visible failure.
func TestDecodeLineSyncTimings_UnwrapsRollover(t *testing.T) {
	// Third value wraps: 70000 cs does not fit in u16 and lands at 4464.
	want := []int{60000, 65000, 70000, 75000}
	got, err := decodeLineSyncTimings(buildLSY(t, 0xdc18, true, want))
	if err != nil {
		t.Fatalf("decodeLineSyncTimings: %v", err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("timing[%d] = %d, want %d (rollover not unwrapped)", i, got[i], want[i])
		}
	}
	if !isNonDecreasing(got) {
		t.Errorf("unwrapped timings are not monotonic: %v", got)
	}
}

func TestDecodeLineSyncTimings_RejectsMalformed(t *testing.T) {
	good := buildLSY(t, 0xdc18, true, []int{100, 200, 300})

	t.Run("truncated header", func(t *testing.T) {
		if _, err := decodeLineSyncTimings(good[:0x40]); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound for a short header, got %v", err)
		}
	})

	t.Run("truncated timestamp array", func(t *testing.T) {
		// Header intact, payload cut inside the timestamps. Must be a typed miss,
		// never a panic -- this is the shape a partial network read produces.
		short := append([]byte(nil), good[:lsyPayloadStart+2]...)
		if _, err := decodeLineSyncTimings(short); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound for a truncated array, got %v", err)
		}
	})

	t.Run("zero lines", func(t *testing.T) {
		z := append([]byte(nil), good...)
		binary.LittleEndian.PutUint32(z[lsyLineCountOff:], 0)
		if _, err := decodeLineSyncTimings(z); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound for a zero line count, got %v", err)
		}
	})

	t.Run("absurd line count is capped, not allocated", func(t *testing.T) {
		z := append([]byte(nil), good...)
		binary.LittleEndian.PutUint32(z[lsyLineCountOff:], 0xffffffff)
		if _, err := decodeLineSyncTimings(z); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound for an over-cap line count, got %v", err)
		}
	})

	t.Run("geometry disagrees with length", func(t *testing.T) {
		z := append([]byte(nil), good...)
		binary.LittleEndian.PutUint16(z[lsyLineLengthOff:], 128) // real slots are 64
		if _, err := decodeLineSyncTimings(z); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound when declared geometry contradicts the length, got %v", err)
		}
	})
}

// TestZipLineSync_RefusesAMismatch is the guard against the worst outcome this
// lane can produce. Zipping unequal sequences would attribute every later line
// to the wrong moment: an .lrc that looks right and drifts. Failing loudly is
// strictly better than emitting it.
func TestZipLineSync_RefusesAMismatch(t *testing.T) {
	if _, err := zipLineSync([]int{100, 200, 300}, "alpha\nbeta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound when text is shorter than timings, got %v", err)
	}
	if _, err := zipLineSync([]int{100}, "alpha\nbeta\ngamma"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound when text is longer than timings, got %v", err)
	}
}

func TestZipLineSync_PairsInOrderAndToleratesTrailingNewline(t *testing.T) {
	// A trailing newline is conventional and must not fail an aligned pair.
	cues, err := zipLineSync([]int{0, 150, 1234}, "alpha\nbeta\ngamma\n")
	if err != nil {
		t.Fatalf("zipLineSync: %v", err)
	}
	if len(cues) != 3 {
		t.Fatalf("got %d cues, want 3", len(cues))
	}
	if cues[0].Text != "alpha" || cues[2].Text != "gamma" {
		t.Errorf("cue text out of order: %q .. %q", cues[0].Text, cues[2].Text)
	}
	// 1234 cs = 12.34 s.
	if got := cues[2].Time.Total; got < 12.33 || got > 12.35 {
		t.Errorf("cue[2] time = %v s, want ~12.34 s (centiseconds wrongly scaled?)", got)
	}
	// CRLF must not leave a stray carriage return welded to the text.
	crlf, err := zipLineSync([]int{0, 100}, "alpha\r\nbeta\r\n")
	if err != nil {
		t.Fatalf("zipLineSync with CRLF: %v", err)
	}
	if strings.ContainsRune(crlf[0].Text, '\r') {
		t.Errorf("CRLF survived into cue text: %q", crlf[0].Text)
	}
}

func isNonDecreasing(v []int) bool {
	for i := 1; i < len(v); i++ {
		if v[i] < v[i-1] {
			return false
		}
	}
	return true
}

// TestZipLineSync_KeepsInteriorBlankLines pins a behavior zipLineSync documents
// but nothing previously tested. An interior blank is a timed line as far as
// this lane is concerned; dropping one would shift every later cue onto the
// wrong moment.
//
// The count-mismatch guard cannot catch that regression: dropping an interior
// blank removes one line AND one pairing together, so the counts still agree
// and the zip succeeds while the alignment is wrong. That is exactly the silent
// drift this lane is written to prevent, which is why it needs its own test.
func TestZipLineSync_KeepsInteriorBlankLines(t *testing.T) {
	cues, err := zipLineSync([]int{0, 100, 200}, "alpha\n\ngamma\n")
	if err != nil {
		t.Fatalf("zipLineSync: %v", err)
	}
	if len(cues) != 3 {
		t.Fatalf("got %d cues, want 3 (interior blank dropped?)", len(cues))
	}
	if cues[1].Text != "" {
		t.Errorf("interior blank cue text = %q, want empty", cues[1].Text)
	}
	if cues[2].Text != "gamma" {
		t.Errorf("cue[2] = %q, want \"gamma\" -- a dropped interior blank shifts every later cue", cues[2].Text)
	}
	if got := cues[2].Time.Total; got < 1.99 || got > 2.01 {
		t.Errorf("cue[2] time = %v s, want ~2.0 s", got)
	}
}

// songXML builds one <song> element with an explicit lyrics id. Synthetic
// filler only -- no provider content.
func songXML(lyricsID, title string, payload []byte) string {
	return `<song><lyricsId>` + lyricsID + `</lyricsId><title>` + title +
		`</title><lyricsType>1</lyricsType><lyricsData>` +
		base64.StdEncoding.EncodeToString(payload) + `</lyricsData></song>`
}

// TestLineSync_TextLookupPinsTheLyricsID is the regression test for the defect
// CodeRabbit caught on this PR.
//
// The tier-1 continuation used to re-run selectCandidate, which scores on ISRC,
// duration, and title text and never consults the lyrics id. So the second
// request could settle on a DIFFERENT recording than the one that supplied the
// timings. When that other recording carries the SAME LINE COUNT, zipLineSync
// sees a well-formed pair and emits an .lrc whose every cue belongs to another
// performance -- undetectable downstream.
//
// The fixture is built to reproduce exactly that: the decoy is listed FIRST and
// is given the title that scores best against the requested track, so a
// scoring selector prefers it, and it carries the same number of lines so the
// count guard cannot save us. Only pinning the id yields the right text.
func TestLineSync_TextLookupPinsTheLyricsID(t *testing.T) {
	const wantID, decoyID = "222", "111"
	blob := buildLSY(t, 0xdc18, true, []int{100, 250, 400})

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		if r.PostForm.Get("lyricsType") == "1" {
			// Decoy first, and titled to out-score the real one.
			_, _ = w.Write([]byte(`<response><songs>` +
				songXML(decoyID, "Lorem Ipsum", []byte("wrong1\nwrong2\nwrong3\n")) +
				songXML(wantID, "Lorem Ipsum (Live)", []byte("right1\nright2\nright3\n")) +
				`</songs></response>`))
			return
		}
		// The tier-2 response identifies which record the timings came from.
		_, _ = w.Write([]byte(`<response><songs>` +
			`<song><lyricsId>` + wantID + `</lyricsId><title>Lorem Ipsum (Live)</title>` +
			`<lyricsType>2</lyricsType><lyricsData>` +
			base64.StdEncoding.EncodeToString(blob) +
			`</lyricsData></song></songs></response>`))
	})

	song, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"})
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if len(song.Subtitles.Lines) != 3 {
		t.Fatalf("got %d cues, want 3", len(song.Subtitles.Lines))
	}
	if got := song.Subtitles.Lines[0].Text; got != "right1" {
		t.Errorf("cue[0] = %q, want %q -- the text lookup did not pin the tier-2 lyrics id", got, "right1")
	}
}

// TestLineSync_RefusesWhenTheLyricsIDIsAbsent pins the other half: if the
// tier-1 response does not carry the id the timings came from, there is no
// provably-matching text, so the lane must refuse rather than fall back to a
// scored guess.
func TestLineSync_RefusesWhenTheLyricsIDIsAbsent(t *testing.T) {
	blob := buildLSY(t, 0xdc18, true, []int{100, 250, 400})

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		if r.PostForm.Get("lyricsType") == "1" {
			_, _ = w.Write([]byte(`<response><songs>` +
				songXML("999", "Lorem Ipsum", []byte("a\nb\nc\n")) +
				`</songs></response>`))
			return
		}
		_, _ = w.Write([]byte(`<response><songs>` +
			`<song><lyricsId>222</lyricsId><title>Lorem Ipsum</title>` +
			`<lyricsType>2</lyricsType><lyricsData>` +
			base64.StdEncoding.EncodeToString(blob) +
			`</lyricsData></song></songs></response>`))
	})

	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "Lorem Ipsum"}); err == nil {
		t.Fatal("a missing lyrics id must refuse, not fall back to a scored candidate")
	}
}
