//go:build !windows

package commands

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLoadRefSet_FIFORejected(t *testing.T) {
	for target, want := range map[string]string{"f.lrc": taMsgRefFile, "manifest.toml": taMsgManifest} {
		ref := taRefDir(t, t.TempDir(), "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"f.lrc\"\nline_residual_ms = 1\n")
		p := filepath.Join(ref, target)
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skip("mkfifo unavailable")
		}
		if code, got := taRejectOne(t, ref); code != 1 || got != "timing-accuracy: "+want+"\n" {
			t.Errorf("%s: exit %d out %q; want 1 with %q", target, code, got, want)
		}
	}
}
