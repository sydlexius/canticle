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
