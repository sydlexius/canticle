package lyrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstrumentalProvenance_IsDetector(t *testing.T) {
	if !(InstrumentalProvenance{Source: SourceDetector}).IsDetector() {
		t.Fatal("marker with SourceDetector should be detector-sourced")
	}
	if (InstrumentalProvenance{Source: "musixmatch"}).IsDetector() {
		t.Fatal("provider source must not be detector-sourced")
	}
	if (InstrumentalProvenance{}).IsDetector() {
		t.Fatal("empty (legacy) source must not be detector-sourced")
	}
}

func TestReadInstrumentalProvenance(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name       string
		body       string
		wantMarker bool
		wantSource string
		wantDV     string
	}{
		{"detector", "[by:canticle]\n[source:canticle-detector]\n[dv:9.9.9]\n" + InstrumentalMarker + "\n", true, "canticle-detector", "9.9.9"},
		{"provider", "[by:canticle]\n[source:musixmatch]\n" + InstrumentalMarker + "\n", true, "musixmatch", ""},
		{"legacy-bare", InstrumentalMarker + "\n", true, "", ""},
		{"not-a-marker", "[00:01.00]la la\n", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".txt")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			prov, isMarker, err := ReadInstrumentalProvenance(p)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if isMarker != tc.wantMarker || prov.Source != tc.wantSource || prov.DetectorVersion != tc.wantDV {
				t.Fatalf("got (marker=%v src=%q dv=%q), want (marker=%v src=%q dv=%q)",
					isMarker, prov.Source, prov.DetectorVersion, tc.wantMarker, tc.wantSource, tc.wantDV)
			}
		})
	}
}
