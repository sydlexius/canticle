package lyrics

import "strings"

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

// ReadInstrumentalProvenance reads the file at path and reports whether it is an
// instrumental marker and, if so, its provenance ([source:]/[dv:]). A legacy bare
// marker (no header) returns a zero-value InstrumentalProvenance. A file with no
// marker line returns isMarker=false. Any read error is returned.
func ReadInstrumentalProvenance(path string) (prov InstrumentalProvenance, isMarker bool, err error) {
	tags, lyricLines, err := parseLRCHeader(path)
	if err != nil {
		return InstrumentalProvenance{}, false, err
	}
	for _, t := range tags {
		switch strings.ToLower(t.key) {
		case "source":
			prov.Source = strings.TrimSpace(t.value)
		case "dv":
			prov.DetectorVersion = strings.TrimSpace(t.value)
		}
		if strings.Contains(t.raw, InstrumentalMarker) {
			isMarker = true
		}
	}
	for _, l := range lyricLines {
		if strings.Contains(l, InstrumentalMarker) {
			isMarker = true
			break
		}
	}
	if !isMarker {
		return InstrumentalProvenance{}, false, nil
	}
	return prov, true, nil
}
