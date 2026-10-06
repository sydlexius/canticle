package ffmpeg

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

// runErr runs a real process and returns its error, so the classification is
// judged against the error shapes exec actually produces.
func runErr(t *testing.T, name string, args ...string) error {
	t.Helper()
	err := exec.Command(name, args...).Run()
	if err == nil {
		t.Fatalf("%s %v succeeded; the test needs a failure", name, args)
	}
	return err
}

// Only a process that ran and exited non-zero on its own is a per-file decode
// failure; start failures, signals and the wrapper/termination exit codes are
// the host's and must stay retryable (#1149).
func TestSampleErrorMarksOnlyDecodeFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh and POSIX signals")
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"decode exit 1", runErr(t, "sh", "-c", "exit 1"), true},
		{"decode exit 69", runErr(t, "sh", "-c", "exit 69"), true},
		{"start failure", runErr(t, "/nonexistent/ffmpeg"), false},
		{"not on PATH", runErr(t, "canticle-no-such-ffmpeg"), false},
		{"wrapper could not exec", runErr(t, "sh", "-c", "exit 127"), false},
		{"wrapper not executable", runErr(t, "sh", "-c", "exit 126"), false},
		{"ffmpeg caught a signal", runErr(t, "sh", "-c", "exit 255"), false},
		{"killed", runErr(t, "sh", "-c", "kill -9 $$"), false},
		{"plain error", errors.New("exit status 1"), false},
	}
	for _, tc := range cases {
		if got := errors.Is(SampleError("detector", tc.err, ""), ErrSampleFailed); got != tc.want {
			t.Errorf("%s: errors.Is(ErrSampleFailed) = %v; want %v (cause %v)", tc.name, got, tc.want, tc.err)
		}
	}
}
