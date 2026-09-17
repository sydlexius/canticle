package scanner

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"strings"

	"github.com/dhowden/tag"
)

// This file recovers multi-value Vorbis-comment ARTIST/ARTISTS/ALBUMARTIST/
// ALBUMARTISTS fields (FLAC, Ogg Vorbis, Ogg Opus) mangled by
// github.com/dhowden/tag (issues #969, #973). It is the Vorbis-comment sibling of
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
// string. FLAC embeds the comment block as one of its metadata blocks; Ogg
// (Vorbis and Opus alike -- the container framing and comment-header layout
// are identical after a different magic prefix) carries it as the second
// logical packet, which may itself span more than one Ogg page.
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

// oggMaxPages bounds how many Ogg pages this package reads while
// reassembling the comment header packet, so a file with no terminating
// page (corrupt or hostile) cannot loop forever. The comment packet is
// always the second logical packet in the stream (after the tiny
// identification header), so a real file needs only a handful of pages even
// when the comment header itself spans several.
const oggMaxPages = 256

// oggPageHeaderLen is the fixed portion of an Ogg page header, before the
// variable-length segment table: "OggS" (4) + version (1) + header flags
// (1) + granule position (8) + serial number (4) + sequence number (4) +
// CRC32 (4) + segment count (1).
const oggPageHeaderLen = 27

var (
	vorbisIdentPrefix   = []byte("\x01vorbis")
	vorbisCommentPrefix = []byte("\x03vorbis")
	opusIdentPrefix     = []byte("OpusHead")
	opusTagsPrefix      = []byte("OpusTags")
)

// vorbisMultiValueFields recovers the repeated Vorbis-comment fields from an
// already-open FLAC or Ogg (Vorbis/Opus) file, keyed by lowercased field
// name with values in file order. r is repositioned freely and left at an
// arbitrary offset; every caller in this package either has no further use
// for the handle's position or (audioDuration) always seeks to 0 itself
// before reading, so call order relative to duration parsing never matters.
//
// Returns nil for any file type other than FLAC/OGG, and nil on a genuine
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
	case tag.OGG:
		fields, ok = oggVorbisCommentFields(r)
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
// may be nil when the file is not FLAC/OGG or recovery failed, in which
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

// oggVorbisCommentFields reassembles the comment header packet from r's Ogg
// pages and parses it. The comment header is the second logical packet in
// the elementary bitstream: packet 1 is the tiny identification header
// ("\x01vorbis" for Vorbis, "OpusHead" for Opus), packet 2 is the comment
// header ("\x03vorbis" or "OpusTags"), and after that prefix the payload
// format is identical for both codecs (vendor string, then length-prefixed
// "KEY=value" entries) -- so one parser serves both.
//
// Packets are tracked per Ogg serial number (a file could in principle
// multiplex more than one logical bitstream); whichever stream's second
// completed packet carries a recognized prefix wins. A page's CRC is not
// verified here -- unlike dhowden/tag's own reader, this package trusts the
// framing fields (segment table, continuation flag) and only cares whether
// they parse structurally, since a CRC mismatch would already have failed
// the file at tag.ReadFrom before this ever runs.
func oggVorbisCommentFields(r io.ReadSeeker) (map[string][]string, bool) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, false
	}

	type stream struct {
		buf     bytes.Buffer
		packets int
		// done marks a stream whose comment-header position has already been
		// judged: its second packet was not a usable Vorbis/Opus comment block
		// (a non-audio stream such as Theora in a multiplexed file, or a
		// comment block that failed to parse). Later pages for that serial are
		// skipped rather than ending the scan, so another stream's comment
		// block can still be found.
		done bool
		// commentPrefix is the packet-2 prefix this stream's identification
		// packet (packet 1) commits it to: a Vorbis identification header only
		// accepts a Vorbis comment header, an Opus one only OpusTags. A stream
		// whose packet 1 names neither codec is marked done immediately.
		commentPrefix []byte
	}
	streams := make(map[uint32]*stream)

	for page := 0; page < oggMaxPages; page++ {
		hdr := make([]byte, oggPageHeaderLen)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil, false
		}
		if string(hdr[0:4]) != "OggS" {
			return nil, false
		}
		flags := hdr[5]
		serial := binary.LittleEndian.Uint32(hdr[14:18])
		segCount := int(hdr[26])

		segTable := make([]byte, segCount)
		if _, err := io.ReadFull(r, segTable); err != nil {
			return nil, false
		}
		pageDataLen := 0
		for _, s := range segTable {
			pageDataLen += int(s)
		}
		// A single page carries at most 255*255 bytes, so this allocation is
		// bounded by the page format itself; the 1 MiB cap is enforced per
		// stream below, on the packet actually being buffered, so a large
		// unrelated stream cannot exhaust the budget before the audio stream's
		// comment block is read. oggMaxPages bounds the total pages read.
		pageData := make([]byte, pageDataLen)
		if _, err := io.ReadFull(r, pageData); err != nil {
			return nil, false
		}

		st, ok := streams[serial]
		if !ok {
			st = &stream{}
			streams[serial] = st
		}
		if st.done {
			continue
		}

		pos := 0
		for _, s := range segTable {
			st.buf.Write(pageData[pos : pos+int(s)])
			pos += int(s)
			if st.buf.Len() > vorbisMultiValueCap {
				// This stream's in-progress packet is oversized: drop the stream,
				// not the whole scan.
				st.done = true
				st.buf.Reset()
				break
			}
			if s < 255 {
				// Lacing value below 255 terminates the packet.
				st.packets++
				data := st.buf.Bytes()
				if st.packets == 1 {
					switch {
					case bytes.HasPrefix(data, vorbisIdentPrefix):
						st.commentPrefix = vorbisCommentPrefix
					case bytes.HasPrefix(data, opusIdentPrefix):
						st.commentPrefix = opusTagsPrefix
					default:
						// Not an audio stream this package reads comments from.
						st.done = true
						st.buf.Reset()
					}
					if st.done {
						break
					}
					st.buf.Reset()
					continue
				}
				// packets == 2: the comment header, accepted only when its prefix
				// matches the codec packet 1 declared.
				if bytes.HasPrefix(data, st.commentPrefix) {
					if fields, ok := parseVorbisCommentFields(data[len(st.commentPrefix):]); ok {
						return fields, true
					}
				}
				// Not a usable comment block on this stream: stop tracking it and
				// keep reading pages for the others.
				st.done = true
				st.buf.Reset()
				break
			}
		}
		_ = flags // continuation is implicit in the per-serial buffer; the
		// flag itself is not consulted -- a page's segment table already
		// tells us unambiguously whether the accumulating packet continues
		// (last lacing value 255) or terminates (last lacing value <255)
		// regardless of what the continuation bit claims, and trusting the
		// data over a possibly-inconsistent flag is the safer read.
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
