package failsig

import (
	"strings"
	"testing"
)

// #1167: shapes the original rules passed through. Every input is synthetic.
// Each row names the private fragment that must not survive.
func TestNormalizeStripsTrailingPathsURLsAndTrackText(t *testing.T) {
	for _, tc := range []struct {
		name, in, want, leak string
	}{
		{"trailing path with extension", "open /srv/synthetic dir/a b/track.flac", "open <path>", "synthetic"},
		{"trailing stat dir, no extension", "stat /srv/Some Artist/Album", "stat <path>", "Some Artist"},
		{"trailing lstat dir, line inside a join", "first: thing\nlstat /srv/Some Artist/Album\nlast", "first: thing\nlstat <path>\nlast", "Some Artist"},
		{"ffmpeg exited trailing path", "detector: ffmpeg exited: /srv/Some Artist/clip", "detector: ffmpeg exited: <path>", "Some Artist"},
		{"verb path keeps its delimited cause", "stat /srv/Some Artist/Album: no such file or directory", "stat <path>: no such file or directory", "Some Artist"},
		{"tab-delimited trailing path", "write\t/srv/Some Artist/Album/x.lrc\tdisk full", "write\t<path>\tdisk full", "Some Artist"},
		{"windows path with delimiter", `open D:\Music\Some Artist\track.flac: access denied`, "open <path>: access denied", "Some Artist"},
		{"windows path to eol", `stat C:\Music\Some Artist\track.flac`, "stat <path>", "Some Artist"},
		{"windows dir to eol after verb", `stat C:\Music\Some Artist\Album`, "stat <path>", "Some Artist"},
		{"unc path with delimiter", `open \\nas\music\Some Artist\x.flac: access denied`, "open <path>: access denied", "Some Artist"},
		{"unc path to eol", `read \\nas\music\Some Artist\x.flac`, "read <path>", "Some Artist"},
		{"quoted url with query", `Get "https://api.example.invalid/x?token=SECRET&k=v": EOF`, `Get "<url>": EOF`, "SECRET"},
		{"bare url with query", "fetch http://host.invalid:8080/p?q=1 failed", "fetch <url> failed", "q=1"},
		{"bracketed ipv6 url with query", `Get "http://[fd00::1]:8080/x?usertoken=SECRET": EOF`, `Get "<url>": EOF`, "SECRET"},
		{"bare bracketed ipv6 url", "dial http://[fd00::1]:8080/x?usertoken=SECRET refused", "dial <url> refused", "SECRET"},
		{"file url with spaces to eol", "open file:///srv/Some Artist/x.flac", "open <url>", "Some Artist"},
		{"smb url with spaces and cause", "read smb://nas/music/Some Artist/x.flac: host is down", "read <url>: host is down", "Some Artist"},
		{"nothing to save", "writer: nothing to save for Some Artist - Song", "writer: nothing to save for <track>", "Some Artist"},
		{"nothing to save keeps its cause", "worker: nothing to save for Some Artist - Song: context deadline exceeded", "worker: nothing to save for <track>: context deadline exceeded", "Some Artist"},
		{"nothing to save, title with colon", "writer: nothing to save for Some Artist - Song: Part Two", "writer: nothing to save for <track>", "Part Two"},
		{"nothing to save, title with feat", "writer: nothing to save for Some Artist - Song (feat. Other Artist)", "writer: nothing to save for <track>", "Other Artist"},
		{"nothing to save inside a join", "nothing to save for Some Artist - Song\nsecond error", "nothing to save for <track>\nsecond error", "Some Artist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, tc.leak) {
				t.Errorf("leak %q survived: %q", tc.leak, got)
			}
			if again := Normalize(got); again != got {
				t.Errorf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// The other half of #1167: the new rules must not merge distinct failures or
// erase fixed text. Each pair must stay two groups.
func TestNormalizeNewRulesKeepDistinctFailures(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"windows path, two prose causes", `open D:\m\x.flac permission denied`, `open D:\m\x.flac checksum mismatch`},
		{"unc path, two prose causes", `open \\nas\m\x.flac permission denied`, `open \\nas\m\x.flac checksum mismatch`},
		{"posix verb path, two prose causes", "stat /mnt/a permission denied", "stat /mnt/a checksum mismatch"},
		{"nothing to save, two causes", "nothing to save for A - B: context deadline exceeded", "nothing to save for A - B: permission denied"},
		{"url, two causes", `Get "http://h.invalid/x?a=1": EOF`, `Get "http://h.invalid/x?a=1": connection refused`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a, b := Normalize(tc.a), Normalize(tc.b); a == b {
				t.Errorf("two distinct failures collapsed into %q", a)
			}
		})
	}
}

// Fixed emitter text that the track and path rules must leave alone.
func TestNormalizeNewRulesLeaveFixedTextAlone(t *testing.T) {
	for _, s := range []string{
		// petitlyrics.ErrProviderUnavailable, verbatim shape.
		"petitlyrics: provider returned no results for 20 consecutive lookups (application id revoked?): petitlyrics: no results found",
		"orchestrator: no match for this track",
		"read lrc: permission denied",
	} {
		if got := Normalize(s); got != s {
			t.Errorf("fixed text was altered:\n got %q\nwant %q", got, s)
		}
	}
}

// Classify must still see a cause that follows a normalized track or path.
func TestNormalizeKeepsCauseForClassify(t *testing.T) {
	sig := Normalize("worker: nothing to save for Some Artist - Song: context deadline exceeded")
	if got := Classify(sig); got != Transient {
		t.Errorf("Classify(%q) = %s; want transient", sig, got)
	}
}

// #1167 round 2: shapes found by the second hostile pass. Every input is
// synthetic.
func TestNormalizeRound2Shapes(t *testing.T) {
	for _, tc := range []struct {
		name, in, want, leak string
	}{
		// A ")" or "]" in a URL path once ended the match, leaving the query.
		{"url query after a paren in the path", `Get "http://h.invalid/a(b)c?usertoken=SECRET": EOF`, `Get "<url>": EOF`, "SECRET"},
		{"url query after a bracket in the path", "fetch http://h.invalid/a[1]/b?usertoken=SECRET failed", "fetch <url> failed", "SECRET"},
		// os.PathError/LinkError ops that were missing from the verb list.
		{"readdir trailing dir", "readdir /srv/Some Artist/Album", "readdir <path>", "Some Artist"},
		{"opendir trailing dir", "opendir /srv/Some Artist/Album", "opendir <path>", "Some Artist"},
		{"symlink trailing dir", "symlink /srv/Some Artist/Album", "symlink <path>", "Some Artist"},
		{"link trailing dir", "link /srv/Some Artist/Album", "link <path>", "Some Artist"},
		// Verb-less paths that end the text in a known extension.
		{"verb-less windows path", `C:\M\Some Artist\x.flac`, "<path>", "Some Artist"},
		{"verb-less posix path", "/srv/Some Artist/x.flac", "<path>", "Some Artist"},
		// A spaced last segment, so the verb rule cannot claim it and only the
		// case-insensitive extension can.
		{"upper-case extension", "/srv/Some Artist/Some Track.FLAC", "<path>", "Some Track"},
		{"sidecar extension", "worker: rename /srv/Some Artist/x.lrc.tmp", "worker: rename <path>", "Some Artist"},
		// The scheme is case-insensitive on both file/smb rules.
		{"upper-case file url with cause", "read FILE:///srv/Some Artist/x.flac: gone", "read <url>: gone", "Some Artist"},
		{"upper-case file url to end", "open FILE:///srv/Some Artist/x.flac", "open <url>", "Some Artist"},
		// Multi-line values (errors.Join). The verb rule needs its (?m): a bare
		// UNC host has no delimited sibling to claim it mid-text.
		{"verb rule, bare unc host mid-text", "open \\\\nas\nsecond error", "open <path>\nsecond error", "nas"},
		{"windows extension path mid-text", "open C:\\M\\Some Artist\\x.flac\nsecond error", "open <path>\nsecond error", "Some Artist"},
		{"posix extension path mid-text", "open /srv/Some Artist/x.flac\nsecond error", "open <path>\nsecond error", "Some Artist"},
		{"output path mid-text", "write output /srv/Some Artist/x\nsecond error", "write output <path>\nsecond error", "Some Artist"},
		{"file url mid-text", "open file:///srv/Some Artist/x\nsecond error", "open <url>\nsecond error", "Some Artist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q; want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, tc.leak) {
				t.Errorf("leak %q survived: %q", tc.leak, got)
			}
			if again := Normalize(got); again != got {
				t.Errorf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// A trailing dotted token is not an extension unless it is one canticle reads
// or writes. The first tail rules accepted any 1-5 alphanumeric suffix, so each
// pair below merged into one group.
func TestNormalizeTrailingDottedProseIsNotAnExtension(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"timeout value", "read /mnt/a timed out after 2.5", "read /mnt/a timed out after 3.0"},
		{"version", "read /mnt/a failed in v1.2", "read /mnt/a failed in v1.3"},
		{"host name", "read /mnt/a via host.com", "read /mnt/a via peer.net"},
		{"musixmatch method", "musixmatch API error: status 404, body: Cannot GET /ws/1.1/track.get", "musixmatch API error: status 404, body: Cannot GET /ws/1.1/macro.get"},
		{"windows timeout value", `open C:\m\a timed out after 2.5`, `open C:\m\a timed out after 3.0`},
		{"unc version", `open \\nas\m\a failed in v1.2`, `open \\nas\m\a failed in v1.3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a, b := Normalize(tc.a), Normalize(tc.b); a == b {
				t.Errorf("two distinct failures collapsed into %q", a)
			}
		})
	}
}
