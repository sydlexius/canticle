package lyrics

// SourceDetector is the [source:] token stamped into an instrumental marker that
// the audio detector wrote. Any other source token (a provider lane name) marks
// a provider-written, editorial-authoritative instrumental. Kept here so the
// writer and the scanner agree on one spelling.
const SourceDetector = "canticle-detector"

// InstrumentalProvenance is the provenance read from an instrumental .txt marker.
// A detector marker carries Source == SourceDetector and a DetectorVersion; a
// provider marker carries the provider lane name and an empty DetectorVersion.
type InstrumentalProvenance struct {
	Source          string
	DetectorVersion string
}

// IsDetector reports whether the marker was written by the audio detector, i.e.
// whether it is provisional (re-checkable) rather than editorially terminal.
func (p InstrumentalProvenance) IsDetector() bool {
	return p.Source == SourceDetector
}
