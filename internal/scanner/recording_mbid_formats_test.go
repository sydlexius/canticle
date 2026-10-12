package scanner

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhowden/tag"
	"github.com/sydlexius/canticle/internal/testutil"
)

// Fixed, made-up UUIDs. The recording and per-release track IDs differ on
// purpose so a test can tell which one was returned (#1520).
const (
	fmtRecordingMBID    = "11111111-2222-4333-8444-555555555555"
	fmtReleaseTrackMBID = "66666666-7777-4888-9999-000000000000"
	// fmtLetterMBID has hex letters, so upper-casing it changes the string.
	fmtLetterMBID = "abcdef12-3456-4789-abcd-ef0123456789"
)

func readTagsFromBytes(t *testing.T, buf []byte) tag.Metadata {
	t.Helper()
	m, err := tag.ReadFrom(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tag.ReadFrom: %v", err)
	}
	return m
}

// oggStream builds a two-page Ogg stream whose second packet is the comment
// packet, introduced by prefix ("\x03vorbis" for Vorbis, "OpusTags" for Opus).
func oggStream(t *testing.T, prefix []byte, kv []string) []byte {
	t.Helper()
	ident := []byte("\x01vorbisIDHEADERPAD")
	comment := append(append([]byte{}, prefix...), vorbisCommentPayload(t, "vendor", kv)...)
	var s bytes.Buffer
	s.Write(buildOggPage(1, 0, false, segmentTableFor(len(ident)), ident))
	s.Write(buildOggPage(1, 1, false, segmentTableFor(len(comment)), comment))
	return s.Bytes()
}

// freeformAtom builds an iTunes "----" atom (mean + name + data), the shape
// Picard uses for MusicBrainz IDs in MP4.
func freeformAtom(name, value string) []byte {
	var p []byte
	p = append(p, box("mean", append([]byte{0, 0, 0, 0}, "com.apple.iTunes"...))...)
	p = append(p, box("name", append([]byte{0, 0, 0, 0}, name...))...)
	p = append(p, dataAtom(value)...)
	return box("----", p)
}

// freeformAtomMulti is freeformAtom with one data child per value. dhowden/tag
// joins them as "\x00\x00\x00\x00id1;\x00\x00\x00\x00id2" (readAtomData).
func freeformAtomMulti(name string, values ...string) []byte {
	var p []byte
	p = append(p, box("mean", append([]byte{0, 0, 0, 0}, "com.apple.iTunes"...))...)
	p = append(p, box("name", append([]byte{0, 0, 0, 0}, name...))...)
	for _, v := range values {
		p = append(p, dataAtom(v)...)
	}
	return box("----", p)
}

// id3UFIDFile builds a minimal ID3v2.4 tag holding one UFID frame owned by the
// MusicBrainz provider URL, with the audio-less body the tag reader accepts.
func id3UFIDFile(owner, id string) []byte {
	payload := append(append([]byte(owner), 0), id...)
	n := len(payload)
	frame := append([]byte("UFID"), byte(n>>21&0x7f), byte(n>>14&0x7f), byte(n>>7&0x7f), byte(n&0x7f), 0, 0)
	frame = append(frame, payload...)
	m := len(frame)
	hdr := []byte{'I', 'D', '3', 4, 0, 0, byte(m >> 21 & 0x7f), byte(m >> 14 & 0x7f), byte(m >> 7 & 0x7f), byte(m & 0x7f)}
	return append(hdr, frame...)
}

func TestExtractRecordingMBID_PerFormat(t *testing.T) {
	flac := func(comments map[string]string) []byte { return testutil.GenerateFLACExtended(44100, 44100, comments) }
	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{"flac MUSICBRAINZ_TRACKID only", flac(map[string]string{"MUSICBRAINZ_TRACKID": fmtRecordingMBID}), fmtRecordingMBID},
		{"flac both ids: recording wins", flac(map[string]string{
			"MUSICBRAINZ_TRACKID":        fmtRecordingMBID,
			"MUSICBRAINZ_RELEASETRACKID": fmtReleaseTrackMBID,
		}), fmtRecordingMBID},
		{"flac release track id only is not a recording id", flac(map[string]string{"MUSICBRAINZ_RELEASETRACKID": fmtReleaseTrackMBID}), ""},
		{"flac explicit recording id wins over track id", flac(map[string]string{
			"MUSICBRAINZ_RECORDINGID": fmtRecordingMBID,
			"MUSICBRAINZ_TRACKID":     fmtReleaseTrackMBID,
		}), fmtRecordingMBID},
		{"flac no musicbrainz tags", flac(map[string]string{"TITLE": "Quiet Harbor"}), ""},
		{"ogg vorbis MUSICBRAINZ_TRACKID", oggStream(t, vorbisCommentPrefix, []string{
			"MUSICBRAINZ_TRACKID=" + fmtRecordingMBID, "MUSICBRAINZ_RELEASETRACKID=" + fmtReleaseTrackMBID}), fmtRecordingMBID},
		{"ogg vorbis release track id only", oggStream(t, vorbisCommentPrefix, []string{
			"MUSICBRAINZ_RELEASETRACKID=" + fmtReleaseTrackMBID}), ""},
		{"opus MUSICBRAINZ_TRACKID", oggStream(t, opusTagsPrefix, []string{
			"MUSICBRAINZ_TRACKID=" + fmtRecordingMBID, "MUSICBRAINZ_RELEASETRACKID=" + fmtReleaseTrackMBID}), fmtRecordingMBID},
		{"opus release track id only", oggStream(t, opusTagsPrefix, []string{
			"MUSICBRAINZ_RELEASETRACKID=" + fmtReleaseTrackMBID}), ""},
		{"mp3 TXXX", testutil.GenerateID3v2Extended("Alpha", "Title", "Album", "", nil,
			map[string]string{"MusicBrainz Track Id": fmtRecordingMBID}), fmtRecordingMBID},
		{"mp3 UFID", id3UFIDFile("http://musicbrainz.org", fmtRecordingMBID), fmtRecordingMBID},
		{"mp3 release track id only", testutil.GenerateID3v2Extended("Alpha", "Title", "Album", "", nil,
			map[string]string{"MusicBrainz Release Track Id": fmtReleaseTrackMBID}), ""},
		{"mp3 no musicbrainz tags", testutil.GenerateID3v2("Alpha", "Title", "Album", ""), ""},
		{"m4a freeform Track Id", buildM4A(freeformAtom("MusicBrainz Track Id", fmtRecordingMBID),
			freeformAtom("MusicBrainz Release Track Id", fmtReleaseTrackMBID)), fmtRecordingMBID},
		{"flac upper-case id is lowercased", flac(map[string]string{"MUSICBRAINZ_TRACKID": strings.ToUpper(fmtLetterMBID)}), fmtLetterMBID},
		{"flac padded id is trimmed", flac(map[string]string{"MUSICBRAINZ_TRACKID": "  " + fmtLetterMBID + " \t"}), fmtLetterMBID},
		{"mp3 upper-case padded id", testutil.GenerateID3v2Extended("Alpha", "Title", "Album", "", nil,
			map[string]string{"MusicBrainz Track Id": " " + strings.ToUpper(fmtLetterMBID) + " "}), fmtLetterMBID},
		{"m4a upper-case id", buildM4A(freeformAtom("MusicBrainz Track Id", strings.ToUpper(fmtLetterMBID))), fmtLetterMBID},
		{"m4a multi-value returns the first, clean", buildM4A(freeformAtomMulti("MusicBrainz Track Id",
			fmtRecordingMBID, fmtReleaseTrackMBID)), fmtRecordingMBID},
		{"m4a release track id only", buildM4A(freeformAtom("MusicBrainz Release Track Id", fmtReleaseTrackMBID)), ""},
		{"m4a no musicbrainz tags", buildM4A(tagAtom("\xa9nam", "Quiet Harbor")), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractRecordingMBID(readTagsFromBytes(t, tc.buf)); got != tc.want {
				t.Errorf("extractRecordingMBID = %q; want %q", got, tc.want)
			}
		})
	}
}

// End to end through the scan loop: a FLAC file carrying Picard's
// MUSICBRAINZ_TRACKID lands its recording ID on the scan row (#1520).
func TestScanLibrary_FLACRecordingMBID(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "track.flac", 44100, 44100, map[string]string{
		"TITLE":                      "Quiet Harbor",
		"MUSICBRAINZ_TRACKID":        fmtRecordingMBID,
		"MUSICBRAINZ_RELEASETRACKID": fmtReleaseTrackMBID,
	}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	results, err := skipDurationScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1, EnrichRecording: true})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results; want 1", len(results))
	}
	if got := results[0].Track.RecordingMBID; got != fmtRecordingMBID {
		t.Errorf("Track.RecordingMBID = %q; want %q", got, fmtRecordingMBID)
	}
	facts, err := ReadAudioFacts(filepath.Join(dir, "track.flac"))
	if err != nil {
		t.Fatalf("ReadAudioFacts: %v", err)
	}
	if facts.MBID != fmtRecordingMBID {
		t.Errorf("AudioFacts.MBID = %q; want %q", facts.MBID, fmtRecordingMBID)
	}
}
