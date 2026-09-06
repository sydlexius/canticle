package innertube

import (
	"encoding/json"
	"testing"
)

// TestIsUnusableBody_Classification is the predicate's direct contract test.
// isUnusableBody was wrong in three consecutive review rounds of PR #872
// precisely because it was only ever exercised indirectly, through Search /
// Next / Browse, where a miscall reads as a plausible provider error rather
// than as a predicate bug. Every case below states what the byte string IS,
// so the reason a body lands in the benign-miss bucket is visible without
// reading a transport call.
func TestIsUnusableBody_Classification(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		// Usable: the two shapes a real innertube response can take.
		{"object", `{"contents":{}}`, false},
		{"empty_object", `{}`, false},
		{"array", `[1,2,3]`, false},
		{"empty_array", `[]`, false},
		{"leading_whitespace_object", "  \n\t{}", false},
		{"trailing_whitespace_object", "{}  \n", false},
		// Usable by this predicate on purpose: a body that BEGINS an object
		// but is truncated mid-document is a genuinely broken response, not a
		// clean miss, and must stay an unclassified error at the call site.
		{"truncated_object", `{"contents":{"tabbedSearchResultsRenderer":`, false},

		// Unusable: nothing at all.
		{"empty", "", true},
		{"single_space", " ", true},
		{"whitespace", "   \n\t\r", true},

		// Unusable: a non-JSON interstitial, the realistic failure for an
		// undocumented API reached without an account.
		{"html_error_page", "<html><body>captive portal</body></html>", true},
		{"plain_text", "service unavailable", true},

		// Unusable: a bare JSON SCALAR. Valid JSON, but no innertube response
		// this package parses is a scalar, so a body that is only a scalar
		// carries nothing usable and belongs in the same benign-miss bucket
		// as an error page (854-R4F2). The predicate's criterion is
		// object-or-array, NOT "starts with any JSON value character"; an
		// earlier comment claimed the broader test and was corrected to
		// match the code, because the code was the part that was right.
		{"json_string", `"hello"`, true},
		{"json_number", `123`, true},
		{"json_true", `true`, true},
		{"json_false", `false`, true},
		{"json_null", `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnusableBody([]byte(tc.body)); got != tc.want {
				t.Errorf("isUnusableBody(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestIsUnusableBody_ScalarsAreUnreachableOrBenign is the evidence behind the
// scalar rows above, and the reason this predicate needs no per-call-site
// variant. Its two parse call sites reach it only AFTER unmarshalling into a
// response struct has already failed, so what matters is which scalars fail
// that unmarshal:
//
//   - every scalar except null FAILS it, so it reaches isUnusableBody and is
//     classified as a benign miss rather than raised as a false alarm;
//   - a bare null SUCCEEDS into a zero-valued struct, so it never reaches
//     this predicate at all and falls through the call site's own
//     "nothing found" path -- the same benign class.
//
// The classification is therefore uniform across every scalar shape whichever
// way the body goes, which is what makes object-or-array correct at all three
// call sites at once. If this test ever fails, the assumption the predicate's
// comment rests on has changed in encoding/json.
func TestIsUnusableBody_ScalarsAreUnreachableOrBenign(t *testing.T) {
	// A stand-in for the response structs the parse call sites unmarshal
	// into: an object with at least one field, which is every one of them.
	type responseShape struct {
		Contents struct{} `json:"contents"`
	}

	for _, tc := range []struct {
		name              string
		body              string
		wantUnmarshalFail bool
	}{
		{"string", `"hello"`, true},
		{"number", `123`, true},
		{"true", `true`, true},
		{"false", `false`, true},
		{"null", `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp responseShape
			err := json.Unmarshal([]byte(tc.body), &resp)
			if gotFail := err != nil; gotFail != tc.wantUnmarshalFail {
				t.Fatalf("unmarshal(%s) failed = %v (err %v), want failed = %v",
					tc.body, gotFail, err, tc.wantUnmarshalFail)
			}
			if !tc.wantUnmarshalFail {
				// Unreachable by the parse sites; nothing more to assert
				// than that it decoded to the zero value, which is what
				// makes the call site's own miss path take over.
				if resp.Contents != (struct{}{}) {
					t.Errorf("want a zero-valued struct from %s", tc.body)
				}
				return
			}
			if !isUnusableBody([]byte(tc.body)) {
				t.Errorf("a scalar that fails the struct unmarshal must classify as unusable, %s did not", tc.body)
			}
		})
	}
}

// TestIsUnusableBody_IgnoresContentAfterTheFirstToken pins the predicate's
// scope: it judges only how a body STARTS. Deciding whether a well-formed
// opening is followed by a valid document is the JSON decoder's job, and
// duplicating that here would reclassify real parse errors as benign misses.
func TestIsUnusableBody_IgnoresContentAfterTheFirstToken(t *testing.T) {
	for _, body := range []string{
		`{`,
		`[`,
		`{not json at all`,
		`{"a":1}garbage`,
	} {
		if isUnusableBody([]byte(body)) {
			t.Errorf("isUnusableBody(%q) = true, want false: a body that opens an object or array is the decoder's to reject", body)
		}
	}
}
