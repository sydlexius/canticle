package lyrics

import (
	"bytes"
	"errors"
	"os"
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
	return isManualMarker(data)
}

// isManualMarker reports whether data is an instrumental marker carrying
// [source:manual].
func isManualMarker(data []byte) bool {
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

// ErrMarkerChanged is returned by RemoveManualMarker when the entry at the path
// is no longer the file that was read and confirmed a manual marker. Nothing was
// removed; the caller may retry.
var ErrMarkerChanged = errors.New("lyrics: the manual marker changed while being removed; nothing was removed")

// RemoveManualMarker removes path only if it is, at this moment, an instrumental
// marker carrying [source:manual], and reports whether it did. A file that is
// not a manual marker (real lyrics written since, a symlink, a missing file) is
// left alone and is not an error. The marker is read through one no-follow
// handle, and the entry at path must still be that same file (os.SameFile)
// immediately before the unlink, else ErrMarkerChanged and nothing is removed.
// POSIX has no compare-and-unlink, so a file saved over the marker in the few
// instructions between that last check and os.Remove is still unlinked.
func (w *LRCWriter) RemoveManualMarker(path string) (bool, error) {
	data, hfi, err := readRegularNoFollowInfo(path, maxManualMarkerBytes)
	if err != nil || !isManualMarker(data) {
		return false, nil
	}
	return w.removeCheckedMarker(path, hfi)
}

// removeCheckedMarker unlinks path if it is still the file checked describes.
// The self-write record is made only once the unlink will be attempted.
func (w *LRCWriter) removeCheckedMarker(path string, checked os.FileInfo) (bool, error) {
	cur, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !os.SameFile(cur, checked) {
		return false, ErrMarkerChanged
	}
	w.selfWrites.Record(path)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RemoveReplacedSidecar removes a lyric file the manual mark backed up and the
// marker write left behind (a same-extension case variant such as Song.TXT,
// which the writer's own cleanup does not touch). It records the path with the
// writer's selfwrite registry first, never removes a manual marker, and uses
// os.Remove so a symlink is unlinked, never followed. A missing file is not an
// error.
func (w *LRCWriter) RemoveReplacedSidecar(path string) error {
	w.selfWrites.Record(path)
	if ManualMarkerOnDisk(path) {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
