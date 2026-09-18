package revalidate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Revalidate remediates only through realign.Apply, so these are end-to-end
// checks that a rejected .lrc does not leave its word-synced companion (#986)
// behind: a bad line timing must not keep word timings derived from it. A
// foreign companion stays.
func TestRemediationTakesTheOwnedCompanion(t *testing.T) {
	cases := []struct {
		name, body, elrc string
		gone             bool
	}{
		{"demote/owned", overrunBody, "[by:canticle]\n[00:10.00]<00:10.00>alpha\n", true},
		{"quarantine/owned", categoricalBody, "[by:canticle]\n[00:10.00]<00:10.00>alpha\n", true},
		{"quarantine/foreign", categoricalBody, "[00:10.00]<00:10.00>alpha\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, lrc := lib(t, tc.body)
			elrc := strings.TrimSuffix(lrc, ".lrc") + ".elrc"
			if err := os.WriteFile(elrc, []byte(tc.elrc), 0o600); err != nil {
				t.Fatalf("write companion: %v", err)
			}
			r, quarantine := newRevalidator(t, root, fixedDuration(), nil)
			plan, err := r.Plan(t.Context())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.Moves) != 1 {
				t.Fatalf("moves = %+v; want one remediation (the companion is never judged itself)", plan.Moves)
			}
			applyPlan(t, plan)
			if _, err := os.Stat(lrc); !os.IsNotExist(err) {
				t.Fatalf("the rejected .lrc is still in place")
			}
			_, serr := os.Stat(elrc)
			if gone := os.IsNotExist(serr); gone != tc.gone {
				t.Fatalf("companion removed=%v; want %v", gone, tc.gone)
			}
			if tc.gone {
				moved := filepath.Join(quarantine, "album", "track.elrc")
				if b, err := os.ReadFile(moved); err != nil || string(b) != tc.elrc {
					t.Errorf("companion not recoverable from quarantine at %s: %q, %v", moved, b, err)
				}
			}
		})
	}
}
