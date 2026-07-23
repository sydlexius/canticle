package commands

import (
	"testing"
	"time"
)

// TestResolveUnsyncedBefore covers parsing the scan --unsynced-before cutoff into
// the ScanOptions filter (#617). Empty means no filter, so an ordinary --upgrade
// keeps reopening every unsynced sidecar.
func TestResolveUnsyncedBefore(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		upgrade   bool
		wantZero  bool
		wantUTC   string
		wantError bool
	}{
		{name: "empty -> no filter", in: "", upgrade: true, wantZero: true},
		{name: "date only -> midnight UTC", in: "2026-04-01", upgrade: true, wantUTC: "2026-04-01T00:00:00Z"},
		{name: "rfc3339 -> exact instant", in: "2026-04-01T12:30:00Z", upgrade: true, wantUTC: "2026-04-01T12:30:00Z"},
		// time.Parse accepts fractional seconds against the RFC3339 layout, and the
		// cutoff is APPLIED at that precision, so it must survive the round trip
		// here and in the notice -- otherwise the logged scope disagrees with the
		// scope actually used.
		{name: "fractional seconds are preserved", in: "2026-04-01T12:30:00.123Z", upgrade: true, wantUTC: "2026-04-01T12:30:00.123Z"},
		{name: "garbage -> error", in: "not-a-date", upgrade: true, wantError: true},
		{name: "wrong order -> error", in: "01-04-2026", upgrade: true, wantError: true},
		// The filter only narrows an unsynced reopen, so requiring it to accompany
		// a reopen flag turns a silent no-op into an actionable error.
		{name: "without upgrade -> error", in: "2026-04-01", upgrade: false, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveUnsyncedBefore(tc.in, tc.upgrade, false)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error for %q (upgrade=%v); got nil", tc.in, tc.upgrade)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantZero {
				if !got.IsZero() {
					t.Errorf("cutoff = %s; want zero (filter disabled)", got)
				}
				return
			}
			// RFC3339Nano, not RFC3339: formatting with the second-precision layout
			// would DISCARD any fractional part before comparing, so the
			// fractional-seconds case above would pass no matter what resolve
			// returned. Assert at the precision the value actually carries.
			if got.UTC().Format(time.RFC3339Nano) != tc.wantUTC {
				t.Errorf("cutoff = %s; want %s", got.UTC().Format(time.RFC3339Nano), tc.wantUTC)
			}
		})
	}
}

// TestResolveUnsyncedBefore_UpdateRejected covers the --update pairing, which is
// refused rather than merely discouraged.
//
// This inverts an earlier assertion that --update was "a coherent (if broader)
// repair run". It is not. The cutoff can only narrow a .txt reopen -- the scanner
// applies it inside the two .txt branches and nowhere else -- so under --update
// every settled .lrc is re-fetched whatever the cutoff says. That is the opposite
// of a scoped repair, and it rewrites precisely the files a repair must leave
// alone, while the run's own preview line claims it is touching only .txt.
// Documenting that was not enough: a program that prints a false statement about
// what it is about to do has to stop doing it, not explain itself elsewhere.
func TestResolveUnsyncedBefore_UpdateRejected(t *testing.T) {
	if _, err := resolveUnsyncedBefore("2026-04-01", false, true); err == nil {
		t.Fatal("expected --update + --unsynced-before to be rejected; got nil error")
	}
	// --upgrade --update together is still --update's unscoped sweep, so the
	// rejection must not be escapable by passing both.
	if _, err := resolveUnsyncedBefore("2026-04-01", true, true); err == nil {
		t.Fatal("expected --upgrade --update + --unsynced-before to be rejected; got nil error")
	}
	// The plain --upgrade path must keep working.
	got, err := resolveUnsyncedBefore("2026-04-01", true, false)
	if err != nil {
		t.Fatalf("unexpected error with --upgrade alone: %v", err)
	}
	if got.IsZero() {
		t.Fatal("cutoff = zero; want the parsed instant")
	}
}

// TestScanSubcommandSelected covers the guard that keeps --unsynced-before from
// being silently accepted-and-ignored on a `scan` subcommand (it is parsed on
// ScanCmd, so it binds syntactically everywhere, but only runScan consults it).
func TestScanSubcommandSelected(t *testing.T) {
	if scanSubcommandSelected(ScanCmd{}) {
		t.Error("bare scan reported as a subcommand; --unsynced-before must be allowed there")
	}
	cases := []struct {
		name string
		args ScanCmd
	}{
		{"results", ScanCmd{Results: &ScanResultsCmd{}}},
		{"clear", ScanCmd{Clear: &ScanClearCmd{}}},
		{"reconcile", ScanCmd{Reconcile: &ScanReconcileCmd{}}},
		{"reconcile-instrumental", ScanCmd{ReconcileInstrumental: &ScanReconcileInstrumentalCmd{}}},
		{"reconcile-instrumental-recalibrate", ScanCmd{ReconcileInstrumentalRecalibrate: &ScanReconcileInstrumentalRecalibrateCmd{}}},
		{"reconcile-paths", ScanCmd{ReconcilePaths: &ScanReconcilePathsCmd{}}},
		{"reconcile-identity", ScanCmd{ReconcileIdentity: &ScanReconcileIdentityCmd{}}},
		{"reconcile-lrc", ScanCmd{ReconcileLRC: &ScanReconcileLRCCmd{}}},
		{"reconcile-marker-provenance", ScanCmd{ReconcileMarkerProvenance: &ScanReconcileMarkerProvenanceCmd{}}},
		{"reconcile-detector-stats", ScanCmd{ReconcileDetectorStats: &ScanReconcileDetectorStatsCmd{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !scanSubcommandSelected(tc.args) {
				t.Errorf("%s not detected as a subcommand; --unsynced-before would be silently ignored", tc.name)
			}
		})
	}
}

// TestRepairWindowNotice covers the operator-facing line echoed at the start of a
// dated run. It exists so the run's scope is confirmable before a long repair and
// recoverable from the log afterwards, so the resolved instant must appear in UTC
// regardless of the zone the operator supplied.
func TestRepairWindowNotice(t *testing.T) {
	cases := []struct {
		name   string
		cutoff time.Time
		want   string
	}{
		{
			name:   "utc instant is echoed as given",
			cutoff: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			want:   "repair window: reopening only .txt sidecars modified before 2026-04-01T00:00:00Z",
		},
		{
			// A non-UTC cutoff must be normalized, so two operators comparing logs
			// are never reading the same instant under different labels.
			name:   "non-utc instant is normalized to UTC",
			cutoff: time.Date(2026, 4, 1, 0, 0, 0, 0, time.FixedZone("PDT", -7*60*60)),
			want:   "repair window: reopening only .txt sidecars modified before 2026-04-01T07:00:00Z",
		},
		{
			// The cutoff is APPLIED at sub-second precision, so the notice has to
			// report it at that precision too. A second-precision format would
			// print ...00Z for a cutoff enforced at ...00.123Z, leaving the logged
			// scope quietly disagreeing with the scope actually used -- and this
			// line is the only durable record of what a long repair run covered.
			name:   "fractional seconds survive into the notice",
			cutoff: time.Date(2026, 4, 1, 0, 0, 0, 123000000, time.UTC),
			want:   "repair window: reopening only .txt sidecars modified before 2026-04-01T00:00:00.123Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repairWindowNotice(tc.cutoff); got != tc.want {
				t.Errorf("repairWindowNotice()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
