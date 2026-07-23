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

// repairWindowNotice builds the operator-facing line echoed at the start of a
// dated run. The cutoff is normalized to UTC so two operators comparing logs can
// never read the same instant under different labels, and so the run's scope
// stays recoverable from the log after a repair that takes hours.
// RFC3339Nano, not RFC3339: a cutoff parsed from an RFC3339 input keeps its
// fractional seconds and is ENFORCED at that precision, so a second-precision
// format would report a scope the run did not use. This line is the durable
// record of what a multi-hour repair covered; it has to match the cutoff exactly.
func repairWindowNotice(cutoff time.Time) string {
	return fmt.Sprintf("repair window: reopening only .txt sidecars modified before %s",
		cutoff.UTC().Format(time.RFC3339Nano))
}

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
// The cutoff requires --upgrade, and is REFUSED with --update.
//
// It requires a reopen flag because it can only NARROW a re-fetch that was
// already going to happen; with neither flag the flag would silently match
// nothing, and an error is more useful than a run that quietly does nothing.
//
// It refuses --update because the cutoff applies ONLY to .txt sidecars -- the
// scanner consults it inside the two .txt branches and nowhere else -- while
// --update also reopens settled .lrc files, which are then re-fetched whatever
// the cutoff says. The pairing is accepted-looking but incoherent: it presents
// as a scoped repair while rewriting the entire synced population, which is
// exactly the data a repair must not disturb (and, where a cohort is identified
// by sidecar mtime, destroys the evidence that made it identifiable). Refusing
// is better than warning: the run's own preview line would otherwise announce
// "reopening only .txt sidecars" while doing considerably more than that.
func resolveUnsyncedBefore(raw string, upgrade, update bool) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	// Checked before the reopen-flag requirement so that passing BOTH flags is
	// still refused: --upgrade does not redeem an --update sweep.
	if update {
		return time.Time{}, fmt.Errorf("--unsynced-before cannot be combined with --update: the cutoff applies only to .txt sidecars, so --update would still re-fetch every settled .lrc regardless of it. Use --upgrade for a scoped repair, or drop --unsynced-before for a full re-fetch")
	}
	if !upgrade {
		return time.Time{}, fmt.Errorf("--unsynced-before requires --upgrade: it narrows a .txt re-fetch, and without --upgrade no .txt sidecar is reopened")
	}
	for _, layout := range unsyncedBeforeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --unsynced-before %q: want a date (2026-04-01) or an RFC3339 instant (2026-04-01T00:00:00Z)", raw)
}
