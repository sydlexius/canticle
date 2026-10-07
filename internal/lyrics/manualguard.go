package lyrics

import (
	"errors"
	"log/slog"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// refuseManualMarker is the writer's hard guard for a manual instrumental
// marker (#1218). It judges the .txt files this write would replace or remove
// (the exact target when it is a .txt, plus every case variant of the opposite
// .txt) and refuses when any is a manual marker. Unlike the rung comparison it
// ignores the candidate's rung and SetForceOverwrite: a synced .lrc outranks a
// marker, and --update forces, yet neither may displace a hand-placed verdict.
func (w *LRCWriter) refuseManualMarker(fp string, l sidecar.Listing) *KeptError {
	if w.allowManual {
		return nil
	}
	txts := staleSidecars(fp, l)
	if sidecar.KindOf(fp) != sidecar.KindLineSynced {
		txts = append(txts, fp)
	}
	for _, p := range txts {
		if sidecar.KindOf(p) == sidecar.KindLineSynced || !ManualMarkerOnDisk(p) {
			continue
		}
		slog.Debug("keeping manual instrumental marker", "path", p)
		return &KeptError{OnDisk: RungInstrumental, Judged: true, Manual: true}
	}
	return nil
}

// WriteManualMarker is the one way to write a manual instrumental marker over
// an existing manual marker or over lyrics (the mark action, #1218). It is not
// SetForceOverwrite: it forces the replacement and lifts the manual guard, but
// only for a song that is itself a manual instrumental marker, so it cannot be
// used to write anything else over a manual one.
func (w *LRCWriter) WriteManualMarker(song models.Song, filename string, outdir string) error {
	if song.Track.Instrumental != 1 || song.WinningLane != ManualLaneName {
		return errors.New("manual marker write needs an instrumental song from the manual lane")
	}
	opt := *w
	opt.force = true
	opt.allowManual = true
	return opt.WriteLRC(song, filename, outdir)
}
