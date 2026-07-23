package commands

import (
	"fmt"
	"time"
)

// unsyncedBeforeLayouts are the accepted --unsynced-before forms: a bare date
// (interpreted as midnight UTC) or a full RFC3339 instant. A bare date is the
// expected operator input; RFC3339 exists for a cohort boundary that needs to
// land mid-day.
var unsyncedBeforeLayouts = []string{"2006-01-02", time.RFC3339}

// resolveUnsyncedBefore parses the scan --unsynced-before cutoff into the
// ScanOptions filter (#617). An empty value returns the zero time, which
// disables the filter so an ordinary --upgrade is unaffected.
//
// The cutoff requires --upgrade or --update because it can only NARROW an
// unsynced reopen. Without one of those the unsynced class is never reopened at
// all, so the flag would silently do nothing -- an error is more useful than a
// run that quietly matches zero files.
func resolveUnsyncedBefore(raw string, upgrade, update bool) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if !upgrade && !update {
		return time.Time{}, fmt.Errorf("--unsynced-before requires --upgrade or --update: it narrows an unsynced re-fetch, and without one of those no unsynced sidecar is reopened")
	}
	for _, layout := range unsyncedBeforeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --unsynced-before %q: want a date (2026-04-01) or an RFC3339 instant (2026-04-01T00:00:00Z)", raw)
}
