package titlerepair

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/worker"
)

// noFetch is a lane that fails the test when asked: the title rule settles a
// repaired row without one.
type noFetch struct{ t *testing.T }

func (f noFetch) FindLyrics(context.Context, models.Track) (models.Song, error) {
	f.t.Error("a lane was asked for a repaired row")
	return models.Song{}, errors.New("no fetch")
}

// The whole loop: the repair culls and reopens, one worker pass per row (the
// title rule on, a real writer, no lane) writes a [source:title-rule] marker at
// every output and settles the row under the title-rule lane, and a second dry
// run finds nothing left. The second row has two output paths.
func TestWorkerSettlesRepairedRowsWithAMarker(t *testing.T) {
	s := newShelf(t)
	second := filepath.Join(s.base, "music", "copy")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	one := s.row(t, "")
	two := s.row(t, fmt.Sprintf(`output_paths = '[{"outdir":%q,"filename":"2.flac"},{"outdir":%q,"filename":"2.flac"}]'`, s.dir, second))
	stems := []string{one.stem, two.stem, filepath.Join(second, "2")}
	for _, stem := range stems {
		put(t, stem+".lrc", lrcBody, 0)
	}
	if res, _ := run(t, s.d, s.opts(true, func(lyricblock.Backup) error { return nil })); res.Repaired != 2 || res.Removed != 3 {
		t.Fatalf("repair: %+v; want two rows repaired and three files removed", res)
	}
	w := worker.New(queue.NewDBQueue(s.d), cache.New(s.d), noFetch{t}, lyrics.NewLRCWriter(filepath.Join(s.base, "music")))
	w.SetTitleRule(true)
	for range 2 {
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	for _, stem := range stems { // the marker is a .txt, and the culled .lrc stays gone
		if b, err := os.ReadFile(stem + ".txt"); err != nil || !strings.Contains(string(b), "[source:title-rule]") || exists(stem+".lrc") {
			t.Errorf("%s.txt = %q, %v; want a title-rule marker and no .lrc", filepath.Base(stem), b, err)
		}
	}
	for _, tr := range []track{one, two} {
		if got := s.rowState(t, tr.id); got != "done|title-rule|instrumental|1" {
			t.Errorf("row %d after the worker pass: %s", tr.id, got)
		}
	}
	if res, _ := run(t, s.d, s.opts(false, nil)); res.Examined != 0 || res.WouldRepair != 0 {
		t.Errorf("second dry run: %+v; want nothing selected", res)
	}
}
