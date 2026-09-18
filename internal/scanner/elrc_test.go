package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/sidecar"
	"github.com/sydlexius/canticle/internal/testutil"
)

// The word-synced companion's Kind is active since #986 slice 4c, and
// ActivateForTest cannot switch it off, so there is no inactive state left to
// run. The table (and the no-op activation) goes in slice 5. Not parallel:
// ActivateForTest writes a package-global.
var companionFlagStates = []struct {
	name   string
	active bool
}{
	{"kind active", true},
}

const (
	ownedCompanionBody   = "[by:canticle]\n[ti:x]\n[00:01.00]<00:01.00>word\n"
	foreignCompanionBody = "[00:01.00]<00:01.00>word\n"
)

// writeCompanionTrack writes a tagged FLAC named song.flac plus the named
// sidecars, and returns the audio path.
func writeCompanionTrack(t *testing.T, dir string, sidecars map[string]string) string {
	t.Helper()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 44100*30,
		map[string]string{"ARTIST": testArtist, "TITLE": testTitle}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	for name, body := range sidecars {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "song.flac")
}

// A settled .lrc WITHOUT a companion stays settled, even on --upgrade. Nothing
// records that a provider has no word data for a track, so reopening on a
// missing companion would re-read every such file on every scan forever; the
// re-examination is #982's operator-sized pass. The #684 duration bank for the
// skipped file must be unchanged.
func TestScan_LRCWithoutCompanionStaysSettled(t *testing.T) {
	for _, st := range companionFlagStates {
		for _, withCompanion := range []bool{false, true} {
			name := st.name + "/lrc only"
			if withCompanion {
				name = st.name + "/lrc with owned companion"
			}
			t.Run(name, func(t *testing.T) {
				if st.active {
					sidecar.ActivateForTest(t, sidecar.KindWordSynced)
				}
				dir := t.TempDir()
				files := map[string]string{"song.lrc": "[00:01.00]words\n"}
				if withCompanion {
					files["song.elrc"] = ownedCompanionBody
				}
				writeCompanionTrack(t, dir, files)

				store := &recordingDurationStore{}
				sc := NewScanner(WithDurationStore(store))
				res, err := sc.ScanLibrary(context.Background(), dir,
					ScanOptions{MaxDepth: 1, EnrichRecording: true, Upgrade: true})
				if err != nil {
					t.Fatalf("ScanLibrary: %v", err)
				}
				if len(res) != 0 {
					t.Fatalf("companion=%v: got %d results %+v; want 0 (a settled .lrc is not reopened for word timings)",
						withCompanion, len(res), res)
				}
				if len(store.paths) != 1 {
					t.Fatalf("companion=%v: banked %d durations; want 1 (#684 fill for a skipped file must be unchanged)",
						withCompanion, len(store.paths))
				}
			})
		}
	}
}

// A lone companion (no .lrc) is NOT lyric coverage: it describes word timings
// for a line-synced file that is not there. Its audio must be emitted as
// pending work targeting the .lrc, owned or foreign, in both flag states.
func TestScan_LoneCompanionDoesNotSettleItsAudio(t *testing.T) {
	for _, st := range companionFlagStates {
		for _, c := range []struct{ name, body string }{
			{"owned companion", ownedCompanionBody},
			{"foreign companion", foreignCompanionBody},
		} {
			body := c.body
			t.Run(st.name+"/"+c.name, func(t *testing.T) {
				if st.active {
					sidecar.ActivateForTest(t, sidecar.KindWordSynced)
				}
				dir := t.TempDir()
				audio := writeCompanionTrack(t, dir, map[string]string{"song.elrc": body})

				res, err := NewScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
				if err != nil {
					t.Fatalf("ScanLibrary: %v", err)
				}
				if len(res) != 1 {
					t.Fatalf("got %d results %+v; want 1 (a lone companion must not settle its audio)", len(res), res)
				}
				if res[0].FilePath != audio || res[0].Status != "pending" {
					t.Fatalf("result = {%q, %q}; want {%q, pending}", res[0].FilePath, res[0].Status, audio)
				}
				if res[0].Filename != "song"+sidecar.ExtLineSynced {
					t.Fatalf("Filename = %q; want the line-synced target song.lrc", res[0].Filename)
				}
			})
		}
	}
}
