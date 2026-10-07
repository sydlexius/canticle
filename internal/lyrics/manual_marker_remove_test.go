package lyrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

func TestRemoveManualMarker(t *testing.T) {
	dir := t.TempDir()
	w := NewLRCWriter(dir)
	reg := selfwrite.New(time.Minute)
	w.SetSelfWriteRegistry(reg)
	song := models.Song{Track: models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1}, WinningLane: ManualLaneName}
	if err := w.WriteManualMarker(song, "song.flac", dir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "song.txt")
	if removed, err := w.RemoveManualMarker(marker); err != nil || !removed {
		t.Fatalf("marker: removed=%v err=%v; want removed", removed, err)
	}
	if !reg.Suppress(marker) {
		t.Error("a removed marker was not recorded with selfwrite")
	}
	reg = selfwrite.New(time.Minute)
	w.SetSelfWriteRegistry(reg)
	if _, err := os.Lstat(marker); err == nil {
		t.Error("marker still on disk")
	}
	if removed, err := w.RemoveManualMarker(marker); err != nil || removed {
		t.Errorf("missing file: removed=%v err=%v; want a quiet false", removed, err)
	}
	if err := os.WriteFile(marker, []byte("real lyrics\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := w.RemoveManualMarker(marker); err != nil || removed {
		t.Errorf("real lyrics: removed=%v err=%v; want left alone", removed, err)
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Error("real lyrics were removed")
	}
	if reg.Len() != 0 || reg.Suppress(marker) {
		t.Error("a file that is not a manual marker was recorded with selfwrite; the watcher would miss the operator's edit")
	}
}
