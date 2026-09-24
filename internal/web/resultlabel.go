package web

import "github.com/sydlexius/canticle/internal/reports"

// resultLabel renders a reports.ResultClass as the human-facing string shown
// in the Recent Outcomes tables (#627). Word-synced and line-synced get a
// readable hyphenated label distinct from the underscored persisted value
// ("word_synced" -> "word-synced"); every other class renders as its raw
// string, unchanged from before this issue split the synced bucket.
func resultLabel(rc reports.ResultClass) string {
	switch rc {
	case reports.ResultWordSynced:
		return "word-synced"
	case reports.ResultLineSynced:
		return "line-synced"
	default:
		return string(rc)
	}
}

// resultTierClass returns the CSS class(es) for the Result cell's tier badge
// (#627), reusing the pill-badge idiom the Up-next panel introduced for its
// tier column (mx-upnext-tier-*, #572) so the two read as one system rather
// than inventing a second badge language. Empty means "no badge" -- every
// non-synced class (miss, unsynced, instrumental, rejected, unknown) renders
// as plain text, exactly as it did before this issue.
//
// ResultSynced (word_timing_state NULL/unrecorded) gets its own muted class
// rather than reusing markNone/no-badge, so "synced, tier not recorded" reads
// as a distinct, honest state rather than as the absence of a class -- the
// same "shown honestly, not guessed" requirement #627's AC states for legacy
// rows.
func resultTierClass(rc reports.ResultClass) string {
	switch rc {
	case reports.ResultWordSynced:
		return "mx-result-tier mx-result-tier-word"
	case reports.ResultLineSynced:
		return "mx-result-tier mx-result-tier-line"
	case reports.ResultSynced:
		return "mx-result-tier mx-result-tier-unknown"
	default:
		return ""
	}
}
