package lyrics

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

func TestShiftLines(t *testing.T) {
	in := []TimedLine{{StartMS: 0, Text: "", Decorative: true}, {StartMS: 1500, Text: "a"}, {StartMS: 4000, Text: "b"}}
	for _, tc := range []struct {
		name string
		off  int
		want []int
	}{
		{"zero", 0, []int{0, 1500, 4000}},
		{"later", 600, []int{600, 2100, 4600}},
		{"earlier clamps at zero", -2000, []int{0, 0, 2000}},
	} {
		got := ShiftLines(in, tc.off)
		var starts []int
		for _, l := range got {
			starts = append(starts, l.StartMS)
		}
		if !reflect.DeepEqual(starts, tc.want) {
			t.Errorf("%s: starts = %v, want %v", tc.name, starts, tc.want)
		}
	}
	if in[1].StartMS != 1500 {
		t.Errorf("input mutated: %v", in[1].StartMS)
	}
	if got := ShiftLines(nil, 500); len(got) != 0 {
		t.Errorf("nil input: got %v", got)
	}
}

func writeFixture(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mtimeOf(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func assertNoOrig(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p + ".orig"); !os.IsNotExist(err) {
		t.Errorf(".orig exists after a refused edit: %v", err)
	}
}

func TestApplyEditCreatesOrigOnceAndShifts(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a", "t.lrc")
	writeFixture(t, p, "[ti:x]\n[00:01.00]one\n[00:05.00]two\n[00:09.00]three\n")
	orig, tags, err := OriginalLines(p, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p + ".orig"); !os.IsNotExist(err) {
		t.Fatalf(".orig exists before any save: %v", err)
	}
	res, err := ApplyEdit(p, ShiftLines(orig, 600), tags, EditOptions{Roots: []string{root}, ExpectMTime: mtimeOf(t, p), DurationSeconds: 30})
	if err != nil || !res.CreatedOrig {
		t.Fatalf("first save: res=%+v err=%v", res, err)
	}
	if got := readFile(t, p); !strings.Contains(got, "[ti:x]") || !strings.Contains(got, "[00:01.60]one") {
		t.Errorf("rewritten body:\n%s", got)
	}
	if got := readFile(t, p+".orig"); !strings.Contains(got, "[00:01.00]one") {
		t.Errorf(".orig is not the original:\n%s", got)
	}
	// Second save applies to the ORIGINAL, never on top of the first.
	orig2, tags2, _ := OriginalLines(p, []string{root})
	res2, err := ApplyEdit(p, ShiftLines(orig2, 600), tags2, EditOptions{Roots: []string{root}, ExpectMTime: res.NewMTime, DurationSeconds: 30})
	if err != nil || res2.CreatedOrig {
		t.Fatalf("second save: res=%+v err=%v", res2, err)
	}
	if got := readFile(t, p); !strings.Contains(got, "[00:01.60]one") {
		t.Errorf("second save compounded:\n%s", got)
	}
	if got := readFile(t, p+".orig"); !strings.Contains(got, "[00:01.00]one") {
		t.Errorf(".orig overwritten:\n%s", got)
	}
}

func TestApplyEditRefusesChangedFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n")
	stale := mtimeOf(t, p).Add(-time.Hour)
	_, err := ApplyEdit(p, []TimedLine{{StartMS: 2000, Text: "one"}}, nil, EditOptions{Roots: []string{root}, ExpectMTime: stale, DurationSeconds: 30})
	if !errors.Is(err, ErrEditChanged) {
		t.Fatalf("err = %v, want ErrEditChanged", err)
	}
	assertNoOrig(t, p)
}

func TestApplyEditRefusesTiming(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n[00:09.00]two\n")
	_, err := ApplyEdit(p, []TimedLine{{StartMS: 1000, Text: "one"}, {StartMS: 20000, Text: "two"}}, nil, EditOptions{Roots: []string{root}, DurationSeconds: 10})
	if !errors.Is(err, ErrEditTiming) {
		t.Fatalf("err = %v, want ErrEditTiming", err)
	}
	assertNoOrig(t, p)
	if got := readFile(t, p); !strings.Contains(got, "[00:09.00]two") {
		t.Errorf("file changed on refusal:\n%s", got)
	}
}

func TestApplyEditRefusesSymlinkAndEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	real := filepath.Join(outside, "t.lrc")
	writeFixture(t, real, "[00:01.00]one\n")
	link := filepath.Join(root, "t.lrc")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	for _, p := range []string{link, real} {
		if _, err := ApplyEdit(p, []TimedLine{{StartMS: 0, Text: "one"}}, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30}); !errors.Is(err, ErrEditRefused) {
			t.Errorf("%s: err = %v, want ErrEditRefused", p, err)
		}
	}
	assertNoOrig(t, real)
	if _, _, err := OriginalLines(link, []string{root}); !errors.Is(err, ErrEditRefused) {
		t.Errorf("OriginalLines on a symlink: err = %v, want ErrEditRefused", err)
	}
}

func TestApplyEditRefusesNonLRC(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.txt")
	writeFixture(t, p, "one\n")
	if _, err := ApplyEdit(p, nil, nil, EditOptions{Roots: []string{root}}); !errors.Is(err, ErrEditRefused) {
		t.Errorf("err = %v, want ErrEditRefused", err)
	}
}

func TestApplyEditUsesExistingOrigAndNormalizes(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n[00:05.00]one\n")    // backfill-expanded current file
	writeFixture(t, p+".orig", "[00:01.00][00:05.00]one\n") // #470 compressed original
	orig, tags, err := OriginalLines(p, []string{root})
	if err != nil || len(orig) != 2 {
		t.Fatalf("orig lines = %d, err = %v; want 2 expanded", len(orig), err)
	}
	res, err := ApplyEdit(p, ShiftLines(orig, 0), tags, EditOptions{Roots: []string{root}, DurationSeconds: 30})
	if err != nil || res.CreatedOrig {
		t.Fatalf("revert: res=%+v err=%v", res, err)
	}
	if got := readFile(t, p); strings.Contains(got, "][") {
		t.Errorf("stacked timestamps written back:\n%s", got)
	}
	if got := readFile(t, p+".orig"); got != "[00:01.00][00:05.00]one\n" {
		t.Errorf(".orig touched:\n%s", got)
	}
}

func TestApplyEditRecordsSelfWrite(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n")
	reg := selfwrite.New(time.Minute)
	if _, err := ApplyEdit(p, []TimedLine{{StartMS: 1200, Text: "one"}}, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30, SelfWrites: reg}); err != nil {
		t.Fatal(err)
	}
	if !reg.Suppress(p) {
		t.Error("write not recorded with selfwrite")
	}
}

func TestApplyEditRefusesWordTimedLines(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n")
	lines := []TimedLine{{StartMS: 1000, Text: "one", Words: []TimedWord{{StartMS: 1000, Text: "one"}}}}
	if _, err := ApplyEdit(p, lines, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30}); !errors.Is(err, ErrEditRefused) {
		t.Fatalf("err = %v, want ErrEditRefused", err)
	}
	assertNoOrig(t, p)
	if got := readFile(t, p); got != "[00:01.00]one\n" {
		t.Errorf("file changed on refusal:\n%s", got)
	}
}

// A .orig that exists but is not a regular file is no usable original: both
// reading the original and editing refuse, rather than silently treating the
// current .lrc as the original or skipping the backup.
func TestUnusableOrigRefused(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n")
	if err := os.Mkdir(p+".orig", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OriginalLines(p, []string{root}); !errors.Is(err, ErrEditRefused) {
		t.Errorf("OriginalLines err = %v, want ErrEditRefused", err)
	}
	if _, err := ApplyEdit(p, []TimedLine{{StartMS: 1500, Text: "one"}}, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30}); !errors.Is(err, ErrEditRefused) {
		t.Errorf("ApplyEdit err = %v, want ErrEditRefused", err)
	}
	if got := readFile(t, p); got != "[00:01.00]one\n" {
		t.Errorf("file changed on refusal:\n%s", got)
	}
}

// The backup is recorded with selfwrite and leaves no temp file behind.
func TestApplyEditRecordsOrigAndCleansTemp(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n")
	reg := selfwrite.New(time.Minute)
	if _, err := ApplyEdit(p, []TimedLine{{StartMS: 1200, Text: "one"}}, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30, SelfWrites: reg}); err != nil {
		t.Fatal(err)
	}
	if !reg.Suppress(p + ".orig") {
		t.Error(".orig write not recorded with selfwrite")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// An over-cap file is refused before parsing, from either source, and an edit
// that would have to back it up writes nothing.
func TestEditRefusesOversizedFile(t *testing.T) {
	big := "[00:01.00]one\n" + strings.Repeat("x", maxEditFileSize)
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, big)
	if _, _, err := OriginalLines(p, []string{root}); !errors.Is(err, ErrEditRefused) {
		t.Errorf("OriginalLines(.lrc) err = %v, want ErrEditRefused", err)
	}
	if _, err := ApplyEdit(p, []TimedLine{{StartMS: 1500, Text: "one"}}, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30}); !errors.Is(err, ErrEditRefused) {
		t.Errorf("ApplyEdit err = %v, want ErrEditRefused", err)
	}
	assertNoOrig(t, p)
	if got := readFile(t, p); got != big {
		t.Error("oversized .lrc changed on refusal")
	}
	q := filepath.Join(root, "u.lrc")
	writeFixture(t, q, "[00:01.00]one\n")
	writeFixture(t, q+".orig", big)
	if _, _, err := OriginalLines(q, []string{root}); !errors.Is(err, ErrEditRefused) {
		t.Errorf("OriginalLines(.orig) err = %v, want ErrEditRefused", err)
	}
}

// A restrictive source mode survives the edit, on both the rewrite and the backup.
func TestApplyEditKeepsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]one\n")
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEdit(p, []TimedLine{{StartMS: 1200, Text: "one"}}, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p, p + ".orig"} {
		fi, err := os.Lstat(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(f), got)
		}
	}
}

// An exclusive publish never replaces an existing destination and cleans its temp.
func TestRootWriteAtomicExclusiveKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "t.lrc.orig"), "keep\n")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := rootWriteAtomic(root, "t.lrc.orig", []byte("new\n"), 0o644, true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
	if got := readFile(t, filepath.Join(dir, "t.lrc.orig")); got != "keep\n" {
		t.Errorf("destination overwritten: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %d, want only the destination (temp left behind?)", len(entries))
	}
}

const genFixture = "[ti:x]\n[source:lane-a]\n[fetched:2026-01-02T03:04:05Z]\n[00:01.00]one\n[00:05.00]two a\n[00:05.00]two b\n[00:09.00]\n"

// TestApplyEditNilGeneratedBytes pins the exact bytes of the two edit modes
// that predate #1008 (a shift, then a zero-offset revert). The strings were
// produced by the code before EditOptions.Generated existed.
func TestApplyEditNilGeneratedBytes(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, genFixture)
	for _, tc := range []struct {
		off  int
		want string
	}{
		{600, "[ti:x]\n[source:lane-a]\n[fetched:2026-01-02T03:04:05Z]\n[00:01.60]one\n[00:05.60]two a\n[00:05.60]two b\n[00:09.60]♪\n"},
		{0, "[ti:x]\n[source:lane-a]\n[fetched:2026-01-02T03:04:05Z]\n[00:01.00]one\n[00:05.00]two a\n[00:05.00]two b\n[00:09.00]♪\n"},
	} {
		orig, tags, err := OriginalLines(p, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyEdit(p, ShiftLines(orig, tc.off), tags, EditOptions{Roots: []string{root}, DurationSeconds: 30}); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, p); got != tc.want {
			t.Errorf("offset %d wrote:\n%q\nwant:\n%q", tc.off, got, tc.want)
		}
	}
	if got := readFile(t, p+".orig"); got != genFixture {
		t.Errorf(".orig = %q, want the original bytes", got)
	}
}

func genApply(t *testing.T, root, p string, starts []int, g *GeneratedEdit, dur int) (EditResult, error) {
	t.Helper()
	orig, tags, err := OriginalLines(p, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := RetimeLines(orig, starts)
	if err != nil {
		return EditResult{}, err
	}
	return ApplyEdit(p, lines, tags, EditOptions{Roots: []string{root}, ExpectMTime: mtimeOf(t, p), DurationSeconds: dur, Generated: g})
}

func TestApplyEditGeneratedWritesMarkerAndStamps(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, genFixture)
	const head = "[ti:x]\n[source:lane-a]\n[fetched:2026-01-02T03:04:05Z]\n[timing:canticle-aligner]\n"
	g := &GeneratedEdit{Words: []models.WordTiming{{Line: 1, Text: "two", StartMS: 5500}, {Line: 1, Text: "a", StartMS: 5600}}}
	res, err := genApply(t, root, p, []int{1200, 5500, 5500, 9000}, g, 30)
	if err != nil || !res.CreatedOrig {
		t.Fatalf("accept: res=%+v err=%v", res, err)
	}
	// Words are validated, never written, in this slice.
	if got, want := readFile(t, p), head+"[00:01.20]one\n[00:05.50]two a\n[00:05.50]two b\n[00:09.00]♪\n"; got != want {
		t.Errorf("accept wrote:\n%q\nwant:\n%q", got, want)
	}
	// A second accept whose header tags already carry the marker (a file
	// accepted before its backup was lost) still yields it exactly once.
	lines, tags, err := OriginalLines(p, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	tags = append([]string{"[timing:canticle-aligner]"}, tags...)
	res, err = ApplyEdit(p, lines, tags, EditOptions{Roots: []string{root}, DurationSeconds: 30, Generated: &GeneratedEdit{}})
	if err != nil || res.CreatedOrig {
		t.Fatalf("second accept: res=%+v err=%v", res, err)
	}
	if got := readFile(t, p); strings.Count(got, "[timing:") != 1 || !strings.HasPrefix(got, head) {
		t.Errorf("marker not written exactly once, after the kept tags:\n%s", got)
	}
	if got := readFile(t, p+".orig"); got != genFixture {
		t.Errorf(".orig = %q, want the original bytes", got)
	}
	pt, err := ReadProvenanceTags(p)
	if err != nil || pt.Timing != TimingAligner || pt.Source != "lane-a" {
		t.Errorf("tags = %+v (%v), want timing %q and the source kept", pt, err, TimingAligner)
	}
	if pt, _ := ReadProvenanceTags(p + ".orig"); pt.Timing != "" {
		t.Errorf("unmarked file reads timing %q", pt.Timing)
	}
}

func TestApplyEditGeneratedRefusals(t *testing.T) {
	ok := []int{1200, 5500, 5500, 9000}
	word := func(line, ms int) *GeneratedEdit {
		return &GeneratedEdit{Words: []models.WordTiming{{Line: line, Text: "w", StartMS: ms}}}
	}
	for _, tc := range []struct {
		name   string
		body   string
		setup  func(t *testing.T, dir string)
		starts []int
		gen    *GeneratedEdit
		want   error  // nil: the accept succeeds
		as     string // the name the path is addressed by, when not t.lrc
		foldFS bool   // observable only where the volume folds name case
	}{
		{name: "too few stamps", starts: []int{1200, 5500, 5500}, want: ErrEditInvalid},
		{name: "negative", starts: []int{-1, 5500, 5500, 9000}, want: ErrEditInvalid},
		{name: "decreasing", starts: []int{6000, 5500, 5500, 9000}, want: ErrEditInvalid},
		{name: "split same-stamp group", starts: []int{1200, 5500, 5600, 9000}, want: ErrEditInvalid},
		{name: "timing guard", starts: []int{1200, 95000, 95000, 95000}, want: ErrEditTiming},
		{name: "word names no line", starts: ok, gen: word(4, 10), want: ErrEditInvalid},
		{name: "negative word", starts: ok, gen: word(0, -5), want: ErrEditInvalid},
		{name: "stamp past 24h", starts: []int{1200, 5500, 5500, 86_400_001}, want: ErrEditInvalid},
		{name: "word past 24h", starts: ok, gen: word(0, 86_400_001), want: ErrEditInvalid},
		{name: "words out of order", starts: ok, want: ErrEditInvalid, gen: &GeneratedEdit{Words: []models.WordTiming{
			{Line: 1, Text: "a", StartMS: 5600}, {Line: 1, Text: "two", StartMS: 5500}}}},
		{name: "loose inline words", body: "[00:01.00]<00:01.000>one <00:01.500>more\n[00:05.00]two a\n[00:05.00]two b\n[00:09.00]\n", starts: ok, want: ErrEditHasWords},
		{name: "loose words, no fraction", body: "[00:01.00]<00:01>one <00:02>more\n[00:05.00]two a\n[00:05.00]two b\n[00:09.00]\n", starts: ok, want: ErrEditHasWords},
		// A known conservative refusal: lyric text shaped like a mark is not told apart from one.
		{name: "ratio in the text", body: "[00:01.00]ratio <16:9>\n[00:05.00]two a\n[00:05.00]two b\n[00:09.00]\n", starts: ok, want: ErrEditHasWords},
		{name: "ordinary angle brackets", body: "[00:01.00]a < b > c <3\n[00:05.00]<x:1> two <12>\n[00:05.00]two b\n[00:09.00]\n", starts: ok},
		// foldFS rows: the Lstat branch is observable only on a case-insensitive volume; Linux CI guards the listing branch.
		{name: "companion stem in another case", starts: ok, want: ErrEditHasWords, foldFS: true, setup: func(t *testing.T, dir string) {
			writeFixture(t, filepath.Join(dir, "T.elrc"), "[by:someone]\n")
		}},
		{name: "path in another case than disk", starts: ok, want: ErrEditHasWords, foldFS: true, as: "T.lrc", setup: func(t *testing.T, dir string) {
			writeFixture(t, filepath.Join(dir, "t.elrc"), "[by:someone]\n")
		}},
		{name: "inline words", body: "[00:01.00]<00:01.00>one <00:01.50>more\n[00:05.00]two a\n[00:05.00]two b\n[00:09.00]\n", starts: ok, want: ErrEditHasWords},
		{name: "elrc sibling", starts: ok, want: ErrEditHasWords, setup: func(t *testing.T, dir string) {
			writeFixture(t, filepath.Join(dir, "t.elrc"), "[by:someone]\n")
		}},
		{name: "upper-case sibling", starts: ok, want: ErrEditHasWords, setup: func(t *testing.T, dir string) {
			writeFixture(t, filepath.Join(dir, "t.ELRC"), "x\n")
		}},
		{name: "directory sibling", starts: ok, want: ErrEditHasWords, setup: func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "t.elrc"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "dangling symlink sibling", starts: ok, want: ErrEditHasWords, setup: func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(dir, "absent"), filepath.Join(dir, "t.eLrc")); err != nil {
				t.Skip("symlinks unsupported")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := filepath.Join(root, "a", "t.lrc")
			body := tc.body
			if body == "" {
				body = genFixture
			}
			writeFixture(t, p, body)
			if tc.foldFS && caseSensitiveFS(t, root) {
				t.Skip("case-sensitive volume: that name is another track's file, so nothing resolves it as this one's companion")
			}
			writeFixture(t, filepath.Join(root, "a", "other.elrc"), "x\n") // not its companion
			if tc.setup != nil {
				tc.setup(t, filepath.Join(root, "a"))
			}
			addressed := p
			if tc.as != "" {
				addressed = filepath.Join(root, "a", tc.as)
			}
			gen := tc.gen
			if gen == nil {
				gen = &GeneratedEdit{}
			}
			before := mtimeOf(t, p)
			if _, err := genApply(t, root, addressed, tc.starts, gen, 30); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil {
				return
			}
			if got := readFile(t, p); got != body || !mtimeOf(t, p).Equal(before) {
				t.Errorf("refused accept touched the file:\n%q", got)
			}
			assertNoOrig(t, p)
		})
	}
}

// TestIsWordCompanionName judges entry NAMES directly, so the extension-case
// rule is proven on a case-insensitive filesystem too, where "t.ELRC" and
// "t.elrc" cannot both exist and a listing returns only one spelling. It is
// the name rule ONLY: "T.elrc" is another track's file by name, and whether
// the volume resolves it as this one's companion is refuseIfWordTimed's Lstat
// (the two case rows of TestApplyEditGeneratedRefusals).
func TestIsWordCompanionName(t *testing.T) {
	for name, want := range map[string]bool{
		"t.elrc": true, "t.ELRC": true, "t.eLrC": true,
		"t.lrc": false, "t.txt": false, "t.elrc.orig": false, "t.x.elrc": false,
		"T.elrc": false, "other.elrc": false, ".elrc": false, "t": false,
	} {
		if got := isWordCompanionName(name, "t"); got != want {
			t.Errorf("isWordCompanionName(%q, \"t\") = %v, want %v", name, got, want)
		}
	}
}

// TestReadRegularDetectsInPlaceRewrite: a rewrite of the same inode after the
// Lstat (different size, or same size with a new mtime) is ErrEditChanged.
func TestReadRegularDetectsInPlaceRewrite(t *testing.T) {
	for _, tc := range []struct{ name, body string }{{"size", "[00:02.00]longer\n"}, {"mtime", "[00:01.00]aaaa\n"}} {
		dir := t.TempDir()
		p := filepath.Join(dir, "a.lrc")
		if err := os.WriteFile(p, []byte("[00:01.00]bbbb\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		fi, err := lstatRegular(root, "a.lrc")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, time.Time{}, fi.ModTime().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := readRegular(root, "a.lrc", fi); !errors.Is(err, ErrEditChanged) {
			t.Errorf("%s: err = %v, want ErrEditChanged", tc.name, err)
		}
	}
}

// TestCurrentLinesIgnoresBackup pins the reader a generated accept validates
// from: the file itself whatever its .orig holds, and only at the given mtime.
func TestCurrentLinesIgnoresBackup(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[00:01.00]cur\n")
	writeFixture(t, p+".orig", "[00:02.00]old a\n[00:03.00]old b\n")
	lines, err := CurrentLines(p, []string{root}, mtimeOf(t, p))
	if err != nil || len(lines) != 1 || lines[0].Text != "cur" {
		t.Errorf("CurrentLines = %+v, %v; want the current file's one cue", lines, err)
	}
	for _, stale := range []time.Time{{}, mtimeOf(t, p).Add(time.Second)} {
		if _, err := CurrentLines(p, []string{root}, stale); !errors.Is(err, ErrEditChanged) {
			t.Errorf("CurrentLines at mtime %v: err = %v, want ErrEditChanged", stale, err)
		}
	}
}

// A .orig that is not the original of the current file must stop a rewrite:
// the bytes it would replace are saved nowhere (#1313).
func TestApplyEditRefusesStaleOrig(t *testing.T) {
	const cur = "[ar:a]\n[source:new]\n[00:01.00]one\n[00:05.00]two\n"
	for _, tc := range []struct {
		name, current, orig string
		generated, want     bool // want: refused
	}{
		{name: "same text and tags, other stamps", current: cur, orig: "[ar:a]\n[source:new]\n[00:02.00]one\n[00:06.00]two\n"},
		{name: "empty cue equals note", current: "[00:01.00]\n[00:05.00]two\n", orig: "[00:01.00]♪\n[00:05.00]two\n"},
		{name: "aligner marker only in current", current: "[ar:a]\n[timing:canticle-aligner]\n[00:01.00]one\n", orig: "[ar:a]\n[00:01.00]one\n"},
		{name: "different text", current: cur, orig: "[ar:a]\n[source:new]\n[00:01.00]uno\n[00:05.00]dos\n", want: true},
		{name: "different tags", current: cur, orig: "[ar:a]\n[source:old]\n[00:01.00]one\n[00:05.00]two\n", want: true},
		{name: "unstamped line only in current", current: cur + "Credit line\n", orig: cur, want: true},
		{name: "unstamped line in both", current: cur + "Credit line\n", orig: "Credit line\n" + cur},
		{name: "generated: stale text is not lost", current: cur, orig: "[00:01.00]uno\n[00:05.00]dos\n", generated: true},
		{name: "generated: unstamped line is lost", current: cur + "Credit line\n", orig: cur, generated: true, want: true},
	} {
		root := t.TempDir()
		p := filepath.Join(root, "t.lrc")
		writeFixture(t, p, tc.current)
		writeFixture(t, p+".orig", tc.orig)
		opts := EditOptions{Roots: []string{root}, DurationSeconds: 30}
		var lines []TimedLine
		var tags []string
		var err error
		if tc.generated {
			opts.Generated = &GeneratedEdit{}
			lines = []TimedLine{{StartMS: 1000, Text: "one"}, {StartMS: 5000, Text: "two"}}
		} else {
			if lines, tags, err = OriginalLines(p, opts.Roots); tc.want {
				if !errors.Is(err, ErrEditStaleOrig) {
					t.Errorf("%s: OriginalLines err = %v, want ErrEditStaleOrig", tc.name, err)
				}
				lines = []TimedLine{{StartMS: 1000, Text: "x"}}
			} else if err != nil {
				t.Fatalf("%s: OriginalLines: %v", tc.name, err)
			}
		}
		_, err = ApplyEdit(p, lines, tags, opts)
		if tc.want != errors.Is(err, ErrEditStaleOrig) || (!tc.want && err != nil) {
			t.Errorf("%s: ApplyEdit err = %v, want refused=%v", tc.name, err, tc.want)
		}
		if tc.want && readFile(t, p) != tc.current {
			t.Errorf("%s: a refused edit rewrote the file:\n%s", tc.name, readFile(t, p))
		}
		if readFile(t, p+".orig") != tc.orig {
			t.Errorf("%s: .orig touched", tc.name)
		}
	}
}
