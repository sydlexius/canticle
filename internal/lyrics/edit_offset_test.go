package lyrics

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// saveShift runs one editor save of shiftMS against the original, the way the
// web handler does, and fails the test on error.
func saveShift(t *testing.T, root, p string, shiftMS int) {
	t.Helper()
	orig, tags, err := OriginalLines(p, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyEdit(p, ShiftLines(orig, shiftMS), WithOffsetTag(tags, shiftMS), EditOptions{Roots: []string{root}, ExpectMTime: mtimeOf(t, p), DurationSeconds: 30})
	if err != nil {
		t.Fatalf("save %d: %v", shiftMS, err)
	}
}

func offsetTagLines(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if _, ok := isOffsetTag(l); ok {
			out = append(out, l)
		}
	}
	return out
}

func TestOffsetTagWrittenWithNoPriorHeader(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[ti:x]\n[00:01.00]one\n[00:05.00]two\n")
	saveShift(t, root, p, 600)
	got := readFile(t, p)
	if tl := offsetTagLines(got); !reflect.DeepEqual(tl, []string{"[offset:600]"}) {
		t.Errorf("offset tags = %q, want [offset:600]\n%s", tl, got)
	}
	if !strings.Contains(got, "[00:01.60]one") {
		t.Errorf("stamps not shifted:\n%s", got)
	}
}

func TestOffsetTagReplacedOnSecondSave(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[ti:x]\n[00:01.00]one\n[00:05.00]two\n")
	saveShift(t, root, p, 600)
	saveShift(t, root, p, -300)
	got := readFile(t, p)
	if tl := offsetTagLines(got); !reflect.DeepEqual(tl, []string{"[offset:-300]"}) {
		t.Errorf("offset tags = %q, want exactly [offset:-300]\n%s", tl, got)
	}
	// A save of zero returns to the original, header and all.
	saveShift(t, root, p, 0)
	if got := readFile(t, p); len(offsetTagLines(got)) != 0 {
		t.Errorf("zero shift kept a header:\n%s", got)
	}
}

func TestOffsetTagSumsOriginalsOwnHeader(t *testing.T) {
	got := WithOffsetTag([]string{"[ar:a]", "[offset: 200 ]", "[OFFSET:5]"}, 100)
	if want := []string{"[ar:a]", "[offset:300]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WithOffsetTag = %q, want %q", got, want)
	}
}

// A clamped save records the REQUESTED shift summed with the original's own
// offset, not what each line actually moved; pinning it keeps the documented
// (lossy) behavior from changing silently.
func TestOffsetTagRecordsRequestedShiftWhenLinesClamp(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[offset:200]\n[00:01.00]one\n[00:05.00]two\n")
	saveShift(t, root, p, -3000)
	got := readFile(t, p)
	if tl := offsetTagLines(got); !reflect.DeepEqual(tl, []string{"[offset:-2800]"}) {
		t.Errorf("offset tags = %q, want [offset:-2800]", tl)
	}
	if !strings.Contains(got, "[00:00.00]one") || !strings.Contains(got, "[00:02.00]two") {
		t.Errorf("lines not clamped/shifted as expected:\n%s", got)
	}
}

// An original carrying its own [offset:] is reproduced by a zero-shift save:
// the header is the original's own value plus the requested shift, so a shift
// of zero (or a revert to the original) must keep it, not strip it.
func TestOffsetTagOriginalsOwnHeaderKeptOnZeroShift(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[offset:200]\n[00:01.00]one\n[00:05.00]two\n")
	saveShift(t, root, p, 0)
	if tl := offsetTagLines(readFile(t, p)); !reflect.DeepEqual(tl, []string{"[offset:200]"}) {
		t.Errorf("zero shift offset tags = %q, want [offset:200]", tl)
	}
	saveShift(t, root, p, 500)
	saveShift(t, root, p, 0) // revert to the original's stamps
	got := readFile(t, p)
	if tl := offsetTagLines(got); !reflect.DeepEqual(tl, []string{"[offset:200]"}) {
		t.Errorf("revert offset tags = %q, want [offset:200]\n%s", tl, got)
	}
	if !strings.Contains(got, "[00:01.00]one") {
		t.Errorf("revert did not restore stamps:\n%s", got)
	}
}

// A line holding an offset tag plus another tag is not an offset tag: it is
// neither replaced nor dropped, and the neighboring tag survives.
func TestOffsetTagCompoundLineUntouched(t *testing.T) {
	const compound = "[offset:10][foo:bar]"
	if _, ok := isOffsetTag(compound); ok {
		t.Fatalf("isOffsetTag(%q) = true, want false", compound)
	}
	got := WithOffsetTag([]string{compound, "[ar:a]"}, 100)
	if want := []string{compound, "[ar:a]", "[offset:100]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WithOffsetTag = %q, want %q", got, want)
	}
	if got := dropOffsetTags([]string{compound, "[offset:5]"}); !reflect.DeepEqual(got, []string{compound}) {
		t.Errorf("dropOffsetTags = %q, want only the compound line", got)
	}
}

// A non-numeric existing offset is kept as written and no new tag is added.
func TestOffsetTagNonNumericPreserved(t *testing.T) {
	for _, v := range []string{"abc", "1.5"} {
		in := []string{"[ar:a]", "[offset:" + v + "]"}
		if got := WithOffsetTag(in, 100); !reflect.DeepEqual(got, in) {
			t.Errorf("offset %q: WithOffsetTag = %q, want unchanged %q", v, got, in)
		}
	}
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[offset:abc]\n[00:01.00]one\n[00:05.00]two\n")
	saveShift(t, root, p, 600)
	got := readFile(t, p)
	if tl := offsetTagLines(got); !reflect.DeepEqual(tl, []string{"[offset:abc]"}) {
		t.Errorf("offset tags = %q, want [offset:abc]\n%s", tl, got)
	}
	if !strings.Contains(got, "[00:01.60]one") {
		t.Errorf("stamps not shifted:\n%s", got)
	}
}

// A generated retime drops the recorded shift: the new stamps no longer relate
// to it.
func TestGeneratedRetimeDropsOffsetTag(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[ti:x]\n[offset:200]\n[00:01.00]one\n[00:05.00]two\n")
	lines := []TimedLine{{StartMS: 1500, Text: "one"}, {StartMS: 5500, Text: "two"}}
	if _, err := ApplyEdit(p, lines, nil, EditOptions{Roots: []string{root}, DurationSeconds: 30, Generated: &GeneratedEdit{}}); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, p)
	if len(offsetTagLines(got)) != 0 || !strings.Contains(got, "[ti:x]") || !strings.Contains(got, "[00:01.50]one") {
		t.Errorf("retime kept the offset or lost other content:\n%s", got)
	}
}

// The current file's [offset:] record never counts against the .orig backup
// (the backup is the pre-shift original, so it lacks the tag).
func TestSkeletonIgnoresOffsetTagAgainstOrig(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "t.lrc")
	writeFixture(t, p, "[ti:x]\n[offset:600]\n[00:01.60]one\n[00:05.60]two\n")
	writeFixture(t, p+".orig", "[ti:x]\n[00:01.00]one\n[00:05.00]two\n")
	if _, _, err := OriginalLines(p, []string{root}); err != nil {
		t.Errorf("OriginalLines with an offset only in the current file: %v", err)
	}
}
