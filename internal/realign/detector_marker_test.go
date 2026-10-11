package realign

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/orchestrator"
)

type settleDetector struct{}

func (settleDetector) Detect(context.Context, string) (detector.Result, error) {
	return detector.Result{Instrumental: true, Version: "1.5.0"}, nil
}

// TestDetectorSettledMarkerSurvivesRealign is the #530 round trip with real
// files and real code on both sides: the detector lane settles a track that
// carries an ISRC, the real LRCWriter writes the marker, the audio is renamed,
// and realign must re-attach the marker intact.
//
// It also pins WHICH tier carries it, because that is the finding behind the
// lane's decision: an instrumental marker writes no [isrc:]/[mbid:] for ANY lane
// (the instrumental branch of WriteLRC emits [by:]/[source:]/[dv:] only), so the
// exact tier cannot serve a detector marker, or a provider one, and the
// heuristic tier does. If a later change makes instrumental markers carry
// identity tags, the Method assertion below should flip to "exact" on purpose.
//
// This is a characterization / regression guard, not a test of the lane change:
// it passed before #530's lane change and does not exercise the carried fields
// (dropping any of them leaves it green). What it pins is that a
// detector-settled marker follows its audio through a rename via the heuristic
// tier, byte for byte.
func TestDetectorSettledMarkerSurvivesRealign(t *testing.T) {
	root := tempRoot(t)
	dir := filepath.Join(root, "Quiet Rooms")
	const isrc = "ZZXXX2600001"

	lane := orchestrator.NewDetectorLane(settleDetector{}, circuit.New(time.Minute, time.Hour), nil)
	song, err := lane.FindLyrics(context.Background(), models.Track{
		ArtistName: "Fennel Quartet", TrackName: "Slow Lantern", AlbumName: "Quiet Rooms",
		TrackLength: 241, ISRC: isrc,
	}, "/music/slow-lantern.flac")
	if err != nil {
		t.Fatalf("detector lane: %v", err)
	}
	song.WinningLane = lyrics.DetectorLaneName // what the orchestrator stamps on a live settle

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lyrics.NewLRCWriter().WriteLRC(song, "01 Slow Lantern.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	orphan := filepath.Join(dir, "01 Slow Lantern.txt")
	before, err := os.ReadFile(orphan) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if !strings.Contains(string(before), "[source:canticle-detector]") {
		t.Fatalf("marker is not detector-sourced: %q", before)
	}

	// The audio was renamed: it carries the ISRC in its tags, no sidecar beside it.
	audio := filepath.Join(dir, "01 Slow Lantern (Remastered).flac")
	write(t, audio, "a")
	r, lib := newRealigner(root, defaultCfg(), nil)
	r.readProv = func(path string) (string, string, string, string, error) {
		return isrc, "", "Fennel Quartet", "Slow Lantern", nil
	}

	res, err := r.PlanLibrary(lib)
	if err != nil {
		t.Fatalf("PlanLibrary: %v", err)
	}
	if len(res.Moves) != 1 || res.Moves[0].Orphan != orphan {
		t.Fatalf("moves = %+v, skips = %+v; want the detector marker re-attached", res.Moves, res.Skips)
	}
	if res.Moves[0].Method != "heuristic" {
		t.Errorf("tier = %q; want heuristic (an instrumental marker carries no [isrc:]/[mbid:] for the exact tier)", res.Moves[0].Method)
	}
	applied, err := r.Apply(res.Moves, filepath.Join(t.TempDir(), "b.jsonl"), Policy{AllowHeuristic: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if moved, _, _ := CountApplied(applied); moved != 1 {
		t.Fatalf("applied = %+v; want 1 move", applied)
	}
	after, err := os.ReadFile(filepath.Join(dir, "01 Slow Lantern (Remastered).txt")) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatalf("marker not at the renamed stem: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("marker bytes changed across realign:\n got %q\nwant %q", after, before)
	}
	if _, serr := os.Stat(orphan); !os.IsNotExist(serr) {
		t.Errorf("old marker path should be gone: %v", serr)
	}
}
