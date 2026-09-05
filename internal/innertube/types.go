package innertube

// SearchCandidate is one result from an innertube search call: enough to
// decide whether it corresponds to the queried track and, if so, to continue
// the flow with a next() call.
//
// The client (search) produces these; the selection slice consumes them to
// apply the correspondence guard. Search never signals "no match" -- see
// doc.go -- so every candidate must be independently checked against the
// query before being trusted.
type SearchCandidate struct {
	VideoID string
	Title   string
	Artist  string

	// DurationSeconds is the candidate's duration as parsed from the search
	// response, available as a pre-filter. Zero means "not supplied" -- an
	// absent duration must never reject a candidate, so the pre-filter fails
	// OPEN on zero. The seed only guarantees this value is carried from the
	// search response through to selection; #853 owns the decision about
	// how (or whether) to use it as a pre-filter.
	DurationSeconds int
}

// Cue is one timed lyric line as extracted from a browse response: a line of
// text paired with a millisecond start and end offset (innertube's cueRange
// carries both, unlike petitlyrics' subtitle format which has none -- see
// doc.go).
//
// The client (browse, against the ANDROID_MUSIC/IOS_MUSIC clients that return
// timings) produces these; the decode slice consumes them to build a
// models.Synced via models.MsToTime.
type Cue struct {
	Text    string
	StartMs int

	// EndMs is captured deliberately: an end time per line is a genuine
	// distinguishing feature of this provider's payload (see doc.go). No
	// current internal/models type consumes a line-level end time --
	// models.Lines is {Text, Time} and models.WordTiming.EndMS is
	// word-level, not line-level -- so this value currently has nowhere to
	// go downstream. #852 (decode) should carry it rather than silently
	// dropping it or widening internal/models, which is outside its scope.
	EndMs int
}
