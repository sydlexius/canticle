package scanner

import "github.com/sydlexius/canticle/internal/lyrics"

// reopenClasses is the set of settled lyric states a scan is willing to
// reconsider. Modeling re-check eligibility as classes (not a single bool) keeps
// the instrumental-provenance distinction expressible: a provider marker is
// authoritative and a detector marker is provisional, and only the requested
// classes are reopened. See #502.
//
// Synced covers a settled .lrc, the one class that is not a .txt state. Only a
// full --update reopens it: --upgrade exists to promote a track toward synced,
// so it has nothing to do for a track that is already there. See #575.
type reopenClasses struct {
	Unsynced                  bool
	ProvisionalInstrumental   bool
	AuthoritativeInstrumental bool
	Synced                    bool
}

// reopenClassesFor derives the reopen set from the scan flags. --update is a full
// re-fetch (reopens every class); --upgrade reopens unsynced .txt and provisional
// (detector-written) instrumental markers, but not authoritative provider markers
// and not a settled .lrc.
func reopenClassesFor(opts ScanOptions) reopenClasses {
	switch {
	case opts.Update:
		return reopenClasses{Unsynced: true, ProvisionalInstrumental: true, AuthoritativeInstrumental: true, Synced: true}
	case opts.Upgrade:
		return reopenClasses{Unsynced: true, ProvisionalInstrumental: true}
	default:
		return reopenClasses{}
	}
}

// instrumentalReopenable reports whether a settled instrumental .txt should be
// reconsidered. A detector-written (provisional) marker reopens when the
// ProvisionalInstrumental class is requested, or when the detector version has
// moved on since the marker was written (version invalidation, mirroring
// providers_version cache retirement) -- but only when both the current and the
// stored versions are known. A provider-written or legacy bare marker
// (authoritative) reopens only on a full --update.
func instrumentalReopenable(prov lyrics.InstrumentalProvenance, r reopenClasses, currentDetectorVersion string) bool {
	if prov.IsDetector() {
		if r.ProvisionalInstrumental {
			return true
		}
		return currentDetectorVersion != "" && prov.DetectorVersion != "" && prov.DetectorVersion != currentDetectorVersion
	}
	return r.AuthoritativeInstrumental
}
