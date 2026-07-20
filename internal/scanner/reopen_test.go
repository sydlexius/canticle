package scanner

import (
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
)

func TestReopenClassesFor(t *testing.T) {
	cases := []struct {
		name            string
		update, upgrade bool
		want            reopenClasses
	}{
		{"neither", false, false, reopenClasses{}},
		// --upgrade promotes a track toward synced, so it must never set Synced:
		// there is nothing above a settled .lrc to promote it to (#575).
		{"upgrade", false, true, reopenClasses{Unsynced: true, ProvisionalInstrumental: true}},
		{"update", true, false, reopenClasses{Unsynced: true, ProvisionalInstrumental: true, AuthoritativeInstrumental: true, Synced: true}},
		{"both", true, true, reopenClasses{Unsynced: true, ProvisionalInstrumental: true, AuthoritativeInstrumental: true, Synced: true}},
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

func TestInstrumentalReopenable(t *testing.T) {
	det := func(dv string) lyrics.InstrumentalProvenance {
		return lyrics.InstrumentalProvenance{Source: lyrics.SourceDetector, DetectorVersion: dv}
	}
	provider := lyrics.InstrumentalProvenance{Source: "musixmatch"}
	legacy := lyrics.InstrumentalProvenance{} // bare marker, no header

	cases := []struct {
		name       string
		prov       lyrics.InstrumentalProvenance
		r          reopenClasses
		curVersion string
		want       bool
	}{
		// Detector marker + upgrade -> provisional class reopens it.
		{"detector_upgrade", det("1.0"), reopenClasses{Unsynced: true, ProvisionalInstrumental: true}, "1.0", true},
		// Detector marker, no upgrade, same version -> stays.
		{"detector_neither_sameversion", det("1.0"), reopenClasses{}, "1.0", false},
		// Detector marker, no upgrade, but detector version moved on -> invalidated.
		{"detector_versionbump", det("1.0"), reopenClasses{}, "2.0", true},
		// Detector marker, no upgrade, no current version known (dir mode) -> no invalidation.
		{"detector_noversion", det("1.0"), reopenClasses{}, "", false},
		// Detector marker with empty dv, version known -> cannot compare -> not invalidated by version.
		{"detector_emptydv", det(""), reopenClasses{}, "2.0", false},
		// Provider marker: only a full --update (AuthoritativeInstrumental) reopens it.
		{"provider_upgrade", provider, reopenClasses{Unsynced: true, ProvisionalInstrumental: true}, "2.0", false},
		{"provider_update", provider, reopenClasses{Unsynced: true, ProvisionalInstrumental: true, AuthoritativeInstrumental: true}, "2.0", true},
		// Legacy bare marker behaves like a provider (authoritative) marker.
		{"legacy_upgrade", legacy, reopenClasses{Unsynced: true, ProvisionalInstrumental: true}, "2.0", false},
		{"legacy_update", legacy, reopenClasses{AuthoritativeInstrumental: true}, "2.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := instrumentalReopenable(tc.prov, tc.r, tc.curVersion); got != tc.want {
				t.Fatalf("instrumentalReopenable(%+v, %+v, %q) = %v, want %v", tc.prov, tc.r, tc.curVersion, got, tc.want)
			}
		})
	}
}
