package lyrics

import (
	"os"
	"path/filepath"
	"strings"
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

func TestWriteMarkerProvenance(t *testing.T) {
	t.Run("bare_marker_gets_detector_header", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "track.txt")
		if err := os.WriteFile(p, []byte(InstrumentalMarker+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		changed, err := WriteMarkerProvenance(p, InstrumentalProvenance{Source: SourceDetector, DetectorVersion: "1.2.3"})
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v; want changed=true nil", changed, err)
		}
		prov, isMarker, err := ReadInstrumentalProvenance(p)
		if err != nil || !isMarker {
			t.Fatalf("re-read: isMarker=%v err=%v", isMarker, err)
		}
		if prov.Source != SourceDetector || prov.DetectorVersion != "1.2.3" {
			t.Fatalf("provenance = %+v; want detector/1.2.3", prov)
		}
		data, _ := os.ReadFile(p)
		if !strings.Contains(string(data), InstrumentalMarker) {
			t.Fatalf("marker line lost:\n%s", data)
		}
	})

	t.Run("empty_dv_omits_dv_tag", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "track.txt")
		_ = os.WriteFile(p, []byte(InstrumentalMarker+"\n"), 0o644)
		changed, err := WriteMarkerProvenance(p, InstrumentalProvenance{Source: SourceDetector})
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "[dv:") {
			t.Fatalf("empty dv must not emit [dv:]:\n%s", data)
		}
		if !strings.Contains(string(data), "[source:canticle-detector]") {
			t.Fatalf("missing source header:\n%s", data)
		}
	})

	t.Run("already_headed_is_noop", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "track.txt")
		orig := "[by:canticle]\n[source:canticle-detector]\n[dv:9.9.9]\n" + InstrumentalMarker + "\n"
		_ = os.WriteFile(p, []byte(orig), 0o644)
		changed, err := WriteMarkerProvenance(p, InstrumentalProvenance{Source: SourceDetector, DetectorVersion: "1.2.3"})
		if err != nil || changed {
			t.Fatalf("changed=%v err=%v; want changed=false (idempotent)", changed, err)
		}
		data, _ := os.ReadFile(p)
		if string(data) != orig {
			t.Fatalf("already-headed file must be untouched:\n%s", data)
		}
	})

	t.Run("not_a_marker_is_noop", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "lyrics.txt")
		orig := "just some unsynced lyrics\n"
		_ = os.WriteFile(p, []byte(orig), 0o644)
		changed, err := WriteMarkerProvenance(p, InstrumentalProvenance{Source: SourceDetector})
		if err != nil || changed {
			t.Fatalf("changed=%v err=%v; want changed=false (not a marker)", changed, err)
		}
		data, _ := os.ReadFile(p)
		if string(data) != orig {
			t.Fatalf("non-marker must be untouched:\n%s", data)
		}
	})

	t.Run("symlink_is_skipped", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		_ = os.WriteFile(real, []byte(InstrumentalMarker+"\n"), 0o644)
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		changed, err := WriteMarkerProvenance(link, InstrumentalProvenance{Source: SourceDetector})
		if err != nil || changed {
			t.Fatalf("changed=%v err=%v; want changed=false (symlink skipped)", changed, err)
		}
	})
}
