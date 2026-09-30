package scanner

import "testing"

func TestReopenClassesFor(t *testing.T) {
	cases := []struct {
		name            string
		update, upgrade bool
		want            reopenClasses
	}{
		{"neither", false, false, reopenClasses{}},
		// --upgrade never sets Synced: a line .lrc's path to word sync is the
		// queue-driven word recheck, not a per-scan reopen (#575, #553).
		{"upgrade", false, true, reopenClasses{Unsynced: true}},
		{"update", true, false, reopenClasses{Unsynced: true, Synced: true}},
		{"both", true, true, reopenClasses{Unsynced: true, Synced: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reopenClassesFor(ScanOptions{Update: tc.update, Upgrade: tc.upgrade})
			if got != tc.want {
				t.Fatalf("reopenClassesFor(update=%v,upgrade=%v) = %+v, want %+v", tc.update, tc.upgrade, got, tc.want)
			}
		})
	}
}
