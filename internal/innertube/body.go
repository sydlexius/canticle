package innertube

import "bytes"

// isUnusableBody reports whether raw is empty, all-whitespace, or does not
// begin a JSON OBJECT or ARRAY -- i.e. it looks like a hollow 200 or a
// non-JSON interstitial (a captive portal, an edge error page, a bot
// challenge -- all realistic for an undocumented API accessed without
// credentials) rather than a genuinely malformed JSON document (854-F4). A
// caller uses this to distinguish that class from a real mid-document parse
// error, which stays a naked, unclassified error: the former is a clean miss
// ("reached it, nothing usable", matching ErrNotFound's contract in
// errors.go), the latter is a signal something is actually broken and should
// not be silently bucketed as benign.
//
// Object-or-array, NOT "starts with any JSON value character", is the
// deliberate criterion (854-R4F2). An earlier revision of this comment
// claimed the broader test, which read as a bug against the narrower code;
// the CLAIM was what was wrong, and it is the claim that is corrected here.
// Every innertube response this package parses is a JSON object, so a body
// whose entire content is the scalar "text", 123, or true is exactly as
// unusable as an HTML error page, and all three call sites want it in the
// benign-miss bucket:
//
//   - parseSearchCandidates and parseLyricsBrowseID call this only AFTER
//     unmarshalling into their response struct has already failed. Accepting
//     scalars as "usable" there would reclassify those bodies as genuine
//     mid-document parse errors -- an unclassified error, and a false alarm,
//     for a response that plainly carries nothing.
//   - Browse calls it on the raw bytes it is about to hand to the decode
//     package. Accepting a scalar there would pass (nil error, undecodable
//     bytes) downstream, the exact hand-off 854-F5 exists to prevent.
//
// A bare null is the one scalar that never reaches here: it unmarshals into
// a zero struct without error, so both parse sites fall through to their own
// "nothing found" path, which is the same ErrNotFound class. The
// classification is therefore uniform across every scalar shape either way,
// which is why the fix belongs in this comment and not in any call site.
func isUnusableBody(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true
	}
	return trimmed[0] != '{' && trimmed[0] != '['
}
