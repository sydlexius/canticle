package scanner

// reopenClasses is the set of settled lyric states a scan is willing to
// reconsider.
//
// Unsynced covers every settled .txt: unsynced lyrics AND an instrumental
// marker, whoever wrote it (#553). Neither carries word timing, so neither is
// terminal, and provenance no longer decides eligibility: a provider can be
// wrong about a track being instrumental exactly as the detector can. The
// writer's no-downgrade guard (lyrics.ErrKeptBetter) is what makes reopening
// safe: a re-fetch that comes back worse never replaces what is on disk.
//
// Synced covers a settled .lrc. Only a full --update reopens it; a line-synced
// .lrc's path to word sync is the queue-driven word recheck (#982/#1048), not
// a per-scan reopen (the #684 disk-wake constraint).
type reopenClasses struct {
	Unsynced bool
	Synced   bool
}

// reopenClassesFor derives the reopen set from the scan flags. --update is a
// full re-fetch (reopens every class); --upgrade reopens every .txt.
func reopenClassesFor(opts ScanOptions) reopenClasses {
	switch {
	case opts.Update:
		return reopenClasses{Unsynced: true, Synced: true}
	case opts.Upgrade:
		return reopenClasses{Unsynced: true}
	default:
		return reopenClasses{}
	}
}
