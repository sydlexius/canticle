package scanner

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/testutil"
)

// txtNoreadFixture is a library of audio files that each have a lone .txt:
// two instrumental markers (one with provenance, one bare), one manual marker
// and two plain lyric sidecars. All names are invented.
type txtNoreadFixture struct {
	dir   string
	stems []string
	body  map[string]string
}

func newTxtNoreadFixture(t *testing.T) txtNoreadFixture {
	t.Helper()
	f := txtNoreadFixture{dir: t.TempDir(), body: map[string]string{
		"sourced":  "[by:canticle]\n[source:musixmatch]\n" + lyrics.InstrumentalMarker + "\n",
		"bare":     lyrics.InstrumentalMarker + "\n",
		"manual":   "[by:canticle]\n[source:" + lyrics.SourceManual + "]\n" + lyrics.InstrumentalMarker + "\n",
		"plainone": "First invented line\nSecond invented line\n",
		"plaintwo": "Third invented line\nFourth invented line\n",
	}}
	for stem, body := range f.body {
		f.stems = append(f.stems, stem)
		if err := testutil.WriteFLACFileWithComments(f.dir, stem+".flac", 44100, 441000,
			map[string]string{"ARTIST": "Invented Artist", "TITLE": "Invented " + stem}); err != nil {
			t.Fatalf("write audio: %v", err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, stem+".txt"), []byte(body), 0o600); err != nil {
			t.Fatalf("write txt: %v", err)
		}
	}
	sort.Strings(f.stems)
	return f
}

// scan runs one scan with a counting .txt reader and returns the sorted stems
// of the emitted rows plus the number of classification reads.
func (f txtNoreadFixture) scan(t *testing.T, opts ScanOptions) (stems []string, reads int64, lookups int) {
	t.Helper()
	var n atomic.Int64
	idx := newRecordingIndexStore()
	sc := NewScanner(WithIndexStore(idx))
	sc.readTxt = func(p string) ([]byte, error) {
		n.Add(1)
		return os.ReadFile(p) //nolint:gosec // reason: test path under t.TempDir
	}
	opts.MaxDepth = 100
	res, err := sc.ScanLibrary(context.Background(), f.dir, opts)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, r := range res {
		stems = append(stems, strings.TrimSuffix(filepath.Base(r.FilePath), ".flac")+"|"+r.Status)
	}
	sort.Strings(stems)
	return stems, n.Load(), idx.lookups
}

// An ordinary scan cannot reopen a .txt, so a marker and a plain sidecar are
// treated alike (indexed, skipped) and no .txt is read to tell them apart.
func TestScanLibrary_OrdinaryScanReadsNoTxt(t *testing.T) {
	f := newTxtNoreadFixture(t)
	want := []string{"bare|done", "manual|done", "plainone|done", "plaintwo|done", "sourced|done"}

	got, reads, lookups := f.scan(t, ScanOptions{})
	if reads != 0 {
		t.Errorf("ordinary scan read %d .txt files, want 0", reads)
	}
	if lookups != len(f.stems) {
		t.Errorf("index lookups = %d, want %d (every file consulted once)", lookups, len(f.stems))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rows = %v, want %v", got, want)
	}

	// Stronger than the counter: swap every .txt for same-size garbage with its
	// mtime restored. A scan that never looks inside cannot tell.
	for stem, body := range f.body {
		p := filepath.Join(f.dir, stem+".txt")
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("Z", len(body))), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, time.Now(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	got2, reads2, _ := f.scan(t, ScanOptions{})
	if reads2 != 0 || strings.Join(got2, ",") != strings.Join(want, ",") {
		t.Errorf("after garbage swap: rows = %v reads = %d, want %v and 0", got2, reads2, want)
	}
}

// Every mode that can reopen a .txt still classifies it, and the marker and
// the plain sidecar are still treated as they were before the read was gated.
func TestScanLibrary_ReopenModesStillClassifyTxt(t *testing.T) {
	cases := []struct {
		name string
		opts ScanOptions
		want []string
	}{
		// manual marker is never reopened (#1218): indexed and skipped.
		{"upgrade", ScanOptions{Upgrade: true},
			[]string{"bare|pending", "manual|done", "plainone|pending", "plaintwo|pending", "sourced|pending"}},
		{"update", ScanOptions{Update: true},
			[]string{"bare|pending", "manual|done", "plainone|pending", "plaintwo|pending", "sourced|pending"}},
		// --bare-markers reopens only a marker without provenance.
		{"upgrade-bare-markers", ScanOptions{Upgrade: true, BareMarkersOnly: true},
			[]string{"bare|pending", "manual|done", "plainone|done", "plaintwo|done", "sourced|done"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTxtNoreadFixture(t)
			got, reads, _ := f.scan(t, tc.opts)
			if reads != int64(len(f.stems)) {
				t.Errorf("read %d .txt files, want %d (every sidecar classified)", reads, len(f.stems))
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}
