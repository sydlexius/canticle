package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

const manualMarkerBody = "[by:canticle]\n[source:manual]\n" + InstrumentalMarker + "\n"

func manualSong() models.Song {
	return models.Song{Track: models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1}, WinningLane: ManualLaneName}
}

func mustKeptManual(t *testing.T, err error) {
	t.Helper()
	var k *KeptError
	if !errors.Is(err, ErrKeptBetter) || !errors.As(err, &k) || !k.Manual || k.OnDisk != RungInstrumental {
		t.Fatalf("err = %v, want a manual *KeptError", err)
	}
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every write path leaves a manual marker byte-identical, including a synced
// .lrc candidate whose rung outranks the marker and a forced write.
func TestWriteLRC_ManualMarkerNeverReplaced(t *testing.T) {
	instrumental, unsynced, line := rungSongs()
	writes := map[string]func(w *LRCWriter, s models.Song, dir string) error{
		"fetch":   func(w *LRCWriter, s models.Song, dir string) error { return w.WriteLRC(s, "song.lrc", dir) },
		"upgrade": func(w *LRCWriter, s models.Song, dir string) error { return w.WriteLRCNoDowngrade(s, "song.lrc", dir) },
		"forced": func(w *LRCWriter, s models.Song, dir string) error {
			w.SetForceOverwrite(true)
			return w.WriteLRC(s, "song.lrc", dir)
		},
		"forced txt target": func(w *LRCWriter, s models.Song, dir string) error {
			w.SetForceOverwrite(true)
			return w.WriteLRC(s, "song.txt", dir)
		},
	}
	songs := map[string]models.Song{"line": line, "unsynced": unsynced, "provider instrumental": instrumental}
	for wname, write := range writes {
		for sname, song := range songs {
			t.Run(wname+"/"+sname, func(t *testing.T) {
				dir := t.TempDir()
				marker := filepath.Join(dir, "song.txt")
				seed(t, marker, manualMarkerBody)
				mustKeptManual(t, write(NewLRCWriter(), song, dir))
				if got := readText(t, marker); got != manualMarkerBody {
					t.Errorf("marker changed: %q", got)
				}
				if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err == nil {
					t.Error("a .lrc was written beside the manual marker")
				}
			})
		}
	}
}

// Control group: a detector- or provider-written marker still yields to a
// better candidate, exactly as before.
func TestWriteLRC_NonManualMarkerBehavesAsBefore(t *testing.T) {
	_, _, line := rungSongs()
	for _, src := range []string{SourceDetector, "musixmatch"} {
		dir := t.TempDir()
		seed(t, filepath.Join(dir, "song.txt"), "[by:canticle]\n[source:"+src+"]\n"+InstrumentalMarker+"\n")
		if err := NewLRCWriter().WriteLRC(line, "song.lrc", dir); err != nil {
			t.Fatalf("%s: upgrade over marker: %v", src, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "song.txt")); err == nil {
			t.Errorf("%s: marker should have been replaced by the .lrc", src)
		}
	}
}

// WriteManualMarker is the one bypass, and only for a manual instrumental song.
func TestWriteManualMarker_IsTheOnlyBypass(t *testing.T) {
	instrumental, _, line := rungSongs()
	dir := t.TempDir()
	marker := filepath.Join(dir, "song.txt")
	seed(t, marker, manualMarkerBody)
	seed(t, filepath.Join(dir, "song.lrc"), "[00:01.00]old\n")

	w := NewLRCWriter()
	if err := w.WriteManualMarker(instrumental, "song.txt", dir); err == nil {
		t.Fatal("a non-manual song was accepted by the opt-in")
	}
	if err := w.WriteManualMarker(manualSong(), "song.txt", dir); err != nil {
		t.Fatalf("WriteManualMarker over a manual marker: %v", err)
	}
	if got := readText(t, marker); got != manualMarkerBody {
		t.Errorf("marker = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err == nil {
		t.Error("opt-in write should have replaced the lyrics")
	}
	// The opt-in does not leak onto the writer it was called on.
	mustKeptManual(t, w.WriteLRC(line, "song.lrc", dir))
}

// A symlinked or non-regular marker path is never followed.
func TestManualMarkerOnDisk_NeverFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	seed(t, real, manualMarkerBody)
	link := filepath.Join(dir, "song.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if !ManualMarkerOnDisk(real) {
		t.Fatal("regular manual marker not recognized")
	}
	if ManualMarkerOnDisk(link) {
		t.Error("symlinked marker was followed")
	}
	if ManualMarkerOnDisk(dir) {
		t.Error("a directory read as a manual marker")
	}
	_, _, line := rungSongs()
	var k *KeptError
	if err := NewLRCWriter().WriteLRC(line, "song.lrc", dir); errors.As(err, &k) && k.Manual {
		t.Error("write refused as manual through a symlink")
	}
}

// Pin: WriteMarkerProvenance leaves a manual marker untouched.
func TestWriteMarkerProvenance_LeavesManualMarkerAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "song.txt")
	seed(t, p, manualMarkerBody)
	changed, err := WriteMarkerProvenance(p, InstrumentalProvenance{Source: SourceDetector, DetectorVersion: "v1"})
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want no-op", changed, err)
	}
	if got := readText(t, p); got != manualMarkerBody {
		t.Errorf("marker changed: %q", got)
	}
}

// The writer stamps exactly [source:manual] for a manual-lane song.
func TestWriteLRC_ManualLaneHeader(t *testing.T) {
	dir := t.TempDir()
	if err := NewLRCWriter().WriteLRC(manualSong(), "song.txt", dir); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(dir, "song.txt")); got != manualMarkerBody {
		t.Errorf("header = %q, want %q", got, manualMarkerBody)
	}
}
