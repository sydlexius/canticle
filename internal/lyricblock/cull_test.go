package lyricblock

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

// Shared with the mark and unblock tests (later PRs): the bodies the fixtures
// write.
const (
	lrcBody  = "[00:01.00]Zorp the Blent\n[00:05.00]Frabble on\n"
	elrcBody = "[by:canticle]\n[00:01.00]<00:01.00>Zorp the Blent\n[00:05.00]<00:05.00>Frabble on\n"
)

func cullDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func names(paths []string) string {
	var b []string
	for _, p := range paths {
		b = append(b, filepath.Base(p))
	}
	return strings.Join(b, ",")
}

func TestInventoryCompanionOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, elrc string
		withLRC    bool
		want       string
	}{
		{"foreign companion is not listed", "[by:someone]\n[00:01.00]<00:01.00>x\n", true, "song.lrc,song.txt"},
		{"owned companion is listed", elrcBody, true, "song.lrc,song.txt,song.elrc"},
		{"owned companion with no lrc beside it is listed", elrcBody, false, "song.txt,song.elrc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"song.txt": "Zorp\n", "song.elrc": tc.elrc}
			if tc.withLRC {
				files["song.lrc"] = lrcBody
			}
			dir := cullDir(t, files)
			got, err := Inventory("Vexa Dunn", "Quill Moor", []Output{{Dir: dir, Filename: "song.flac"}})
			if err != nil {
				t.Fatal(err)
			}
			if names(got) != tc.want {
				t.Errorf("Inventory = %s, want %s", names(got), tc.want)
			}
		})
	}
}

func TestInventoryRefusesSymlink(t *testing.T) {
	dir := cullDir(t, nil)
	outside := filepath.Join(t.TempDir(), "secret.lrc")
	if err := os.WriteFile(outside, []byte(lrcBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "song.lrc")); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory("a", "b", []Output{{Dir: dir, Filename: "song.flac"}}); !errors.Is(err, ErrSymlinkedSidecar) {
		t.Fatalf("err = %v, want ErrSymlinkedSidecar", err)
	}
}

func TestReadBackupCapturesBytesAndCarriesNoPath(t *testing.T) {
	dir := cullDir(t, map[string]string{"song.lrc": lrcBody})
	p := filepath.Join(dir, "song.lrc")
	b, err := ReadBackup("op", 7, p)
	if err != nil || string(b.Content) != lrcBody || b.WorkItemID != 7 || b.Op != "op" || b.Path != p {
		t.Fatalf("ReadBackup = %+v, %v", b, err)
	}
	if err := os.WriteFile(p, make([]byte, MaxBackupBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadBackup("op", 7, p)
	if err == nil || !strings.Contains(err.Error(), "backup limit") || strings.Contains(err.Error(), dir) {
		t.Fatalf("over-limit err = %v, want a path-free backup-limit error", err)
	}
}

func TestAppendBackupWritesOneJSONLine(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "b.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	in := Backup{Op: "op", WorkItemID: 3, Path: "p", Content: []byte("x"), Meta: map[string]string{"k": "v"}}
	if err := AppendBackup(f, in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil || !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("read = %q, %v", raw, err)
	}
	var out Backup
	if err := json.Unmarshal(raw, &out); err != nil || string(out.Content) != "x" || out.Meta["k"] != "v" {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
}

func TestRemoveFilesCompanionFirstRecordsSelfwriteAndToleratesMissing(t *testing.T) {
	dir := cullDir(t, map[string]string{"song.lrc": lrcBody, "song.elrc": elrcBody})
	sw := selfwrite.New(time.Minute)
	lrc, elrc, gone := filepath.Join(dir, "song.lrc"), filepath.Join(dir, "song.elrc"), filepath.Join(dir, "song.txt")
	n, err := RemoveFiles([]string{lrc, gone, elrc}, sw)
	if err != nil || n != 3 {
		t.Fatalf("RemoveFiles = %d, %v", n, err)
	}
	for _, p := range []string{lrc, elrc} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s survived", filepath.Base(p))
		}
		if !sw.Suppress(p) {
			t.Errorf("%s not recorded with selfwrite", filepath.Base(p))
		}
	}
	// A failed companion removal leaves the .lrc too: the pair is never split.
	dir2 := cullDir(t, map[string]string{"song.lrc": lrcBody, "song.elrc": elrcBody})
	if err := os.Chmod(dir2, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir2, 0o755) })
	n, err = RemoveFiles([]string{filepath.Join(dir2, "song.lrc"), filepath.Join(dir2, "song.elrc")}, nil)
	if err == nil || n != 0 {
		t.Fatalf("RemoveFiles = %d, %v; want 0 removed and an error", n, err)
	}
	if strings.Contains(err.Error(), dir2) {
		t.Errorf("error leaks the path: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir2, "song.lrc")); err != nil {
		t.Error(".lrc removed although its companion failed")
	}
}

func TestInventorySkipsAHandPlacedInstrumentalMarker(t *testing.T) {
	dir := cullDir(t, map[string]string{"song.lrc": lrcBody, "song.txt": "[by:canticle]\n[source:manual]\n♪ Instrumental ♪\n"})
	got, err := Inventory("Vexa Dunn", "Quill Moor", []Output{{Dir: dir, Filename: "song.flac"}})
	if err != nil || names(got) != "song.lrc" {
		t.Fatalf("Inventory = %s, %v; want only the real lyric", names(got), err)
	}
}

func TestResolveOutputsConfinesToRoots(t *testing.T) {
	root := t.TempDir()
	in := []models.OutputPath{{Outdir: root, Filename: "a.flac"}}
	if got, err := resolveOutputs([]string{root}, in); err != nil || len(got) != 1 || got[0].Filename != "a.flac" {
		t.Fatalf("inside: %+v, %v", got, err)
	}
	if _, err := resolveOutputs([]string{filepath.Join(root, "other")}, in); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("outside err = %v, want ErrOutsideRoots", err)
	}
}

func TestReportOnceByPathAndContent(t *testing.T) {
	var sent []string
	fn := func(b Backup) error { sent = append(sent, string(b.Content)); return nil }
	seen := reportedSet{}
	for _, c := range []string{"one", "one", "two"} {
		if err := report(fn, Backup{Path: "p", Content: []byte(c)}, seen); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(sent, ",") != "one,two" {
		t.Errorf("sent %v; want a repeat of the same bytes skipped and changed bytes sent", sent)
	}
}

func TestReportErrorCarriesNoPath(t *testing.T) {
	err := ReportAll(func(b Backup) error {
		return &os.PathError{Op: "write", Path: b.Path, Err: errors.New("disk full")}
	}, []Backup{{Path: "/private/lib/song.lrc"}}, reportedSet{})
	if err == nil || !strings.Contains(err.Error(), "disk full") || strings.Contains(err.Error(), "/private") {
		t.Fatalf("err = %v, want the cause without the path", err)
	}
}
