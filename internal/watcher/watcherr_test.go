package watcher

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// TestAnnotateWatchErr_ENOSPCExplainsInotifyLimit is the point of this helper.
// inotify reports an exhausted watch quota as ENOSPC, whose stringification is
// "no space left on device" -- on a host with terabytes free. An operator who
// reads that goes looking at disk usage, which is the wrong place entirely.
func TestAnnotateWatchErr_ENOSPCExplainsInotifyLimit(t *testing.T) {
	err := annotateWatchErr(fmt.Errorf("watch /music: %w", syscall.ENOSPC), 7457)
	got := err.Error()

	for _, want := range []string{
		"fs.inotify.max_user_watches",
		"not a disk-space problem",
		"7457",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("annotated error missing %q:\n%s", want, got)
		}
	}
	// The original error must stay matchable: callers and logs still need the
	// underlying cause, and errors.Is must keep working through the wrap.
	if !errors.Is(err, syscall.ENOSPC) {
		t.Error("annotation broke errors.Is(err, syscall.ENOSPC)")
	}
}

// TestAnnotateWatchErr_OtherErrorsPassThroughUnchanged keeps the annotation
// narrow. Attaching an inotify explanation to a permission or path error would
// send the reader somewhere just as wrong as the ENOSPC string does today.
func TestAnnotateWatchErr_OtherErrorsPassThroughUnchanged(t *testing.T) {
	orig := fmt.Errorf("watch /music: %w", syscall.EACCES)
	err := annotateWatchErr(orig, 7457)

	if err.Error() != orig.Error() {
		t.Errorf("non-ENOSPC error was modified:\ngot  %s\nwant %s", err, orig)
	}
	if !errors.Is(err, syscall.EACCES) {
		t.Error("annotation broke errors.Is for a non-ENOSPC error")
	}
}

func TestAnnotateWatchErr_NilIsNil(t *testing.T) {
	if err := annotateWatchErr(nil, 0); err != nil {
		t.Errorf("annotateWatchErr(nil) = %v; want nil", err)
	}
}
