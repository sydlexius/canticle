package lyrics

import (
	"bytes"
	"strings"

	"github.com/sydlexius/canticle/internal/sidecar"
)

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
	// Case-insensitive: the writer emits lowercase, but a hand-edited "Manual"
	// is still an operator's verdict and must not be displaced.
	return strings.EqualFold(p.Source, SourceManual)
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

// OwnedCompanions is every canticle-owned word-synced companion variant beside
// the line-synced path fp, whether or not fp itself exists: exactly the set a
// write to fp removes. Exported so the manual-mark backup inventories the same
// files the writer will delete rather than re-deriving the rule.
func OwnedCompanions(fp string, l sidecar.Listing) []string { return ownedCompanions(fp, l) }

// ReadRegularNoFollow reads path through one no-follow handle that must fstat
// as a regular file and refuses a file over limit bytes. Exported for the
// manual-mark backup, which must not follow a symlink swapped in after a stat.
func ReadRegularNoFollow(path string, limit int64) ([]byte, error) {
	return readRegularNoFollow(path, limit)
}
