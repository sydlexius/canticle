package innertube

import "bytes"

// isUnusableBody reports whether raw is empty, blank, or does not begin a
// JSON OBJECT -- i.e. it looks like a hollow 200 or a non-JSON interstitial
// (a captive portal, an edge error page, a bot challenge -- all realistic for
// an undocumented API accessed without credentials) rather than a genuinely
// malformed JSON document (854-F4). A caller uses this to distinguish that
// class from a real mid-document parse error, which stays a naked,
// unclassified error: the former is a clean miss ("reached it, nothing
// usable", matching ErrNotFound's contract in errors.go), the latter is a
// signal something is actually broken and should not be silently bucketed as
// benign.
//
// OBJECT-ONLY, not "starts with any JSON value character", is the deliberate
// criterion. Every innertube response this package parses unmarshals into a
// struct, so a body whose entire content is a scalar OR A TOP-LEVEL ARRAY is
// exactly as unusable as an HTML error page: each fails with the same
// "cannot unmarshal X into ..." class, and every call site wants that in the
// benign-miss bucket rather than raised as a real parse failure.
//
// An array was previously accepted, which contradicted that rationale
// (854-R5F1): "[1,2,3]" was reported usable while being just as undecodable
// as "text". Widening in the other direction, to any JSON value character,
// was rejected for the same reason (854-R4F2) -- so the accept set is the
// one shape a response actually takes.
//
// A bare null is the one scalar that never reaches here: it unmarshals into
// a zero struct without error, so a parse site falls through to its own
// "nothing found" path, which is the same ErrNotFound class.
//
// The leading byte-order mark is stripped first. bytes.TrimSpace does not
// remove U+FEFF, so a BOM-prefixed but otherwise valid object would
// otherwise be misfiled as a hollow body and its lyric lost silently
// (854-R5F2). Note the trim is UNICODE whitespace, which is broader than the
// four bytes JSON permits; a body led by an exotic space is reported usable
// and then fails to decode, which is the safe direction -- it surfaces as a
// real error rather than a silent miss.
func isUnusableBody(raw []byte) bool {
	// Trim on BOTH sides of the BOM strip: the mark may sit before or after
	// leading whitespace, and TrimPrefix only matches at byte zero.
	trimmed := bytes.TrimSpace(raw)
	trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("\ufeff")))
	if len(trimmed) == 0 {
		return true
	}
	return trimmed[0] != '{'
}
