// Package testutil provides helpers for generating synthetic tagged audio files
// used by load/concurrency tests and the genlib tool. It encodes a minimal
// ID3v2.4 (UTF-8) tag in pure Go so no audio-encoding dependency is needed; the
// output is parseable by github.com/dhowden/tag.
package testutil

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// synchsafe encodes n as a 4-byte ID3v2 synchsafe integer (7 bits per byte).
func synchsafe(n int) []byte {
	return []byte{
		byte((n >> 21) & 0x7f),
		byte((n >> 14) & 0x7f),
		byte((n >> 7) & 0x7f),
		byte(n & 0x7f),
	}
}

// frame builds one ID3v2.4 frame: 4-byte id, synchsafe size, 2 flag bytes, payload.
func frame(id string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	b.Write(synchsafe(len(payload)))
	b.Write([]byte{0x00, 0x00})
	b.Write(payload)
	return b.Bytes()
}

// textFrame builds a UTF-8 (encoding 0x03) text information frame.
func textFrame(id, text string) []byte {
	return frame(id, append([]byte{0x03}, []byte(text)...))
}

// usltFrame builds an Unsynchronized lyrics/text (USLT) frame: UTF-8 encoding,
// "eng" language, an empty content descriptor, then the lyrics. SYLT
// (Synchronized lyrics) is intentionally NOT generated -- synced-tag support is
// out of scope.
func usltFrame(lyrics string) []byte {
	var p bytes.Buffer
	p.WriteByte(0x03) // UTF-8
	p.WriteString("eng")
	p.WriteByte(0x00) // empty content descriptor terminator
	p.WriteString(lyrics)
	return frame("USLT", p.Bytes())
}

// txxxFrame builds a UTF-8 TXXX (user-defined text information) frame.
// desc is the null-terminated description field; text is the value.
func txxxFrame(desc, text string) []byte {
	var p bytes.Buffer
	p.WriteByte(0x03) // UTF-8
	p.WriteString(desc)
	p.WriteByte(0x00) // null-terminate description
	p.WriteString(text)
	return frame("TXXX", p.Bytes())
}

// GenerateID3v2 builds an ID3v2.4 (UTF-8) tag with TIT2 (title), TPE1 (artist),
// and TALB (album) frames. When lyrics is non-empty a USLT frame is appended.
// The returned bytes are a complete, parseable tag suitable to write as a .mp3
// file for github.com/dhowden/tag to read. Unicode artist/title/album are
// supported via the UTF-8 text encoding byte.
func GenerateID3v2(artist, title, album, lyrics string) []byte {
	return GenerateID3v2Extended(artist, title, album, lyrics, nil, nil)
}

// GenerateID3v2Extended builds an ID3v2.4 tag like GenerateID3v2 but also
// accepts extra text frames and TXXX frames.
// textExtras maps frame ID to value (e.g. "TSRC" -> "US-ABC-12-34567").
// txxxExtras maps TXXX description to value (e.g. "MusicBrainz Track Id" -> "uuid").
func GenerateID3v2Extended(artist, title, album, lyrics string, textExtras, txxxExtras map[string]string) []byte {
	var frames bytes.Buffer
	if title != "" {
		frames.Write(textFrame("TIT2", title))
	}
	if artist != "" {
		frames.Write(textFrame("TPE1", artist))
	}
	if album != "" {
		frames.Write(textFrame("TALB", album))
	}
	if lyrics != "" {
		frames.Write(usltFrame(lyrics))
	}
	for id, val := range textExtras {
		frames.Write(textFrame(id, val))
	}
	for desc, val := range txxxExtras {
		frames.Write(txxxFrame(desc, val))
	}
	var b bytes.Buffer
	b.WriteString("ID3")
	b.Write([]byte{0x04, 0x00}) // version 2.4.0
	b.WriteByte(0x00)           // header flags
	b.Write(synchsafe(frames.Len()))
	b.Write(frames.Bytes())
	return b.Bytes()
}

// WriteAudioFileExtended writes a synthetic tagged .mp3 using GenerateID3v2Extended.
func WriteAudioFileExtended(dir, filename, artist, title, album, lyrics string, textExtras, txxxExtras map[string]string) error {
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, GenerateID3v2Extended(artist, title, album, lyrics, textExtras, txxxExtras), 0o644); err != nil { //nolint:gosec // test fixture file
		return fmt.Errorf("testutil: write audio file %s: %w", path, err)
	}
	return nil
}

// WriteAudioFile writes a synthetic tagged .mp3 (ID3v2.4) into dir. lyrics is
// optional; when empty, no USLT frame is embedded.
func WriteAudioFile(dir, filename, artist, title, album, lyrics string) error {
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, GenerateID3v2(artist, title, album, lyrics), 0o644); err != nil { //nolint:gosec // test fixture file
		return fmt.Errorf("testutil: write audio file %s: %w", path, err)
	}
	return nil
}

// GenerateFLAC builds a minimal valid FLAC file containing only a STREAMINFO
// metadata block. The STREAMINFO encodes sampleRate and totalSamples so that
// audioduration.FLAC can calculate the exact duration (totalSamples/sampleRate
// seconds). No audio frames are present -- this is header-only, which is all
// that audioduration reads.
//
// STREAMINFO bit layout (34 bytes after the block header):
//
//	bits  0-15  : min block size (16-bit)
//	bits 16-31  : max block size (16-bit)
//	bits 32-55  : min frame size (24-bit)
//	bits 56-79  : max frame size (24-bit)
//	bits 80-99  : sample rate (20-bit)
//	bits 100-102: channels minus one (3-bit)
//	bits 103-107: bits per sample minus one (5-bit)
//	bits 108-143: total samples (36-bit)
//	bits 144-271: MD5 signature (128-bit, zeroed)
func GenerateFLAC(sampleRate, totalSamples uint32) []byte {
	var b bytes.Buffer

	// "fLaC" stream marker.
	b.WriteString("fLaC")

	// STREAMINFO block header: last-metadata-block flag (0x80) | type 0.
	b.WriteByte(0x80) // last metadata block, type = STREAMINFO (0)
	// 24-bit block length = 34 bytes.
	b.Write([]byte{0x00, 0x00, 0x22})

	// 34-byte STREAMINFO payload -- build via bit-packing.
	var si [34]byte

	// min/max block size: 4096 (2 bytes each, big-endian).
	si[0] = 0x10
	si[1] = 0x00
	si[2] = 0x10
	si[3] = 0x00

	// min/max frame size: 0 (3 bytes each, meaning unknown).
	// si[4..9] already zero.

	// Pack sample_rate (20 bits), channels-1 (3 bits), bps-1 (5 bits),
	// total_samples (36 bits) into si[10..17].
	// channels=1 (stored as 0), bps=16 (stored as 15=0x0F).
	sr := uint64(sampleRate)
	ts := uint64(totalSamples)

	// Bits 80-99 = sample_rate (20 bits) at byte offset 10.
	// Bits 100-102 = channels-1 (3 bits).
	// Bits 103-107 = bps-1 (5 bits).
	// Bits 108-143 = total_samples (36 bits).
	//
	// Pack into bytes 10-17 (8 bytes).
	pack := (sr << 44) | (0 << 41) | (uint64(15) << 36) | (ts & 0xFFFFFFFFF)
	var packed [8]byte
	binary.BigEndian.PutUint64(packed[:], pack)
	copy(si[10:18], packed[:])
	// si[18..33] = MD5, zeroed.

	b.Write(si[:])
	return b.Bytes()
}

// WriteFLACFile writes a minimal FLAC fixture (header-only, no audio frames)
// into dir with the given sampleRate and totalSamples.
func WriteFLACFile(dir, filename string, sampleRate, totalSamples uint32) error {
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, GenerateFLAC(sampleRate, totalSamples), 0o644); err != nil { //nolint:gosec // test fixture file
		return fmt.Errorf("testutil: write flac file %s: %w", path, err)
	}
	return nil
}

// WriteLRCFile writes a stub .lrc sidecar into dir (used to simulate libraries
// where some tracks already have lyrics).
func WriteLRCFile(dir, filename string) error {
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte("[00:00.00] stub\n"), 0o644); err != nil { //nolint:gosec // test fixture file
		return fmt.Errorf("testutil: write lrc file %s: %w", path, err)
	}
	return nil
}
