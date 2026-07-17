package lyrics

import "testing"

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
