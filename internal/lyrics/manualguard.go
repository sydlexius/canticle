package lyrics

import (
	"errors"
	"log/slog"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// refuseManualMarker is the writer's hard guard for a manual instrumental
// marker (#1218). It judges the .txt files this write would replace or remove
// (the exact target when it is a .txt and its same-extension case variants,
// plus every case variant of the opposite .txt) and refuses when any is a manual marker. Unlike the rung comparison it
// ignores the candidate's rung and SetForceOverwrite: a synced .lrc outranks a
// marker, and --update forces, yet neither may displace a hand-placed verdict.
func (w *LRCWriter) refuseManualMarker(fp string, l sidecar.Listing) *KeptError {
	if w.allowManual {
		return nil
	}
	txts := staleSidecars(fp, l)
	if sidecar.KindOf(fp) != sidecar.KindLineSynced {
		// The exact target plus its same-extension case variants: on a
		// case-sensitive filesystem a write to song.txt beside a manual
		// Song.TXT would otherwise leave two markers. The listing is in hand.
		txts = append(txts, l.Variants(fp)...) // includes fp itself when it exists
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

// at calls the test-only race hook, if any.
func (w *LRCWriter) at(stage string) {
	if w.racePoint != nil {
		w.racePoint(stage)
	}
}

// recheckManualMarker repeats the up-front probe just before a destructive
// step, and also reads the target itself: a marker that appeared after the
// directory listing was taken is not in it.
func (w *LRCWriter) recheckManualMarker(fp string, l sidecar.Listing) *KeptError {
	if k := w.refuseManualMarker(fp, l); k != nil {
		return k
	}
	if !w.allowManual && sidecar.KindOf(fp) != sidecar.KindLineSynced && ManualMarkerOnDisk(fp) {
		return &KeptError{OnDisk: RungInstrumental, Judged: true, Manual: true}
	}
	return nil
}
