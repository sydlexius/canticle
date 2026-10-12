package scanner

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// Fixture builders for the external scanner_test package. A test that also
// imports internal/audiometa cannot live in package scanner (audiometa imports
// scanner), and testutil's FLAC writer takes a map, which cannot repeat a key.

// BuildFLACWithRawComments is buildFLACWithRawComments.
var BuildFLACWithRawComments = buildFLACWithRawComments

// BuildOggVorbisWithRawComments builds a two-page Ogg Vorbis stream carrying
// kv verbatim as comment entries, duplicates allowed.
func BuildOggVorbisWithRawComments(t *testing.T, kv []string) []byte {
	t.Helper()
	ident := []byte("\x01vorbisIDHEADERPAD")
	comment := append(append([]byte{}, vorbisCommentPrefix...), vorbisCommentPayload(t, "vendor", kv)...)
	var s bytes.Buffer
	s.Write(buildOggPage(1, 0, false, segmentTableFor(len(ident)), ident))
	s.Write(buildOggPage(1, 1, false, segmentTableFor(len(comment)), comment))
	return s.Bytes()
}

// MP4 atom builders: BuildM4A assembles a file from tag atoms, TagAtom makes a
// text atom with one data child per value, FreeformAtom an iTunes "----" atom.
var (
	BuildM4A     = buildM4A
	TagAtom      = tagAtom
	FreeformAtom = freeformAtom
)

// SetAudioOpener replaces the opener the scan uses for an audio file, so a
// test can count opens or change the file between the lookup stat and the read.
func SetAudioOpener(sc *Scanner, open func(string) (*os.File, error)) { sc.openAudio = open }

// SetDurationProbe replaces the scan's duration probe.
func SetDurationProbe(sc *Scanner, probe func(string) (int, error)) { sc.probeFunc = probe }

// SetVorbisReader wraps the handle the scan's Vorbis multi-value re-read uses.
func SetVorbisReader(sc *Scanner, wrap func(*os.File) io.ReadSeeker) { sc.vorbisReader = wrap }
