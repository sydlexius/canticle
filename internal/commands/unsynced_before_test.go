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
			if got.UTC().Format(time.RFC3339) != tc.wantUTC {
				t.Errorf("cutoff = %s; want %s", got.UTC().Format(time.RFC3339), tc.wantUTC)
			}
		})
	}
}

// TestResolveUnsyncedBefore_UpdateAlsoAccepted confirms --update satisfies the
// reopen requirement: it reopens the unsynced class too, so a dated --update is
// a coherent (if broader) repair run.
func TestResolveUnsyncedBefore_UpdateAlsoAccepted(t *testing.T) {
	got, err := resolveUnsyncedBefore("2026-04-01", false, true)
	if err != nil {
		t.Fatalf("unexpected error with --update: %v", err)
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
