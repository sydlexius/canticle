//go:build unix

package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestReadProvenanceHeader_BoundedAndRegularOnly: the header reader stops at
// MaxHeaderBytes (a tag past it is never seen) and refuses a FIFO or symlink
// without blocking.
func TestReadProvenanceHeader_BoundedAndRegularOnly(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.lrc")
	if err := os.WriteFile(good, []byte("[source:innertube]\n[upstream:lyricfind]\n[00:01.00]la\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pt, err := ReadProvenanceHeader(good); err != nil || pt.Upstream != "lyricfind" {
		t.Fatalf("good: %+v %v", pt, err)
	}

	late := filepath.Join(dir, "late.lrc")
	body := "[source:innertube]\n[" + strings.Repeat("x", MaxHeaderBytes) + ":v]\n[upstream:lyricfind]\n"
	if err := os.WriteFile(late, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if pt, err := ReadProvenanceHeader(late); err != nil || pt.Upstream != "" {
		t.Errorf("tag past the cap must not be read: %+v %v", pt, err)
	}

	fifo := filepath.Join(dir, "fifo.lrc")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if _, err := ReadProvenanceHeader(fifo); err == nil {
		t.Error("fifo: want a refusal")
	}
	link := filepath.Join(dir, "link.lrc")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProvenanceHeader(link); err == nil {
		t.Error("symlink: want a refusal")
	}
}
