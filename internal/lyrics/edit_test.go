package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
