package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func canticleLRCBody(extraTags []string) string {
	var sb strings.Builder
	sb.WriteString("[by:canticle]\n[ar:Test Artist]\n[ti:Test Track]\n")
	for _, tag := range extraTags {
		sb.WriteString(tag + "\n")
	}
	sb.WriteString("[ve:1.14.0]\n[00:01.00]hello\n")
	return sb.String()
}

func TestInjectEditorTag_Decisions(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantInjected bool
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "canticle-written gets tagged immediately before ve",
			body:         canticleLRCBody(nil),
			wantInjected: true,
			wantContains: []string{"[re:canticle]\n[ve:1.14.0]", "[00:01.00]hello"},
		},
		{
			name:         "existing re preserved, never duplicated",
			body:         canticleLRCBody([]string{"[re:some-other-tool]"}),
			wantInjected: false,
			wantContains: []string{"[re:some-other-tool]"},
			wantAbsent:   []string{"[re:canticle]"},
		},
		{
			name:         "foreign by+ve pair untouched",
			body:         "[by:OtherApp]\n[ar:A]\n[ti:T]\n[ve:2.0.0]\n[00:01.00]hi\n",
			wantInjected: false,
			wantAbsent:   []string{"[re:"},
		},
		{
			name:         "canticle by with no ve is a no-op",
			body:         "[by:canticle]\n[ar:A]\n[ti:T]\n[00:01.00]hi\n",
			wantInjected: false,
			wantAbsent:   []string{"[re:"},
		},
		{
			// #483 hostile-review finding 5: v1.7.0-written files predate the
			// 98bac8b rename from mxlrcgo-svc to canticle and are still
			// canticle-written.
			name:         "legacy mxlrcgo-svc by tag is canticle-written too",
			body:         "[by:mxlrcgo-svc]\n[ar:A]\n[ti:T]\n[ve:1.7.0]\n[00:01.00]hi\n",
			wantInjected: true,
			wantContains: []string{"[re:canticle]\n[ve:1.7.0]"},
		},
		{
			// #483 hostile-review finding 4: this package's backup cannot
			// restore a stripped BOM, so a BOM-prefixed file is left alone.
			name:         "BOM-prefixed file is skipped, never mutated",
			body:         "\xEF\xBB\xBF" + canticleLRCBody(nil),
			wantInjected: false,
			wantAbsent:   []string{"[re:"},
		},
		{
			// #483 hostile-review finding 4: mixed CRLF/LF cannot be restored
			// byte-for-byte either, so the file is left alone rather than
			// normalized to one style.
			name:         "mixed CRLF/LF line endings is skipped, never mutated",
			body:         "[by:canticle]\r\n[ar:A]\n[ti:T]\n[ve:1.14.0]\n[00:01.00]hi\n",
			wantInjected: false,
			wantAbsent:   []string{"[re:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "track.lrc")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			injected, err := InjectEditorTag(path)
			if err != nil {
				t.Fatalf("InjectEditorTag: %v", err)
			}
			if injected != tc.wantInjected {
				t.Errorf("injected = %v, want %v", injected, tc.wantInjected)
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("read: %v", rerr)
			}
			got := string(data)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q, got:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("unwanted %q present, got:\n%s", absent, got)
				}
			}
			if tc.body != got && !tc.wantInjected {
				t.Errorf("file changed despite injected=false:\nbefore:\n%s\nafter:\n%s", tc.body, got)
			}
		})
	}
}

func TestInjectEditorTag_IdempotentSecondRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.lrc")
	if err := os.WriteFile(path, []byte(canticleLRCBody(nil)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := InjectEditorTag(path); err != nil {
		t.Fatalf("first InjectEditorTag: %v", err)
	}
	first, _ := os.ReadFile(path)

	injected, err := InjectEditorTag(path)
	if err != nil {
		t.Fatalf("second InjectEditorTag: %v", err)
	}
	if injected {
		t.Error("second run reported injected=true; want false (already tagged)")
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("second run changed content:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Count(string(second), "[re:") != 1 {
		t.Errorf("expected exactly one [re:] tag, got:\n%s", second)
	}
}

func TestEditorTagEligible_NoWriteAndMatchesInjectDecision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.lrc")
	if err := os.WriteFile(path, []byte(canticleLRCBody(nil)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := os.ReadFile(path)

	eligible, err := EditorTagEligible(path)
	if err != nil {
		t.Fatalf("EditorTagEligible: %v", err)
	}
	if !eligible {
		t.Error("eligible = false; want true")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("EditorTagEligible must never write to disk")
	}

	if _, err := InjectEditorTag(path); err != nil {
		t.Fatalf("InjectEditorTag: %v", err)
	}
	if eligible, err = EditorTagEligible(path); err != nil {
		t.Fatalf("EditorTagEligible after inject: %v", err)
	} else if eligible {
		t.Error("eligible = true after the tag was already added; want false")
	}
}

func TestInjectEditorTag_SkipsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges on windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.lrc")
	if err := os.WriteFile(real, []byte(canticleLRCBody(nil)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "link.lrc")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	injected, err := InjectEditorTag(link)
	if err != nil {
		t.Fatalf("InjectEditorTag: %v", err)
	}
	if injected {
		t.Error("injected = true for a symlink; want false (symlinks are never followed)")
	}
	data, _ := os.ReadFile(real)
	if strings.Contains(string(data), "[re:") {
		t.Error("the symlink target was modified even though the symlink itself must be skipped")
	}
}

// TestInjectEditorTag_ChangedDuringRewrite pins #483 hostile-review finding 3:
// a concurrent writer (the worker's own atomic write, a revalidate demotion)
// that touches path between InjectEditorTag's read and its rename must win --
// the rewrite must never clobber it. injectEditorTagPreRenameHook lets the
// test land its own write deterministically inside that window instead of
// racing a goroutine against rename timing.
func TestInjectEditorTag_ChangedDuringRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.lrc")
	if err := os.WriteFile(path, []byte(canticleLRCBody(nil)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	orig := injectEditorTagPreRenameHook
	t.Cleanup(func() { injectEditorTagPreRenameHook = orig })
	const concurrentWrite = "[by:SomeOtherWriter]\n[ar:A]\n[ti:T]\n[ve:9.9.9]\n[00:02.00]changed\n"
	injectEditorTagPreRenameHook = func(p string) {
		if werr := os.WriteFile(p, []byte(concurrentWrite), 0o600); werr != nil {
			t.Fatalf("simulate concurrent write: %v", werr)
		}
	}

	injected, err := InjectEditorTag(path)
	if injected {
		t.Error("injected = true; want false, the file changed mid-rewrite")
	}
	if !errors.Is(err, ErrChangedDuringRewrite) {
		t.Fatalf("err = %v; want ErrChangedDuringRewrite", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != concurrentWrite {
		t.Errorf("the concurrent write was overwritten; got:\n%s\nwant:\n%s", got, concurrentWrite)
	}
}

// TestInjectEditorTag_TargetVanishedDuringRewrite pins #483 hostile-review
// finding 3's other hazard: a target removed by another writer (a purge, a
// quarantine) mid-rewrite must stay absent, never be recreated.
func TestInjectEditorTag_TargetVanishedDuringRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "track.lrc")
	if err := os.WriteFile(path, []byte(canticleLRCBody(nil)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	orig := injectEditorTagPreRenameHook
	t.Cleanup(func() { injectEditorTagPreRenameHook = orig })
	injectEditorTagPreRenameHook = func(p string) {
		if rerr := os.Remove(p); rerr != nil {
			t.Fatalf("simulate concurrent removal: %v", rerr)
		}
	}

	injected, err := InjectEditorTag(path)
	if injected {
		t.Error("injected = true; want false, the target vanished mid-rewrite")
	}
	if !errors.Is(err, ErrChangedDuringRewrite) {
		t.Fatalf("err = %v; want ErrChangedDuringRewrite", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("InjectEditorTag recreated a target that was removed concurrently; want it to stay absent")
	}
}
