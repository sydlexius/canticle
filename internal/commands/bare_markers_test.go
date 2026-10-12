package commands

import (
	"bytes"
	"strings"
	"testing"
)

// TestResolveBareMarkers pins the #618 guard: the flag needs --upgrade and is
// refused with --update, and unset it is inert.
func TestResolveBareMarkers(t *testing.T) {
	cases := []struct {
		name                  string
		bare, upgrade, update bool
		wantErr               string
	}{
		{"unset is inert", false, false, false, ""},
		{"unset with update is inert", false, false, true, ""},
		{"with upgrade", true, true, false, ""},
		{"without upgrade", true, false, false, "requires --upgrade"},
		{"with update", true, false, true, "cannot be combined with --update"},
		{"with upgrade and update", true, true, true, "cannot be combined with --update"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveBareMarkers(tc.bare, tc.upgrade, tc.update)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestRunScanCmd_BareMarkersRefusals drives the real command: a refused
// combination exits 1 with a message that carries no library path, and the flag
// is rejected on a scan subcommand instead of being silently ignored.
func TestRunScanCmd_BareMarkersRefusals(t *testing.T) {
	ctx, cfgPath, _, root := setupPurgeProvenance(t)
	cases := []struct {
		name string
		cmd  ScanCmd
		want string
	}{
		{"without upgrade", ScanCmd{ConfigPath: cfgPath, BareMarkers: true}, "requires --upgrade"},
		{"with update", ScanCmd{ConfigPath: cfgPath, BareMarkers: true, Update: true}, "cannot be combined with --update"},
		{"on a subcommand", ScanCmd{ConfigPath: cfgPath, BareMarkers: true, Upgrade: true, Results: &ScanResultsCmd{}}, "applies only to `scan` itself"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if rc := runScanCmd(ctx, &out, tc.cmd); rc != 1 {
				t.Fatalf("rc = %d, want 1; out=%s", rc, out.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output %q, want containing %q", out.String(), tc.want)
			}
			if strings.Contains(out.String(), root) || strings.Contains(out.String(), "ArtistA") {
				t.Errorf("output leaks a library path or name: %q", out.String())
			}
		})
	}
}
