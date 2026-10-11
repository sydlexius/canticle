package lyricblock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
)

// The options a bulk caller sets on Mark. Their zero values are covered by
// mark_test.go, whose fixture row is hand-edited and is marked all the same.

var errVet = errors.New("vet says no")

// untouched fails unless the mark changed nothing: the lyric still on disk, no
// block, the row settled with its offset.
func (f *fx) untouched(t *testing.T, res MarkResult) {
	t.Helper()
	if res != (MarkResult{}) {
		t.Errorf("result = %+v, want zero", res)
	}
	if !f.exists("song.lrc") || f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 ||
		f.count(t, `SELECT COUNT(*) FROM work_queue WHERE status = 'done' AND outcome_type = 'synced' AND lyric_offset_ms = 250`) != 1 {
		t.Error("a refused mark changed the file, the blocks or the row")
	}
}

func TestMarkOpLabelsTheBackupRecords(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	var ops []string
	// The first report places a late file, so the late read is labeled too.
	r := f.req(func(b Backup) error {
		ops = append(ops, b.Op)
		f.write(t, "song.txt", "Late words nobody saw\n")
		return nil
	})
	r.Op = "bulk-repair"
	if _, err := f.svc.Mark(f.ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0] != "bulk-repair" || ops[1] != "bulk-repair" {
		t.Errorf("backup ops = %v, want the request's Op on the first and the late record", ops)
	}
}

func TestRefusalNamesTheStrictRefusals(t *testing.T) {
	for err, want := range map[error]string{ErrHandEdited: "lyric edited by hand", ErrStemOccupied: "another file occupies the lyric stem"} {
		if got := Refusal(err, MarkResult{}); got != want {
			t.Errorf("Refusal(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestMarkRefuseHandEdited(t *testing.T) {
	t.Run("an edited row is refused before anything is read", func(t *testing.T) {
		f := newFx(t)
		f.write(t, "song.lrc", lrcBody)
		r := f.req(func(Backup) error { t.Error("Report called for a refused row"); return nil })
		r.RefuseHandEdited = true
		res, err := f.svc.Mark(f.ctx, r)
		if !errors.Is(err, ErrHandEdited) {
			t.Fatalf("Mark = %v, want ErrHandEdited", err)
		}
		f.untouched(t, res)
		if f.count(t, `SELECT COUNT(*) FROM work_queue WHERE lyric_edited_at = '2026-01-01T00:00:00Z'`) != 1 {
			t.Error("the edit mark was cleared")
		}
	})
	// The edit lands after the row was read and judged unedited: only the clause
	// in the statement can refuse it.
	t.Run("an edit made after the read is refused by the statement", func(t *testing.T) {
		f := newFx(t)
		f.mustExec(t, `UPDATE work_queue SET lyric_edited_at = NULL`)
		f.write(t, "song.lrc", lrcBody)
		if err := cache.New(f.db).Store(f.ctx, "Vexa Dunn", "Quill Moor", 0, "old"); err != nil {
			t.Fatal(err)
		}
		r := f.req(nil)
		r.RefuseHandEdited = true
		r.Vet = func([]Backup) error {
			f.mustExec(t, `UPDATE work_queue SET lyric_edited_at = '2026-02-02T00:00:00Z'`)
			return nil
		}
		res, err := f.svc.Mark(f.ctx, r)
		if !errors.Is(err, ErrHandEdited) {
			t.Fatalf("Mark = %+v, %v; want ErrHandEdited", res, err)
		}
		f.untouched(t, res)
		if f.count(t, `SELECT COUNT(*) FROM work_queue WHERE lyric_edited_at = '2026-02-02T00:00:00Z' AND sync_tier = 'line'`) != 1 ||
			f.count(t, `SELECT COUNT(*) FROM lyrics_cache`) != 1 {
			t.Error("the late edit mark was cleared or the cache was invalidated")
		}
	})
}

func TestMarkVetErrorChangesNothing(t *testing.T) {
	for _, dry := range []bool{true, false} {
		f := newFx(t)
		f.write(t, "song.lrc", lrcBody)
		r := f.req(func(Backup) error { t.Error("Report called after a failed vet"); return nil })
		r.DryRun = dry
		var saw []Backup
		r.Vet = func(recs []Backup) error { saw = recs; return errVet }
		res, err := f.svc.Mark(f.ctx, r)
		if !errors.Is(err, errVet) {
			t.Fatalf("dry=%v: Mark = %+v, %v; want the vet error", dry, res, err)
		}
		if len(saw) != 1 || string(saw[0].Content) != lrcBody || saw[0].WorkItemID != f.id {
			t.Errorf("dry=%v: Vet saw %d records, want the one lyric file's bytes", dry, len(saw))
		}
		f.untouched(t, res)
	}
}

// A file that appears after the first transaction, and one that passed the
// first vet and was rewritten since, are vetted again. A failure leaves every
// file in place and the row settled, with the blocks standing.
func TestMarkVetsALateFileAndLeavesEverythingOnFailure(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	const rewritten = "[by:canticle]\n[00:02.00]Rewritten after the vet\n"
	r := f.req(func(Backup) error {
		f.write(t, "song.lrc", rewritten)
		f.write(t, "song.txt", "Late words nobody saw\n")
		return nil
	})
	var lateLRC string
	var calls [][]string
	r.Vet = func(recs []Backup) error {
		var got []string
		for _, b := range recs {
			got = append(got, filepath.Base(b.Path))
			if len(calls) == 1 && filepath.Ext(b.Path) == ".lrc" {
				lateLRC = string(b.Content)
			}
		}
		if calls = append(calls, got); len(calls) > 1 {
			return errVet
		}
		return nil
	}
	res, err := f.svc.Mark(f.ctx, r)
	if !errors.Is(err, errVet) || res.NewBlocks != 1 || res.Files != 1 || res.Removed != 0 || res.Reopened {
		t.Fatalf("Mark = %+v, %v; want the vet error after one block and no removal", res, err)
	}
	if len(calls) != 2 || names(calls[1]) != "song.lrc,song.txt" || lateLRC != rewritten {
		t.Errorf("Vet calls = %v, want the first read then the rewritten file's new bytes and the late file", calls)
	}
	if !f.exists("song.lrc") || !f.exists("song.txt") || f.status(t) != "done" {
		t.Error("a file was removed or the row reopened after a failed late vet")
	}
}

// instrumental is the marker the title rule writes through WriteLRCIfAbsent.
var instrumental = models.Song{Track: models.Track{ArtistName: "Vexa Dunn", TrackName: "Quill Moor", Instrumental: 1}}

func TestMarkRequireEmptyStem(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		refused          bool
	}{
		{"a foreign companion refuses", "song.elrc", "[by:someone]\n[00:01.00]<00:01.00>x\n", true},
		{"a hand-placed marker refuses", "song.txt", "[by:canticle]\n[source:manual]\n♪ Instrumental ♪\n", true},
		{"an owned companion is culled with its lyric", "song.elrc", elrcBody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			f.write(t, "song.lrc", lrcBody)
			f.write(t, tc.file, tc.body)
			r := f.req(nil)
			r.RequireEmptyStem = true
			if dry := r; tc.refused {
				dry.DryRun = true
				if res, err := f.svc.Mark(f.ctx, dry); !errors.Is(err, ErrStemOccupied) {
					t.Fatalf("dry run: Mark = %+v, %v; want ErrStemOccupied", res, err)
				}
			}
			res, err := f.svc.Mark(f.ctx, r)
			if tc.refused {
				if !errors.Is(err, ErrStemOccupied) {
					t.Fatalf("Mark = %+v, %v; want ErrStemOccupied", res, err)
				}
				f.untouched(t, res)
				// The refusal is the writer's own verdict: with the one file Mark
				// would have removed gone, the stem is still occupied.
				if err := os.Remove(filepath.Join(f.dir, "song.lrc")); err != nil {
					t.Fatal(err)
				}
			} else if err != nil || res.Removed != 2 || !res.Reopened {
				t.Fatalf("Mark = %+v, %v; want both files culled", res, err)
			}
			occ, err := lyrics.NewLRCWriter().Occupant(instrumental, "song.flac", f.dir)
			if err != nil || (occ != nil) != tc.refused {
				t.Errorf("writer sees the stem occupied = %v (%v), want %v", occ != nil, err, tc.refused)
			}
		})
	}
}

// Every output's stem is checked: the foreign companion sits at the second only.
func TestMarkRequireEmptyStemChecksEveryOutput(t *testing.T) {
	f := newFx(t)
	dir2 := filepath.Join(f.root, "album2")
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustExec(t, fmt.Sprintf(`UPDATE work_queue SET output_paths = '[{"outdir":%q,"filename":"song.flac"},{"outdir":%q,"filename":"song.flac"}]'`, f.dir, dir2))
	f.write(t, "song.lrc", lrcBody)
	if err := os.WriteFile(filepath.Join(dir2, "song.elrc"), []byte("[by:someone]\n[00:01.00]<00:01.00>x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := f.req(nil)
	r.RequireEmptyStem = true
	res, err := f.svc.Mark(f.ctx, r)
	if !errors.Is(err, ErrStemOccupied) {
		t.Fatalf("Mark = %+v, %v; want ErrStemOccupied", res, err)
	}
	f.untouched(t, res)
}

// An output with no filename is named from the artist, and the worker names it
// from the resolved one: a file on that stem stops its next write.
func TestMarkRequireEmptyStemCountsTheResolvedArtistStem(t *testing.T) {
	f := newFx(t)
	f.mustExec(t, `UPDATE work_queue SET filename = '', album_artist = 'Ostrel Fenn'`)
	name, err := lyrics.SidecarName("Ostrel Fenn", "Quill Moor", "", true)
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, name, lrcBody)
	r := f.req(func(Backup) error { t.Error("Report called for a refused row"); return nil })
	r.RequireEmptyStem = true
	res, err := f.svc.Mark(f.ctx, r)
	if !errors.Is(err, ErrStemOccupied) || res != (MarkResult{}) || !f.exists(name) {
		t.Fatalf("Mark = %+v, %v (file kept %v); want ErrStemOccupied and nothing changed", res, err, f.exists(name))
	}
	song := instrumental
	song.Track.ArtistName = "Ostrel Fenn"
	if occ, err := lyrics.NewLRCWriter().Occupant(song, "", f.dir); err != nil || occ == nil {
		t.Errorf("writer sees the resolved-artist stem occupied = %v (%v), want true", occ != nil, err)
	}
}

// The worker keys the cache by the resolved artist: the album artist when it is
// a real name, the track artist when it is a compilation placeholder.
func TestMarkInvalidatesTheResolvedArtistCacheKey(t *testing.T) {
	for _, tc := range []struct {
		albumArtist       string
		invalidated, left int
	}{
		{"Ostrel Fenn", 2, 1},
		{"Various Artists", 1, 2},
	} {
		t.Run(tc.albumArtist, func(t *testing.T) {
			f := newFx(t)
			if _, err := f.db.Exec(`UPDATE work_queue SET album_artist = ?`, tc.albumArtist); err != nil {
				t.Fatal(err)
			}
			f.write(t, "song.lrc", lrcBody)
			c := cache.New(f.db)
			for _, k := range [][2]string{{"Vexa Dunn", "Quill Moor"}, {tc.albumArtist, "Quill Moor"}, {"Ostrel Fenn", "Tarn Hollow"}} {
				if err := c.Store(f.ctx, k[0], k[1], 0, "old"); err != nil {
					t.Fatal(err)
				}
			}
			res, err := f.svc.Mark(f.ctx, f.req(nil))
			if err != nil || res.CacheInvalidated != tc.invalidated {
				t.Fatalf("Mark = %+v, %v; want %d cache rows invalidated", res, err, tc.invalidated)
			}
			if got := f.count(t, `SELECT COUNT(*) FROM lyrics_cache`); got != tc.left {
				t.Errorf("%d cache rows left, want %d", got, tc.left)
			}
		})
	}
}
