package lyrics

import "bytes"

// SourceManual is the [source:] token of an instrumental marker an operator
// placed by hand (#1218). ManualLaneName is the models.Song.WinningLane that
// makes the writer stamp it (the writer uses the lane name as the source token).
const (
	SourceManual   = "manual"
	ManualLaneName = "manual"
)

// maxManualMarkerBytes caps the probe read; a real marker is a few lines.
const maxManualMarkerBytes = 64 << 10

// IsManual reports whether the marker was placed by hand.
func (p InstrumentalProvenance) IsManual() bool {
	return p.Source == SourceManual
}

// ManualMarkerOnDisk reports whether path is an instrumental marker carrying
// [source:manual]. The file is read only through readRegularNoFollow: a
// symlink is never followed and a FIFO never blocks. Anything unreadable,
// oversized or non-regular reads as false (the ordinary rung guard still
// applies to it).
func ManualMarkerOnDisk(path string) bool {
	data, err := readRegularNoFollow(path, maxManualMarkerBytes)
	if err != nil {
		return false
	}
	tags, lines, err := parseLRCHeaderFrom(bytes.NewReader(data))
	if err != nil {
		return false
	}
	prov, isMarker := provenanceOf(tags, lines)
	return isMarker && prov.IsManual()
}
