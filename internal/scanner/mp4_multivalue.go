package scanner

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"

	"github.com/dhowden/tag"
)

// This file recovers multi-value MP4-family (m4a/m4b/m4p/alac) artist and
// album-artist atoms mangled by github.com/dhowden/tag (issue #958). It is
// the MP4 sibling of multiValueTag above (issue #466), but the mechanism and
// the recovery are different: #466 is unrecoverable at the tag layer by
// construction (the ID3 parser destroys the value boundaries before
// m.Artist() returns, and is recovered from a parallel TXXX frame); this one
// is fully recoverable because every byte dhowden/tag read is still present
// in Raw() -- the parser just failed to walk the atom's second and later
// "data" children.
//
// Root cause (see mp4.go:138 in the pinned dependency, readAtomData): the
// parser reads the ENTIRE remaining atom payload into one buffer, strips
// exactly one 8-byte "data" box header plus one 8-byte version/flags+locale
// block from the FRONT, and returns the rest as a single string. For an atom
// with N>=2 "data" children this yields:
//
//	value1 + H2 + value2 + H3 + value3 + ... + HN + valueN
//
// where each Hi (i>=2) is the 16-byte header dhowden/tag failed to strip for
// that child: a 4-byte big-endian box length (Li = 16 + len(valueI)), the
// 4-byte literal "data", 4 bytes version(1)+flags(3), and a 4-byte locale
// (normally all zero). Li is the child's OWN box length as originally
// written by the tagger, so once a header is located it fully determines its
// value's length regardless of what follows -- it is not a "distance to the
// next header" estimate.
//
// Approach chosen: split the already-flattened string in place (design
// option (b) in issue #958), rather than re-opening the file and walking its
// atom tree with a fresh box parser (option (a)). Every byte needed for
// recovery is already sitting in Raw() -- dhowden/tag did not drop any data,
// it just failed to interpret the header framing -- so a re-read buys
// nothing but a second file open on the scan hot path (see the audiodur
// package notes on keeping library disks asleep). The risk option (b) trades
// for that is a legitimate value that happens to contain the "data" atom
// signature; splitMP4SplicedValue closes that gap by validating the
// candidate's length prefix against the ACTUAL remaining byte count for
// every subsequent header in the chain, all the way to end-of-buffer, and by
// gating scanning on the atom's parent tag.Metadata reporting tag.MP4 so the
// unrelated ID3 path (multiValueTag) is never reached. Constructing a
// legitimate artist string that both contains the literal 4-byte "data"
// token AND carries a 4-byte big-endian length immediately before it that
// happens to agree with the exact remaining byte count -- for every header
// in the chain, through to EOF -- is not a realistic accident.

// mp4SpliceHeaderLen is the size in bytes of the box header + version/flags +
// locale block dhowden/tag fails to strip from every "data" child after the
// first: 4 bytes big-endian box length, 4 bytes literal "data", 4 bytes
// version(1)+flags(3), 4 bytes locale.
const mp4SpliceHeaderLen = 16

// mp4TextClassLocale is the 8 bytes every spliced header carries between the
// "data" literal and its payload: a 4-byte version(1 byte, 0) + flags(3 bytes,
// 0x000001 = the TEXT well-known type) block, then a 4-byte locale that text
// atoms leave zero.
//
// Checking it is what makes the splice signature STRUCTURAL rather than a bare
// length coincidence. Without it, the only evidence a header exists is that a
// length prefix happens to account for the remaining bytes, which a legitimate
// single value CAN satisfy -- a payload shaped
// <4-byte length><data><8 bytes><rest> whose length lands exactly at EOF parses
// as a chain and gets split into two artists. These 8 bytes must ALSO match,
// and they are mostly NUL, which a legitimate text artist value does not carry.
//
// Deliberately STRICT about the locale. A non-zero locale falls through to the
// mangled value rather than being split, because the two failure directions are
// not symmetric: a false negative leaves the status quo (one corrupted string,
// which is the bug we already have), while a false positive invents artist
// boundaries in a value that never had them. Bias toward the recoverable error.
var mp4TextClassLocale = [8]byte{0, 0, 0, 1, 0, 0, 0, 0}

// mp4SpliceHeaderAt reports whether a spliced "data" header begins at hdr --
// a 4-byte length, the "data" literal, then mp4TextClassLocale -- and returns
// the payload length that header claims.
//
// The length is compared against the remaining buffer as uint64 BEFORE it is
// narrowed to int. On a 32-bit build int is 32 bits, so a length such as
// 0x80000010 narrows NEGATIVE; valLen then goes negative, valStart+valLen
// compares as less than len(data) so the bounds guard passes, and the slice
// panics with high < low. Verified by narrowing simulation: int32(0x80000010)
// = -2147483632. Current CI and GoReleaser targets are amd64/arm64 only, so no
// shipped build reaches it today -- which makes this a latent defect gated on a
// build-config fact that can change, not one that is safe by construction.
func mp4SpliceHeaderAt(data []byte, hdr int) (valLen int, ok bool) {
	if hdr < 0 || hdr+mp4SpliceHeaderLen > len(data) {
		return 0, false
	}
	if string(data[hdr+4:hdr+8]) != "data" {
		return 0, false
	}
	if !bytes.Equal(data[hdr+8:hdr+mp4SpliceHeaderLen], mp4TextClassLocale[:]) {
		return 0, false
	}
	// Bound the length BEFORE narrowing. int is 32 bits on a 32-bit build, so a
	// value above MaxInt32 narrows NEGATIVE: valLen would go negative, the
	// bounds check below would compare as less-than and PASS, and the slice
	// would panic with high < low (int32(0x80000010) = -2147483632). Rejecting
	// above MaxInt32 costs nothing real -- it is far beyond any atom a tagger
	// writes -- and makes int(l) safe on every word size, so the arithmetic
	// below is plain int with no further conversion.
	l := binary.BigEndian.Uint32(data[hdr : hdr+4])
	if l < mp4SpliceHeaderLen || l > math.MaxInt32 {
		return 0, false
	}
	n := int(l) - mp4SpliceHeaderLen
	// avail is non-negative: the hdr+mp4SpliceHeaderLen > len(data) guard above
	// already returned. Both sides are int, so nothing converts here.
	if avail := len(data) - (hdr + mp4SpliceHeaderLen); n > avail {
		return 0, false
	}
	return n, true
}

// mp4ArtistAtoms and mp4AlbumArtistAtoms name the raw MP4 atom keys
// dhowden/tag's Raw() stores multi-value artist / album-artist data under.
// Raw() is keyed by the literal atom name as read from the file, not by the
// friendly "artist"/"album_artist" name Artist()/AlbumArtist() resolve
// through -- the dependency's atom table maps both "\xa9ART" and its
// lowercase sibling "\xa9art" to the artist meaning, so a tagger could in
// principle write either.
var (
	mp4ArtistAtoms      = []string{"\xa9ART", "\xa9art"}
	mp4AlbumArtistAtoms = []string{"aART"}
)

// multiValueMP4Tag inspects m's raw MP4 atoms for one of atomNames and, when
// its stored string carries the recoverable multi-"data"-child splice
// signature, returns the recovered discrete values trimmed of surrounding
// whitespace, empty values dropped, joined by artistValueSep. It returns ""
// when no atom carries a VALIDATED splice signature, so a single-value atom
// (which dhowden/tag reads correctly, with nothing to recover) falls through
// to the standard accessor untouched.
//
// Returns ok=false ONLY when no atom carries a validated splice, which is the
// single condition under which the raw accessor is trustworthy. The recovered
// string is returned SEPARATELY from that flag because the two are genuinely
// independent: a validated splice whose children are all empty recovers the
// empty string, and "" is the correct artist there, not a signal to fall back.
// Collapsing them -- treating "" as failure -- reinstates the very defect this
// file exists to fix, since the fallback hands back the spliced header bytes.
//
// Strictly gated on m.Format() == tag.MP4 before Raw() is even consulted, so
// an ID3 file's raw frames -- which never carry an MP4 atom key and could in
// principle collide with one by coincidence -- are never inspected by this
// path (issue #958's explicit regression guard against #466).
func multiValueMP4Tag(m tag.Metadata, atomNames []string) (string, bool) {
	if m.Format() != tag.MP4 {
		return "", false
	}
	raw := m.Raw()
	if raw == nil {
		return "", false
	}
	for _, name := range atomNames {
		v, ok := raw[name]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		values, ok := splitMP4SplicedValue(s)
		if !ok {
			continue
		}
		out := make([]string, 0, len(values))
		for _, p := range values {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		// The VALIDATED SPLICE is the success condition -- not a count of
		// surviving values, and not the joined string being non-empty. This is
		// where the MP4 path deliberately departs from multiValueTag's ">= 2"
		// ID3 rule, because the two fall-throughs have opposite safety
		// properties. In the ID3 case the raw accessor is TRUSTWORTHY (a
		// single-value TPE1 reads correctly), so falling through costs nothing
		// and the >= 2 rule usefully avoids overriding a good value. Here a
		// validated splice PROVES the atom carries two or more "data" children
		// and therefore that m.Artist() is corrupt, so any fall-through hands
		// back the spliced header bytes -- the exact defect this file fixes.
		//
		// Two reachable shapes make this concrete, and both leaked the raw
		// bytes under earlier gates: a tagger terminating the atom with a
		// trailing EMPTY child (two children, one surviving value, which a
		// ">= 2" gate dropped), and an atom whose children are ALL empty or
		// whitespace (zero surviving values, which a "non-empty string" gate
		// dropped). In the second case "" is the CORRECT artist -- an empty
		// tag -- and is strictly better than the mangled alternative, which is
		// why success travels in its own return value rather than being
		// inferred from the string.
		return strings.Join(out, artistValueSep), true
	}
	return "", false
}

// splitMP4SplicedValue attempts to recover the discrete values flattened
// into s by the dhowden/tag defect described above. It reports ok=false
// whenever the byte-level splice signature cannot be found and fully
// validated end to end -- most commonly because s genuinely holds a single,
// unmangled value, which is the overwhelmingly common case and must pass
// through unchanged.
//
// value1's length is not recorded anywhere (its own header was correctly
// stripped by dhowden/tag), so the search scans for the first candidate "H2"
// header -- a "data" literal preceded by a 4-byte big-endian length -- and,
// for each candidate left to right, hands the remainder of the buffer to
// completeMP4SpliceChain to confirm the rest of the chain (H3/value3,
// H4/value4, ...) parses cleanly all the way to end-of-buffer with zero
// leftover bytes. A candidate that fails that check is a coincidental match,
// not a real header, and is discarded in favor of the next occurrence
// (or, if none validate, ok=false).
func splitMP4SplicedValue(s string) ([]string, bool) {
	data := []byte(s)
	for idx := 4; idx+4 <= len(data); idx++ {
		if string(data[idx:idx+4]) != "data" {
			continue
		}
		hdr := idx - 4
		valLen, ok := mp4SpliceHeaderAt(data, hdr)
		if !ok {
			continue
		}
		valStart := hdr + mp4SpliceHeaderLen
		values := []string{
			string(data[:hdr]),
			string(data[valStart : valStart+valLen]),
		}
		if completeMP4SpliceChain(data, valStart+valLen, &values) {
			return values, true
		}
	}
	return nil, false
}

// completeMP4SpliceChain extends values with every further spliced value
// starting at pos, the byte immediately after the previously recovered
// value ends. pos is expected to be the start of the NEXT header's 4-byte
// length prefix (or exactly len(data), meaning the previous value was the
// last one). It reports false the moment the bytes at pos stop looking like
// a valid header or a claimed length overruns the buffer, so a coincidental
// partial match can never yield a silently truncated or padded value.
func completeMP4SpliceChain(data []byte, pos int, values *[]string) bool {
	for pos < len(data) {
		valLen, ok := mp4SpliceHeaderAt(data, pos)
		if !ok {
			return false
		}
		valStart := pos + mp4SpliceHeaderLen
		*values = append(*values, string(data[valStart:valStart+valLen]))
		pos = valStart + valLen
	}
	return true
}
