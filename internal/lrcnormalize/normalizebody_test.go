package lrcnormalize

import "testing"

func FuzzNormalizeBody(f *testing.F) {
	f.Add("")
	f.Add("[00:30.00][01:05.00]Chorus\n")
	f.Add("[ar:X]\n[00:10.00]a\n\n")
	f.Add("[00:12.00]<00:12.00>Hi<00:13.00>there\n")
	f.Add("[02:14.00][00:45.00]out\r\n[01:00.00]mid\r\n")
	f.Add("[00:12.00] [00:45.00]spaced")

	f.Fuzz(func(t *testing.T, body string) {
		out1, changed1 := NormalizeBody(body)
		// Idempotency: a second pass is a no-op (critical for a re-runnable backfill).
		out2, changed2 := NormalizeBody(out1)
		if out2 != out1 || changed2 {
			t.Fatalf("not idempotent: out1=%q out2=%q changed2=%v", out1, out2, changed2)
		}
		// changed=false must mean the body is byte-identical (never a silent edit).
		if !changed1 && out1 != body {
			t.Fatalf("changed=false but body differs: in=%q out=%q", body, out1)
		}
		// No output line retains two adjacent leading timestamps.
		for _, line := range splitLines(out1) {
			if _, did := splitStackedLine(line); did {
				t.Fatalf("output still carries a stacked line: %q", line)
			}
		}
	})
}

func TestNormalizeBody_Guards(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{
			name:        "non-stacked body preserved verbatim, unchanged",
			in:          "[ar:Artist]\n[ti:Title]\n[00:10.00]One\n\n[00:20.00]Two\n",
			want:        "[ar:Artist]\n[ti:Title]\n[00:10.00]One\n\n[00:20.00]Two\n",
			wantChanged: false,
		},
		{
			name:        "exact timestamp substrings + text preserved (ms, 1-digit frac, no re-render)",
			in:          "[00:39.267][01:03.5]  Line \n",
			want:        "[00:39.267]  Line \n[01:03.5]  Line \n",
			wantChanged: true,
		},
		{
			name:        "CRLF line endings preserved",
			in:          "[00:30.00][01:05.00]C\r\n",
			want:        "[00:30.00]C\r\n[01:05.00]C\r\n",
			wantChanged: true,
		},
		{
			name:        "mixed line endings preserved per-line (untouched lines keep their EOL)",
			in:          "[ar:X]\n[00:30.00][01:05.00]C\r\n[00:10.00]tail\n",
			want:        "[ar:X]\n[00:30.00]C\r\n[01:05.00]C\r\n[00:10.00]tail\n",
			wantChanged: true,
		},
		{
			name:        "out-of-order stamps sorted ascending within the line",
			in:          "[02:14.00][00:45.00]Chorus\n",
			want:        "[00:45.00]Chorus\n[02:14.00]Chorus\n",
			wantChanged: true,
		},
		{
			name:        "whitespace between stamps is NOT stacked (left verbatim)",
			in:          "[00:12.00] [00:45.00]word\n",
			want:        "[00:12.00] [00:45.00]word\n",
			wantChanged: false,
		},
		{
			name:        "enhanced A2 word-sync left untouched",
			in:          "[00:12.00]<00:12.00>Hi<00:13.00>there\n",
			want:        "[00:12.00]<00:12.00>Hi<00:13.00>there\n",
			wantChanged: false,
		},
		{
			name:        "no trailing newline preserved",
			in:          "[00:30.00][01:05.00]C",
			want:        "[00:30.00]C\n[01:05.00]C",
			wantChanged: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed := NormalizeBody(tt.in)
			if changed != tt.wantChanged {
				t.Errorf("changed: want %v, got %v", tt.wantChanged, changed)
			}
			if out != tt.want {
				t.Errorf("body:\n want %q\n got  %q", tt.want, out)
			}
			// Idempotency: a second pass never changes an already-normalized body.
			out2, changed2 := NormalizeBody(out)
			if changed2 || out2 != out {
				t.Errorf("not idempotent: changed2=%v, out2=%q", changed2, out2)
			}
		})
	}
}

func TestNormalizeBody_SplitsStacked(t *testing.T) {
	in := "[00:30.00][01:05.00][02:10.00]Chorus\n"
	out, changed := NormalizeBody(in)

	want := "[00:30.00]Chorus\n[01:05.00]Chorus\n[02:10.00]Chorus\n"
	if !changed {
		t.Error("want changed=true")
	}
	if out != want {
		t.Errorf("want %q, got %q", want, out)
	}
}
