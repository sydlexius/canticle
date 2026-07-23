package commands

import (
	"fmt"
	"time"
)

// unsyncedBeforeLayouts are the accepted --unsynced-before forms: a bare date or
// a full RFC3339 instant. A bare date is the expected operator input; RFC3339
// exists for a cohort boundary that needs to land mid-day or in a specific zone.
//
// A bare date is parsed as midnight UTC (time.Parse's default zone). Filesystem
// mtimes are compared as instants, so west of UTC a file written late on the
// previous local day still falls before a bare-date cutoff. That is documented in
// the flag help rather than silently switched to time.Local: a repair cohort is
// identified from UTC-stamped analysis, and an operator who needs a local-midnight
// boundary can express it exactly with the RFC3339 form.
var unsyncedBeforeLayouts = []string{"2006-01-02", time.RFC3339}

// scanSubcommandSelected reports whether a `scan` invocation selected one of its
// subcommands rather than running the scan itself. Kept beside the cutoff parser
// because its only caller is the --unsynced-before guard: the flag is declared on
// ScanCmd, so it binds on every subcommand, but only runScan acts on it.
//
// Enumerated explicitly rather than reflected over: a new subcommand added without
// a line here fails open (the flag is accepted and ignored), which is the same
// silent no-op the guard exists to prevent -- so the test enumerates every
// subcommand to keep this honest.
func scanSubcommandSelected(args ScanCmd) bool {
	return args.Results != nil ||
		args.Clear != nil ||
		args.Reconcile != nil ||
		args.ReconcileInstrumental != nil ||
		args.ReconcileInstrumentalRecalibrate != nil ||
		args.ReconcilePaths != nil ||
		args.ReconcileIdentity != nil ||
		args.ReconcileLRC != nil ||
		args.ReconcileMarkerProvenance != nil ||
		args.ReconcileDetectorStats != nil
}

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
