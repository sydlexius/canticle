package web

import (
	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/providers"
)

// Lane mark tokens. These are the values laneMark emits and the templates
// dispatch on -- an indirection layer, not an asset path.
//
// The indirection is the point (#601). A mark's rendering differs by provenance:
// the detector's glyph is authored in-repo and inlined as currentColor SVG,
// while a provider's mark would be a vendored brand asset with fixed colors and
// its own usage terms. A token lets the template pick the treatment without any
// call site knowing which kind it is, and it keeps the eventual asset path in
// one place so a rebrand or takedown is a one-line change.
const (
	// markNone means this lane has no mark. Rendering degrades to the display
	// name alone -- never a broken image, never a reserved blank gap.
	markNone = ""
	// markInstrumentalDetector is the muted-microphone glyph authored in-repo.
	markInstrumentalDetector = "instrumental-detector"
	// markMusixmatch is the vendored official Musixmatch mark. Sourced from the
	// provider's own CDN and used unmodified; see docs/provider-terms.md for the
	// source URL and the date it was retrieved.
	markMusixmatch = "musixmatch"
)

// laneMark returns the mark token for a PERSISTED lane string, or markNone when
// the lane has no mark yet.
//
// Deliberately a sibling of laneLabel rather than an extension of it. The two
// answer different questions and are unmapped independently: every lane has a
// label (laneLabel falls back to the raw value), but most lanes currently have
// no mark, and the provider marks are blocked on sourcing rather than on code.
// Folding them into one function would force a caller wanting only the name to
// reason about assets.
//
// The same switch-not-map reasoning as laneLabel applies: fixed at compile time,
// and a package-level map would be mutable at runtime with nothing in Go able to
// prevent it.
//
// The musixmatch case takes its lane string from providers.Musixmatch rather
// than re-typing the literal, for the same reason laneLabel takes the detector's
// from detectorbackfill: that constant IS the persisted value, so a mark can
// never drift from the lane it marks.
//
// petitlyrics has NO case, and that absence is a finding rather than an
// omission. Its site serves only a raster site-header logo -- no SVG, no brand
// kit, no stated third-party usage terms (see docs/provider-terms.md). Vendoring
// a redraw from a general icon site is exactly what #601 warns against, so the
// lane renders as text, which is the documented degrade path rather than a gap.
func laneMark(lane string) string {
	switch lane {
	case detectorbackfill.LaneName:
		return markInstrumentalDetector
	case providers.Musixmatch:
		return markMusixmatch
	default:
		return markNone
	}
}
