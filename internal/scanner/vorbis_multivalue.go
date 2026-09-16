package scanner

import (
	"encoding/binary"
	"io"
	"log/slog"
	"strings"

	"github.com/dhowden/tag"
)

// This file recovers multi-value Vorbis-comment ARTIST/ARTISTS/ALBUMARTIST/
// ALBUMARTISTS fields in FLAC files mangled by
// github.com/dhowden/tag (issue #969). It is the Vorbis-comment sibling of
// multiValueTag (#466, ID3v2.4 TXXX) and multiValueMP4Tag (#958/#959, MP4
// atoms), but neither of those mechanisms applies here: the dependency's
// Vorbis comment reader stores every field in a plain map[string]string
// keyed by lowercased field name (vorbis.go:59, "m.c[strings.ToLower(k)] =
// v"), so a comment block carrying the same field twice has its first value
// silently overwritten by the second BEFORE Metadata.Artist() or Metadata.
// Raw() ever runs -- unlike the MP4 case, no byte survives in Raw() to
// recover from.
//
// The only way to recover the discrete values is to re-read the Vorbis
// comment block ourselves, directly off the file's own bytes, keeping every
// repeated field as an ordered list rather than collapsing it into one
// string. FLAC embeds the comment block as one of its metadata blocks, which
// is what this file walks. Ogg Vorbis and Opus carry the same comment payload
// as a packet reassembled across Ogg pages; that container is not handled
// here yet, so an Ogg file falls back to the dependency's single value.
//
// Every length read while doing this is bounded against the remaining data
// BEFORE it is used to size a slice or a loop, so a corrupt or hostile file
// cannot force a large allocation, an out-of-bounds slice, or an unbounded
// loop (mirrors the bounds discipline in mp4_multivalue.go).

// vorbisMultiValueCap bounds the total number of comment-block bytes this
// package will read into memory while recovering repeated fields. Real
// Vorbis comment blocks run from a few hundred bytes to a few KB even with
// many extra tags (cover art lives in a separate, skipped block); 1 MiB is
// generous headroom without being large enough to matter for memory
// pressure on a corrupt or adversarial file.
const vorbisMultiValueCap = 1 << 20

// vorbisMultiValueMaxFields bounds the declared comment count read from a
// comment block header, so a corrupt count field cannot drive an unbounded
// loop even when every individual field length is small and within budget.
const vorbisMultiValueMaxFields = 4096

// flacMaxMetadataBlocks bounds how many FLAC metadata block headers this
// package will walk looking for the VORBIS_COMMENT block (type 4), so a file
// whose last-block flag is never set (corrupt or hostile) cannot loop
// forever. Real FLAC files carry a handful of blocks (STREAMINFO, one
// VORBIS_COMMENT, maybe a SEEKTABLE, PICTURE, PADDING); this is generous
// headroom.
const flacMaxMetadataBlocks = 256

// vorbisMultiValueFields recovers the repeated Vorbis-comment fields from an
// already-open FLAC file, keyed by lowercased field
// name with values in file order. r is repositioned freely and left at an
// arbitrary offset; every caller in this package either has no further use
// for the handle's position or (audioDuration) always seeks to 0 itself
// before reading, so call order relative to duration parsing never matters.
//
// Returns nil for any file type other than FLAC, and nil on a genuine
// parse failure (corrupt or truncated comment block) -- logged at Debug, not
// Warn, because this is a best-effort recovery layered on top of a file
// dhowden/tag already parsed successfully; a caller that gets nil falls
// back to the dependency's own (possibly mangled but non-fatal) single
// value, exactly as if this package did not exist.
func vorbisMultiValueFields(r io.ReadSeeker, m tag.Metadata) map[string][]string {
	var fields map[string][]string
	var ok bool
	switch m.FileType() {
	case tag.FLAC:
		fields, ok = flacVorbisCommentFields(r)
	default:
		return nil
	}
	if !ok {
		slog.Debug("could not recover vorbis comment block for multi-value field recovery; falling back to single value", "filetype", m.FileType())
		return nil
	}
	return fields
}

// multiValueVorbisField returns the recovered discrete values of a repeated
// Vorbis-comment field from an already-parsed field map (nil-safe: fields
// may be nil when the file is not FLAC or recovery failed, in which
// case every lookup misses and ok is false), trying keys in PRECEDENCE
// order -- the plural "artists"/"albumartists" form first, else the singular
// "artist"/"albumartist" form, mirroring the ID3 TXXX ARTISTS precedence in
// multiValueTag (#466).
//
// The success condition is that the field was genuinely REPEATED (two or more
// occurrences) and at least one occurrence is non-empty after trimming, not
// that two or more values survive. A repeated field is exactly the shape
// dhowden/tag misreads: it keeps only the LAST occurrence, so a trailing empty
// "ARTIST=" makes m.Artist() return "" while a real artist sits earlier in the
// block. Requiring two survivors would fall back to that "" and lose the one
// real value. The survivors are joined by artistValueSep, so a single survivor
// is returned alone.
//
// Returns ok=false when no key is repeated with a non-empty value, so a field
// that occurs once -- which dhowden/tag already reads correctly, with nothing
// to recover -- falls through to the standard accessor untouched.
func multiValueVorbisField(fields map[string][]string, keys ...string) (string, bool) {
	for _, key := range keys {
		values := fields[key]
		if len(values) < 2 {
			continue // absent, or occurs once: dhowden/tag read it correctly
		}
		out := make([]string, 0, len(values))
		for _, v := range values {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		if len(out) >= 1 {
			return strings.Join(out, artistValueSep), true
		}
	}
	return "", false
}

// flacVorbisCommentFields walks r's FLAC metadata blocks looking for the
// VORBIS_COMMENT block (type 4) and parses it. Returns ok=false on any
// structural problem: bad magic, a truncated read, a block length that
// would overrun a sane cap, or no VORBIS_COMMENT block found before the
// last-block flag (or the block-count cap) is reached.
func flacVorbisCommentFields(r io.ReadSeeker) (map[string][]string, bool) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, false
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != "fLaC" {
		return nil, false
	}

	for i := 0; i < flacMaxMetadataBlocks; i++ {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil, false
		}
		last := hdr[0]&0x80 != 0
		blockType := hdr[0] &^ 0x80
		// 24-bit big-endian block length. The maximum representable value
		// (0xFFFFFF = 16,777,215) fits comfortably in int on any platform
		// dhowden/tag supports, so no narrowing hazard here (unlike the
		// 32-bit length fields elsewhere in this file and in
		// mp4_multivalue.go).
		blockLen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])

		const vorbisCommentBlockType = 4
		if blockType == vorbisCommentBlockType {
			if blockLen < 0 || blockLen > vorbisMultiValueCap {
				return nil, false
			}
			payload := make([]byte, blockLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				return nil, false
			}
			return parseVorbisCommentFields(payload)
		}

		// Not the block we want: skip over it. Seeking past a bogus length
		// never allocates and never over-reads; a corrupt length just makes
		// the NEXT header read fail cleanly (io.ReadFull errors past EOF),
		// which the loop already handles.
		if _, err := r.Seek(int64(blockLen), io.SeekCurrent); err != nil {
			return nil, false
		}
		if last {
			return nil, false
		}
	}
	return nil, false
}

// parseVorbisCommentFields parses a raw Vorbis comment payload (vendor
// string, then a count-prefixed list of length-prefixed "KEY=value"
// entries, all in little-endian, per the Vorbis I spec) into a map of
// lowercased field name to every value seen, in order. Returns ok=false on
// any structural problem -- a length prefix that would read past the end of
// data is rejected before it is used to size a slice, so a corrupt length
// can never over-read or over-allocate.
func parseVorbisCommentFields(data []byte) (map[string][]string, bool) {
	pos := 0
	readLen := func() (uint32, bool) {
		if pos+4 > len(data) {
			return 0, false
		}
		l := binary.LittleEndian.Uint32(data[pos : pos+4])
		pos += 4
		return l, true
	}

	vendorLen, ok := readLen()
	if !ok {
		return nil, false
	}
	// Compare as uint64 before using the length: pos is a small, already-
	// bounded int and vendorLen is a uint32, so the sum can never overflow
	// uint64, and this keeps a huge declared vendor length from being used
	// to size anything before it is checked (mirrors the bounds discipline
	// in mp4_multivalue.go's mp4SpliceHeaderAt).
	if uint64(pos)+uint64(vendorLen) > uint64(len(data)) { //nolint:gosec // reason: pos is a small, non-negative offset bounded by len(data) throughout this function; it can never be negative, so the int->uint64 conversion cannot overflow
		return nil, false
	}
	pos += int(vendorLen) // vendor string itself carries no field data

	commentsLen, ok := readLen()
	if !ok {
		return nil, false
	}
	if commentsLen > vorbisMultiValueMaxFields {
		return nil, false
	}

	fields := make(map[string][]string)
	for i := uint32(0); i < commentsLen; i++ {
		l, ok := readLen()
		if !ok {
			return nil, false
		}
		if uint64(pos)+uint64(l) > uint64(len(data)) { //nolint:gosec // reason: pos is a small, non-negative offset bounded by len(data) throughout this function; it can never be negative, so the int->uint64 conversion cannot overflow
			return nil, false
		}
		kv := string(data[pos : pos+int(l)])
		pos += int(l)
		k, v, found := strings.Cut(kv, "=")
		if !found {
			// Malformed entry (no '='), per the spec every comment must
			// carry one. Skip it rather than aborting the whole block: one
			// bad entry does not invalidate the rest.
			continue
		}
		key := strings.ToLower(k)
		fields[key] = append(fields[key], v)
	}
	return fields, true
}
