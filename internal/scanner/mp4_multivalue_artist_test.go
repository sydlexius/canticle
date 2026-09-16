package scanner

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhowden/tag"
	"github.com/sydlexius/canticle/internal/testutil"
)

// --- Synthetic MP4 fixture helpers -----------------------------------------
//
// github.com/dhowden/tag's MP4 parser only needs enough of the box structure
// to find the tag atoms it recognizes; these helpers build the minimal
// skeleton from issue #958's own reproduction:
//
//	ftyp + moov(udta(meta(4 zero bytes + ilst(<tag atoms>))))
//
// box writes a big-endian uint32 length (including the 8-byte header itself)
// followed by the 4-byte atom name and the payload. dataAtom wraps a string
// in a "data" box with class 1 (text) and an empty (4-byte zero) locale,
// matching what Picard and similar taggers write for a text atom.

func box(name string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload))) //nolint:gosec // reason: test fixture payloads are small, no overflow risk
	copy(b[4:8], name)
	copy(b[8:], payload)
	return b
}

func dataAtom(s string) []byte {
	// 4 bytes version(1)+flags(3, class=1 text), 4 bytes locale (0).
	payload := append([]byte{0, 0, 0, 1, 0, 0, 0, 0}, []byte(s)...)
	return box("data", payload)
}

// tagAtom builds a tag atom (e.g. "\xa9ART" / "aART") carrying one data
// child per value. Two or more values reproduces the #958 corruption: the
// dependency's parser strips only the first data child's header, splicing
// the raw bytes of every subsequent child's header between the values.
func tagAtom(name string, values ...string) []byte {
	var payload []byte
	for _, v := range values {
		payload = append(payload, dataAtom(v)...)
	}
	return box(name, payload)
}

// buildM4A assembles a minimal parseable M4A file carrying the given ilst
// tag atoms.
func buildM4A(tagAtoms ...[]byte) []byte {
	var ilstPayload []byte
	for _, a := range tagAtoms {
		ilstPayload = append(ilstPayload, a...)
	}
	ilst := box("ilst", ilstPayload)
	meta := box("meta", append([]byte{0, 0, 0, 0}, ilst...))
	udta := box("udta", meta)
	moov := box("moov", udta)
	ftyp := box("ftyp", []byte("M4A \x00\x00\x00\x00M4A mp42isom"))
	return append(ftyp, moov...)
}

func readMP4(t *testing.T, buf []byte) tag.Metadata {
	t.Helper()
	m, err := tag.ReadFrom(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tag.ReadFrom: %v", err)
	}
	if m.Format() != tag.MP4 {
		t.Fatalf("Format() = %v; want %v (fixture did not parse as MP4)", m.Format(), tag.MP4)
	}
	return m
}

// A multi-value ©ART atom (two data children) is spliced by dhowden/tag into
// a single corrupted string. The scanner must recover both discrete values
// and join them with artistValueSep -- this is the MP4 sibling of #466,
// verified end to end through ScanLibrary against a real .m4a file.
func TestScanArtist_MP4MultiValueRecovered(t *testing.T) {
	buf := buildM4A(tagAtom("\xa9ART", "First Artist", "Second Artist"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "track.m4a"), buf, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got, want := results[0].Track.ArtistName, "First Artist; Second Artist"; got != want {
		t.Errorf("ArtistName = %q; want %q", got, want)
	}
}

// The corruption repeats per extra data child; three values must all be
// recovered, not just the first two.
func TestExtractArtist_MP4ThreeValues(t *testing.T) {
	buf := buildM4A(tagAtom("\xa9ART", "Alpha", "Bravo", "Charlie"))
	m := readMP4(t, buf)
	if got, want := extractArtist(m, nil), "Alpha; Bravo; Charlie"; got != want {
		t.Errorf("extractArtist = %q; want %q", got, want)
	}
}

// A single-value atom never triggers the splice (only one data child, so
// dhowden/tag reads it correctly); extractArtist must fall through to the
// standard accessor unchanged.
func TestExtractArtist_MP4SingleValueUnchanged(t *testing.T) {
	buf := buildM4A(tagAtom("\xa9ART", "Solo Artist"))
	m := readMP4(t, buf)
	if got, want := extractArtist(m, nil), "Solo Artist"; got != want {
		t.Errorf("extractArtist = %q; want %q", got, want)
	}
}

// The album-artist atom (aART) mirrors the artist atom.
func TestExtractAlbumArtist_MP4MultiValueRecovered(t *testing.T) {
	buf := buildM4A(tagAtom("aART", "Alpha", "Bravo"))
	m := readMP4(t, buf)
	if got, want := extractAlbumArtist(m, nil), "Alpha; Bravo"; got != want {
		t.Errorf("extractAlbumArtist = %q; want %q", got, want)
	}
}

// A legitimate single value containing a literal "$" (the byte that renders
// as the corruption's visible signature, 0x24 being the low byte of a box
// length) must not be mistaken for a splice.
func TestExtractArtist_MP4ValueContainingDollarSign(t *testing.T) {
	buf := buildM4A(tagAtom("\xa9ART", "Ke$ha"))
	m := readMP4(t, buf)
	if got, want := extractArtist(m, nil), "Ke$ha"; got != want {
		t.Errorf("extractArtist = %q; want %q", got, want)
	}
}

// A legitimate single value that happens to contain the literal 4-byte
// sequence "data" (lowercase, the actual box-name token the splice
// detector looks for) exercises the length-prefix validation: the bytes
// immediately preceding "data" in "metadata artist" are "meta", which read
// as a uint32 length far larger than the buffer, so the chain-parse must
// fail and the value must pass through unmangled.
func TestExtractArtist_MP4ValueContainingDataSubstring(t *testing.T) {
	buf := buildM4A(tagAtom("\xa9ART", "metadata artist"))
	m := readMP4(t, buf)
	if got, want := extractArtist(m, nil), "metadata artist"; got != want {
		t.Errorf("extractArtist = %q; want %q", got, want)
	}
}

// Regression guard for #466: the MP4 recovery path must be strictly gated on
// m.Format() == tag.MP4 so an ID3 file's raw atoms (which don't exist) are
// never consulted, and the pre-existing ID3 TXXX ARTISTS recovery is
// untouched.
func TestMultiValueMP4Tag_GatedOnFormat(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteAudioFile(dir, "track.mp3", "Solo Artist", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, "track.mp3")) //nolint:gosec // reason: test-owned tempdir path
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	m, err := tag.ReadFrom(f)
	if err != nil {
		t.Fatalf("tag.ReadFrom: %v", err)
	}
	if m.Format() == tag.MP4 {
		t.Fatalf("fixture parsed as MP4; want an ID3 format")
	}
	// Assert the OK FLAG, not the string. An empty string is now a legitimate
	// success (an atom whose children are all empty), so checking got != ""
	// would pass here whether or not the format gate actually held.
	if got, ok := multiValueMP4Tag(m, mp4ArtistAtoms); ok {
		t.Errorf("multiValueMP4Tag on an ID3 file returned ok with %q; want ok=false (format gate)", got)
	}
}

// A tagger that terminates the atom with a trailing EMPTY data child yields
// two data children but only one non-empty value. The splice signature still
// validates end to end, so m.Artist() is still corrupt -- it carries the
// second child's raw header bytes -- and the recovery must still fire.
//
// This is the regression guard on the gate itself. An earlier revision gated
// the return on the count of SURVIVING (post-trim) values being >= 2, copying
// multiValueTag's ID3 rule. That rule is wrong here: it dropped this file back
// through to the mangled accessor, which is the exact defect #958 is about.
// The correct gate is that the splice VALIDATED, which proves the raw value
// is unusable regardless of how many values survive the trim.
func TestExtractArtist_MP4TrailingEmptyValue(t *testing.T) {
	m := readMP4(t, buildM4A(tagAtom("\xa9ART", "Only One", "")))

	// Precondition: the raw accessor really is corrupt here. Without this the
	// test could pass against a build where the fixture stopped reproducing
	// the defect at all.
	if raw := m.Artist(); raw == "Only One" {
		t.Fatalf("fixture no longer reproduces the splice: Artist() = %q", raw)
	}

	if got, want := extractArtist(m, nil), "Only One"; got != want {
		t.Errorf("extractArtist() = %q, want %q (raw = %q)", got, want, m.Artist())
	}
}

// Five values exercise the chain loop past the two- and three-value cases the
// other tests cover, confirming completeMP4SpliceChain walks an arbitrary
// number of children rather than a hardcoded few.
func TestExtractArtist_MP4FiveValues(t *testing.T) {
	m := readMP4(t, buildM4A(tagAtom("\xa9ART", "V1", "V2", "V3", "V4", "V5")))

	if got, want := extractArtist(m, nil), "V1; V2; V3; V4; V5"; got != want {
		t.Errorf("extractArtist() = %q, want %q", got, want)
	}
}

// The lowercase "\xa9art" atom is in mp4ArtistAtoms because the dependency's
// atom table maps it to the artist meaning alongside "\xa9ART". Without this
// test that entry is asserted only by reading the dependency's source.
func TestExtractArtist_MP4LowercaseAtom(t *testing.T) {
	m := readMP4(t, buildM4A(tagAtom("\xa9art", "Low One", "Low Two")))

	if got, want := extractArtist(m, nil), "Low One; Low Two"; got != want {
		t.Errorf("extractArtist() = %q, want %q", got, want)
	}
}

// Multi-byte UTF-8 values confirm the recovery slices on the byte offsets the
// box lengths describe, never on runes: the length prefix counts bytes, so a
// rune-based split would truncate mid-character here.
func TestExtractArtist_MP4UnicodeValues(t *testing.T) {
	m := readMP4(t, buildM4A(tagAtom("\xa9ART", "Ólafur", "坂本")))

	if got, want := extractArtist(m, nil), "Ólafur; 坂本"; got != want {
		t.Errorf("extractArtist() = %q, want %q", got, want)
	}
}

// An atom whose data children are ALL empty (or whitespace-only) validates as
// a splice but recovers no surviving values. The correct artist there is the
// empty string -- an empty tag -- and the recovery must still report success,
// because the raw accessor holds the spliced header bytes and is strictly
// worse than "".
//
// This is the regression guard on multiValueMP4Tag returning success as its
// OWN value rather than as "the joined string is non-empty" (CodeRabbit,
// #958). Under the collapsed form these files fell through to the mangled
// accessor, which is the defect this file exists to fix.
func TestExtractArtist_MP4AllEmptyChildren(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"both empty", []string{"", ""}},
		{"whitespace only", []string{"  ", "\t"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := readMP4(t, buildM4A(tagAtom("\xa9ART", tc.values...)))

			// Precondition: the raw accessor really is corrupt, so this test
			// cannot pass vacuously against a build where the fixture stopped
			// reproducing the splice.
			if raw := m.Artist(); raw == "" {
				t.Fatalf("fixture no longer reproduces the splice: Artist() = %q", raw)
			}

			if got := extractArtist(m, nil); got != "" {
				t.Errorf("extractArtist() = %q, want %q (raw = %q)", got, "", m.Artist())
			}
		})
	}
}

// splicedValue builds the byte shape a spliced header has: a 4-byte big-endian
// length, the "data" literal, then the 8-byte class+locale block, then payload.
// classLocale is a parameter so a test can pin what happens when those 8 bytes
// do NOT carry the text-class signature.
func splicedValue(prefix string, classLocale []byte, payload string) string {
	b := []byte(prefix)
	b = binary.BigEndian.AppendUint32(b, uint32(mp4SpliceHeaderLen+len(payload))) //nolint:gosec // reason: test payloads are small, no overflow risk
	b = append(b, "data"...)
	b = append(b, classLocale...)
	return string(append(b, payload...))
}

// A legitimate SINGLE value can be shaped like a chain: a length prefix, the
// "data" literal, 8 bytes, and a payload whose length lands EXACTLY at
// end-of-buffer. Length accounting alone cannot tell that apart from a real
// splice -- it partitions cleanly either way -- so such a value was split into
// two artists that the file never had.
//
// Reported by Copilot on PR #959, and it was right that the earlier
// "metadata artist" test did not cover this: that one fails because its length
// prefix is ABSURD, not because the structure is rejected. The discriminator is
// the class+locale block, which is mostly NUL and which real text does not
// carry. Here those 8 bytes are printable, so the split is refused and the
// value passes through whole.
func TestExtractArtist_MP4ChainShapedSingleValue(t *testing.T) {
	value := splicedValue("Prefix Artist", []byte("PRINTABL"), "Tail Words")
	m := readMP4(t, buildM4A(tagAtom("\xa9ART", value)))

	// Precondition: the length really does land at EOF, so this test cannot
	// pass merely because the accounting failed.
	if got, want := m.Artist(), value; got != want {
		t.Fatalf("fixture mangled before the code under test: Artist() = %q, want %q", got, want)
	}

	if got := extractArtist(m, nil); got != value {
		t.Errorf("a chain-shaped SINGLE value was split: extractArtist() = %q, want %q", got, value)
	}
}

// The same shape carrying the REAL text class+locale block is a genuine splice
// and must still be recovered -- the discriminator has to reject the impostor
// above without also rejecting the thing it is modeled on.
func TestExtractArtist_MP4ChainShapedGenuineSplice(t *testing.T) {
	value := splicedValue("First One", mp4TextClassLocale[:], "Second One")
	m := readMP4(t, buildM4A(tagAtom("\xa9ART", value)))

	if got, want := extractArtist(m, nil), "First One; Second One"; got != want {
		t.Errorf("extractArtist() = %q, want %q", got, want)
	}
}

// An oversized length must be rejected BEFORE it is narrowed to int. On a
// 32-bit build int is 32 bits, so 0x80000010 narrows negative, valLen goes
// negative, the bounds guard compares as less-than and PASSES, and the slice
// panics with high < low (verified by narrowing simulation: int32(0x80000010)
// = -2147483632).
//
// Reported by CodeRabbit on PR #959. No shipped build reaches it -- CI and
// GoReleaser target amd64/arm64 only -- so this is a latent defect gated on a
// build-config fact that can change, which is exactly the kind that survives
// until the config changes.
//
// HONEST LIMIT OF THIS TEST: on a 64-bit run it does NOT discriminate. int is
// 64 bits there, 0x80000010 does not overflow, and the pre-fix narrow-then-check
// order rejects the length too -- measured, the mutation passes on amd64. It
// would redden on 386, which cannot be executed on the darwin/arm64 dev machine
// (the binary cross-compiles; there is no runner). So this is a REGRESSION GUARD
// documenting the shape, not proof the fix works. The proof is that the
// comparison happens in uint64 before any narrowing, which is word-size
// independent by construction.
func TestExtractArtist_MP4OversizedLengthRejected(t *testing.T) {
	var big [4]byte
	binary.BigEndian.PutUint32(big[:], 0x80000010)

	raw := []byte("Legit Artist")
	raw = append(raw, big[:]...)
	raw = append(raw, "data"...)
	raw = append(raw, mp4TextClassLocale[:]...)
	raw = append(raw, "Trailing"...)
	value := string(raw)

	m := readMP4(t, buildM4A(tagAtom("\xa9ART", value)))

	// Must not panic, and must not split on a length the buffer cannot back.
	if got := extractArtist(m, nil); got != value {
		t.Errorf("extractArtist() = %q, want the unsplit value %q", got, value)
	}
}
