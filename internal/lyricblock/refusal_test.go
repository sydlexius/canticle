package lyricblock

import (
	"errors"
	"fmt"
	"testing"
)

func TestRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		res  MarkResult
		want string
	}{
		{"nil", nil, MarkResult{}, ""},
		{"unrelated", errors.New("boom"), MarkResult{}, ""},
		{"not found", ErrNotFound, MarkResult{}, "not found"},
		{"wrapped not found", fmt.Errorf("work item 3: %w", ErrNotFound), MarkResult{}, "not found"},
		{"busy", ErrBusy, MarkResult{}, "in flight"},
		{"no sidecar", ErrNoSidecar, MarkResult{}, "no lyric file on disk"},
		{"manual instrumental", ErrManualInstrumental, MarkResult{}, "marked instrumental by hand"},
		{"not markable", ErrNotMarkable, MarkResult{}, "failed or unavailable"},
		{"sentinel after changes began is a failure", ErrBusy, MarkResult{Removed: 1}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Refusal(tc.err, tc.res); got != tc.want {
				t.Errorf("Refusal = %q, want %q", got, tc.want)
			}
		})
	}
}
