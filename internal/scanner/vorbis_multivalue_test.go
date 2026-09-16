package scanner

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/dhowden/tag"
)

// --- Byte-level FLAC fixture builders (issue #969) ---------------------
//
// testutil.GenerateFLACExtended takes a map[string]string and therefore
// cannot express a REPEATED Vorbis-comment key, which is exactly the shape
// this issue is about. These helpers build the FLAC bytes directly, one
// raw "KEY=value" string per comment, duplicates allowed, mirroring
// testutil's block layout (see internal/testutil/id3.go) but taking a slice
// instead of a map.

func vorbisCommentPayload(t *testing.T, vendor string, kv []string) []byte {
	t.Helper()
	var b bytes.Buffer
	writeLE32 := func(n int) {
		var u [4]byte
		binary.LittleEndian.PutUint32(u[:], uint32(n)) //nolint:gosec // reason: test fixture, sizes are small and non-negative
		b.Write(u[:])
	}
	writeLE32(len(vendor))
	b.WriteString(vendor)
	writeLE32(len(kv))
	for _, c := range kv {
		writeLE32(len(c))
		b.WriteString(c)
	}
	return b.Bytes()
}

func flacStreamInfoBlockForTest(sampleRate, totalSamples uint32, last bool) []byte {
	var b bytes.Buffer
	hdr := byte(0x00)
	if last {
		hdr |= 0x80
	}
	b.WriteByte(hdr)
	b.Write([]byte{0x00, 0x00, 0x22}) // 24-bit length = 34 bytes
	var si [34]byte
	si[0], si[2] = 0x10, 0x10
	pack := (uint64(sampleRate) << 44) | (uint64(15) << 36) | (uint64(totalSamples) & 0xFFFFFFFFF)
	binary.BigEndian.PutUint64(si[10:18], pack)
	b.Write(si[:])
	return b.Bytes()
}

// flacTestSampleRate and flacTestTotalSamples describe every FLAC fixture in
// this file (a fixed 30-second track): neither the field-recovery logic nor
// the duration-preservation assertion needs them to vary, so they are
// constants rather than parameters of the builder below.
const (
	flacTestSampleRate   = 44100
	flacTestTotalSamples = flacTestSampleRate * 30
)

// buildFLACWithRawComments builds a minimal header-only FLAC file (STREAMINFO
// + one VORBIS_COMMENT block) carrying kv verbatim as "KEY=value" comment
// entries, duplicates allowed.
func buildFLACWithRawComments(t *testing.T, kv []string) []byte {
	t.Helper()
	payload := vorbisCommentPayload(t, "canticle-test", kv)
	var b bytes.Buffer
	b.WriteString("fLaC")
	b.Write(flacStreamInfoBlockForTest(flacTestSampleRate, flacTestTotalSamples, false))
	b.WriteByte(0x84) // last-block flag | type 4 (VORBIS_COMMENT)
	n := len(payload)
	b.Write([]byte{byte(n >> 16), byte(n >> 8), byte(n)}) //nolint:gosec // reason: test fixture, payload is far below the 24-bit block length limit
	b.Write(payload)
	return b.Bytes()
}

// --- Direct parser tests -------------------------------------------------

func TestParseVorbisCommentFields_Basic(t *testing.T) {
	payload := vorbisCommentPayload(t, "vendor", []string{"ARTIST=Solo Artist", "TITLE=A Song"})
	fields, ok := parseVorbisCommentFields(payload)
	if !ok {
		t.Fatal("parseVorbisCommentFields() ok = false, want true")
	}
	if got := fields["artist"]; len(got) != 1 || got[0] != "Solo Artist" {
		t.Errorf("artist = %v, want [Solo Artist]", got)
	}
	if got := fields["title"]; len(got) != 1 || got[0] != "A Song" {
		t.Errorf("title = %v, want [A Song]", got)
	}
}

func TestParseVorbisCommentFields_RepeatedKeyPreserved(t *testing.T) {
	payload := vorbisCommentPayload(t, "vendor", []string{"ARTIST=Artist One", "ARTIST=Artist Two"})
	fields, ok := parseVorbisCommentFields(payload)
	if !ok {
		t.Fatal("parseVorbisCommentFields() ok = false, want true")
	}
	want := []string{"Artist One", "Artist Two"}
	got := fields["artist"]
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("artist = %v, want %v (order must be preserved)", got, want)
	}
}

func TestParseVorbisCommentFields_CaseInsensitiveKey(t *testing.T) {
	payload := vorbisCommentPayload(t, "vendor", []string{"Artist=Value One", "ARTIST=Value Two", "artist=Value Three"})
	fields, ok := parseVorbisCommentFields(payload)
	if !ok {
		t.Fatal("parseVorbisCommentFields() ok = false, want true")
	}
	if got := fields["artist"]; len(got) != 3 {
		t.Errorf("artist = %v, want 3 values regardless of key case", got)
	}
}

func TestParseVorbisCommentFields_Hostile(t *testing.T) {
	t.Run("truncated before vendor length", func(t *testing.T) {
		if _, ok := parseVorbisCommentFields([]byte{0x01, 0x02}); ok {
			t.Error("ok = true on a 2-byte input, want false")
		}
	})

	t.Run("vendor length overruns buffer", func(t *testing.T) {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint32(data[0:4], 0xFFFFFFFF) // vendor length "4 billion"
		binary.LittleEndian.PutUint32(data[4:8], 0)          // comments length, irrelevant
		if _, ok := parseVorbisCommentFields(data); ok {
			t.Error("ok = true with an out-of-range vendor length, want false")
		}
	})

	t.Run("comment length overruns buffer", func(t *testing.T) {
		var b bytes.Buffer
		var u [4]byte
		binary.LittleEndian.PutUint32(u[:], 0) // empty vendor
		b.Write(u[:])
		binary.LittleEndian.PutUint32(u[:], 1) // one comment
		b.Write(u[:])
		binary.LittleEndian.PutUint32(u[:], 0xFFFFFFFF) // its length: huge
		b.Write(u[:])
		if _, ok := parseVorbisCommentFields(b.Bytes()); ok {
			t.Error("ok = true with an out-of-range comment length, want false")
		}
	})

	t.Run("huge declared comment count never loops or allocates", func(t *testing.T) {
		var b bytes.Buffer
		var u [4]byte
		binary.LittleEndian.PutUint32(u[:], 0) // empty vendor
		b.Write(u[:])
		binary.LittleEndian.PutUint32(u[:], 0xFFFFFFFF) // "4 billion" comments declared
		b.Write(u[:])
		// No comment data follows at all -- if the count were trusted, the
		// first per-comment length read would already be out of bounds; the
		// cap must reject before that read is even attempted.
		if _, ok := parseVorbisCommentFields(b.Bytes()); ok {
			t.Error("ok = true with a comment count above the cap, want false")
		}
	})
}

func TestFLACVorbisCommentFields_Hostile(t *testing.T) {
	t.Run("bad magic", func(t *testing.T) {
		if _, ok := flacVorbisCommentFields(bytes.NewReader([]byte("not-a-flac-file"))); ok {
			t.Error("ok = true on bad magic, want false")
		}
	})

	t.Run("truncated after magic", func(t *testing.T) {
		if _, ok := flacVorbisCommentFields(bytes.NewReader([]byte("fLaC"))); ok {
			t.Error("ok = true on a file with no metadata blocks, want false")
		}
	})

	t.Run("declared block length exceeds cap", func(t *testing.T) {
		var b bytes.Buffer
		b.WriteString("fLaC")
		b.WriteByte(0x84)                 // last | type 4
		b.Write([]byte{0xFF, 0xFF, 0xFF}) // 24-bit length = 16,777,215, over the 1 MiB cap
		if _, ok := flacVorbisCommentFields(bytes.NewReader(b.Bytes())); ok {
			t.Error("ok = true with a block length over the cap, want false")
		}
	})

	t.Run("declared block length exceeds actual data", func(t *testing.T) {
		var b bytes.Buffer
		b.WriteString("fLaC")
		b.WriteByte(0x84)                 // last | type 4
		b.Write([]byte{0x00, 0x10, 0x00}) // 24-bit length = 4096, no payload follows
		if _, ok := flacVorbisCommentFields(bytes.NewReader(b.Bytes())); ok {
			t.Error("ok = true with a block length overrunning the actual data, want false")
		}
	})
}

// --- multiValueVorbisField precedence tests ------------------------------

func TestMultiValueVorbisField_Precedence(t *testing.T) {
	t.Run("ARTISTS wins over ARTIST when both are multi-value", func(t *testing.T) {
		fields := map[string][]string{
			"artists": {"Artists Field One", "Artists Field Two"},
			"artist":  {"Artist Field One", "Artist Field Two"},
		}
		got, ok := multiValueVorbisField(fields, "artists", "artist")
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if want := "Artists Field One; Artists Field Two"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("falls through to ARTIST when ARTISTS is single-value", func(t *testing.T) {
		fields := map[string][]string{
			"artists": {"Only One"},
			"artist":  {"Artist One", "Artist Two"},
		}
		got, ok := multiValueVorbisField(fields, "artists", "artist")
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if want := "Artist One; Artist Two"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("single value never triggers a join", func(t *testing.T) {
		fields := map[string][]string{"artist": {"Solo Artist"}}
		if _, ok := multiValueVorbisField(fields, "artists", "artist"); ok {
			t.Error("ok = true for a single-value field, want false (fall through untouched)")
		}
	})

	t.Run("repeated field with only empty values falls through", func(t *testing.T) {
		fields := map[string][]string{"artist": {"", "   "}}
		if got, ok := multiValueVorbisField(fields, "artists", "artist"); ok {
			t.Errorf("multiValueVorbisField() = (%q, true); want ok=false when no repeated value is non-empty", got)
		}
	})

	t.Run("empty and whitespace-only values are dropped from the join", func(t *testing.T) {
		fields := map[string][]string{"artist": {"Artist One", "   ", "Artist Two", ""}}
		got, ok := multiValueVorbisField(fields, "artists", "artist")
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if want := "Artist One; Artist Two"; got != want {
			t.Errorf("got %q, want %q (blank duplicates must be dropped)", got, want)
		}
	})

	t.Run("nil fields map is safe and always misses", func(t *testing.T) {
		if _, ok := multiValueVorbisField(nil, "artists", "artist"); ok {
			t.Error("ok = true on a nil map, want false")
		}
	})
}

// --- End-to-end tests through extractArtist/extractAlbumArtist and
// ReadAudioFacts, over a real dhowden/tag.Metadata parsed from built FLAC
// bytes. -------------------------------------------------------------------

func TestExtractArtist_FLAC_TwoArtistValues_Joined(t *testing.T) {
	data := buildFLACWithRawComments(t, []string{
		"ARTIST=Artist One", "ARTIST=Artist Two",
	})
	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("tag.ReadFrom() error = %v", err)
	}

	// This is the pre-#969 behavior, unchanged: m.Artist() -- what old
	// extractArtist(m) returned for a Vorbis-comment file, since neither
	// multiValueTag (ID3 TXXX) nor multiValueMP4Tag (MP4 atoms) ever matches
	// a VORBIS-format file. It reads only the LAST value, confirming the bug
	// this issue reports is real on this fixture before the recovery is
	// wired in.
	if got, want := m.Artist(), "Artist Two"; got != want {
		t.Fatalf("m.Artist() = %q, want %q (this is the mangled pre-fix value the fixture must reproduce)", got, want)
	}

	fields := vorbisMultiValueFields(bytes.NewReader(data), m)
	if got, want := extractArtist(m, fields), "Artist One; Artist Two"; got != want {
		t.Errorf("extractArtist() = %q, want %q", got, want)
	}
}

func TestExtractArtist_FLAC_ARTISTSWinsOverARTIST(t *testing.T) {
	data := buildFLACWithRawComments(t, []string{
		"ARTISTS=Artists One", "ARTISTS=Artists Two",
		"ARTIST=Artist One", "ARTIST=Artist Two",
	})
	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("tag.ReadFrom() error = %v", err)
	}
	fields := vorbisMultiValueFields(bytes.NewReader(data), m)
	if got, want := extractArtist(m, fields), "Artists One; Artists Two"; got != want {
		t.Errorf("extractArtist() = %q, want %q (plural field must win)", got, want)
	}
}

func TestExtractArtist_FLAC_SingleValueUnchanged(t *testing.T) {
	data := buildFLACWithRawComments(t, []string{"ARTIST=Solo Artist"})
	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("tag.ReadFrom() error = %v", err)
	}
	fields := vorbisMultiValueFields(bytes.NewReader(data), m)
	if got, want := extractArtist(m, fields), "Solo Artist"; got != want {
		t.Errorf("extractArtist() = %q, want %q (single value must pass through unchanged)", got, want)
	}
}

// A repeated field whose LAST value is empty is the case the dependency gets
// most wrong: its map keeps only that empty value, so falling back to
// m.Artist() returns "" and loses the one real artist. Once the field is
// genuinely repeated, a single surviving value is the correct answer.
func TestExtractArtist_FLAC_RepeatedWithEmptyLastValueKeepsSurvivor(t *testing.T) {
	data := buildFLACWithRawComments(t, []string{"ARTIST=Artist One", "ARTIST="})
	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("tag.ReadFrom() error = %v", err)
	}
	if m.Artist() != "" {
		t.Fatalf("fixture: dependency Artist() = %q; want the empty last value this test depends on", m.Artist())
	}
	fields := vorbisMultiValueFields(bytes.NewReader(data), m)
	if got, want := extractArtist(m, fields), "Artist One"; got != want {
		t.Errorf("extractArtist() = %q, want %q (the non-empty value of a repeated field must survive)", got, want)
	}
}

func TestExtractAlbumArtist_FLAC_TwoValues_Joined(t *testing.T) {
	data := buildFLACWithRawComments(t, []string{
		"ALBUMARTIST=Album Artist One", "ALBUMARTIST=Album Artist Two",
	})
	m, err := tag.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("tag.ReadFrom() error = %v", err)
	}
	fields := vorbisMultiValueFields(bytes.NewReader(data), m)
	if got, want := extractAlbumArtist(m, fields), "Album Artist One; Album Artist Two"; got != want {
		t.Errorf("extractAlbumArtist() = %q, want %q", got, want)
	}
}

// TestReadAudioFacts_FLAC_MultiArtist_DurationStillCorrect is the full
// end-to-end path: a real temp FLAC file on disk, read through
// scanner.ReadAudioFacts, asserting BOTH that the artist is recovered AND
// that TrackLength is still correctly parsed -- proving the extra seek this
// recovery performs on the shared handle does not disturb the existing
// duration parse.
func TestReadAudioFacts_FLAC_MultiArtist_DurationStillCorrect(t *testing.T) {
	data := buildFLACWithRawComments(t, []string{
		"ARTIST=Artist One", "ARTIST=Artist Two",
		"ALBUMARTIST=Album Artist One", "ALBUMARTIST=Album Artist Two",
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "track.flac")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	facts, err := ReadAudioFacts(path)
	if err != nil {
		t.Fatalf("ReadAudioFacts() error = %v", err)
	}
	if got, want := facts.Artist, "Artist One; Artist Two"; got != want {
		t.Errorf("Artist = %q, want %q", got, want)
	}
	if got, want := facts.AlbumArtist, "Album Artist One; Album Artist Two"; got != want {
		t.Errorf("AlbumArtist = %q, want %q", got, want)
	}
	if got, want := facts.TrackLength, 30; got != want {
		t.Errorf("TrackLength = %d, want %d (the Vorbis-comment reseek must not disturb duration parsing)", got, want)
	}
}

// TestVorbisMultiValueFields_NonVorbisFileTypeIsNoOp guards the file-type
// gate: an MP4/ID3 tag.Metadata must never be handed to the FLAC byte
// parser, which would misread arbitrary bytes as a Vorbis comment block.
func TestVorbisMultiValueFields_NonVorbisFileTypeIsNoOp(t *testing.T) {
	// A minimal ID3v1-only reader is enough to get a real tag.Metadata whose
	// FileType() is not FLAC.
	var raw [128]byte
	copy(raw[0:3], "TAG")
	m, err := tag.ReadFrom(bytes.NewReader(raw[:]))
	if err != nil {
		t.Fatalf("tag.ReadFrom() error = %v", err)
	}
	if got := vorbisMultiValueFields(bytes.NewReader(raw[:]), m); got != nil {
		t.Errorf("vorbisMultiValueFields() = %v, want nil for a non-Vorbis file type", got)
	}
}
