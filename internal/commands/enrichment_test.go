package commands

import "testing"

// TestResolveEnrichOverride covers the scan --enrich/--no-enrich mutual-exclusion
// resolution into a tri-state *bool (nil = no override).
func TestResolveEnrichOverride(t *testing.T) {
	cases := []struct {
		name      string
		enrich    bool
		noEnrich  bool
		wantNil   bool
		wantVal   bool
		wantError bool
	}{
		{name: "neither set -> nil", wantNil: true},
		{name: "enrich -> true", enrich: true, wantVal: true},
		{name: "no-enrich -> false", noEnrich: true, wantVal: false},
		{name: "both set -> error", enrich: true, noEnrich: true, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveEnrichOverride(tc.enrich, tc.noEnrich)
			if tc.wantError {
				if err == nil {
					t.Fatal("expected error for conflicting --enrich/--no-enrich; got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNil {
				if got != nil {
					t.Errorf("override = %v; want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("override = nil; want %v", tc.wantVal)
			}
			if *got != tc.wantVal {
				t.Errorf("override = %v; want %v", *got, tc.wantVal)
			}
		})
	}
}
