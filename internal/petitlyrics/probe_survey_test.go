package petitlyrics

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// maxSamples bounds the sweep independently of the track list length, so a
// longer list is truncated rather than silently extending the run.
const maxSamples = 100

// minTrackList is a setup-error floor. Below this, tier-2 captures are too few to
// be a corpus and the isOfficial cross-tabulation has no cells.
const minTrackList = 50

// maxConsecutiveTransportErrors aborts a run that has started measuring the
// network rather than the API. One timeout is noise; three in a row is not.
const maxConsecutiveTransportErrors = 3

// TestSurveyProbe performs ONE paced live sweep against the real petitlyrics API
// and reports aggregates. It answers three questions off the same responses:
// whether isOfficial discriminates usefully (#615/#600), what word-sync coverage
// looks like on this library (#480), and whether a tier-2 corpus can be
// collected (#602).
//
// It is gated on PLPROBE=1 and makes real outbound requests. Run it explicitly:
//
//	PLPROBE_DIR=/path/to/scratchpad/plprobe PLPROBE=1 \
//	  go test -run TestSurveyProbe ./internal/petitlyrics -v -timeout 90m
//
// The track list at $PLPROBE_DIR/tracks.tsv is artist<TAB>title<TAB>album per
// line. Neither it nor the captures may live under the repo.
func TestSurveyProbe(t *testing.T) {
	if os.Getenv("PLPROBE") != "1" {
		t.Skip("live probe: set PLPROBE=1 to run")
	}

	dir := os.Getenv("PLPROBE_DIR")
	if dir == "" {
		t.Fatal("PLPROBE_DIR must be set to a scratchpad directory outside the repo")
	}
	repoRoot, err := findRepoRoot(".")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	dir, err = ensureOutsideRepo(dir, repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	captureDir := filepath.Join(dir, "captures")
	if err := os.MkdirAll(captureDir, 0o700); err != nil {
		t.Fatalf("create capture dir: %v", err)
	}

	tracks, err := readTrackList(filepath.Join(dir, "tracks.tsv"))
	if err != nil {
		t.Fatalf("read track list: %v", err)
	}
	if len(tracks) < minTrackList {
		t.Fatalf("track list has %d entries, need at least %d", len(tracks), minTrackList)
	}
	if len(tracks) > maxSamples {
		t.Logf("track list has %d entries; truncating to %d", len(tracks), maxSamples)
		tracks = tracks[:maxSamples]
	}

	ct := newCaptureTransport(captureDir)
	c := NewClient()
	c.httpClient = &http.Client{
		Timeout:       30 * time.Second,
		Transport:     ct,
		CheckRedirect: c.checkRedirect,
	}
	c.WithMinInterval(DefaultMinInterval)

	report := newSurveyReport()
	ctx := context.Background()
	consecutiveTransportErrors := 0
	var abortBanner string

	for i, track := range tracks {
		obs, fatal := surveySample(ctx, c, track, captureDir, i)
		report.add(obs)

		if fatal != nil {
			// Record the error CLASS, never the wrapped error: a wrapped message
			// could in principle carry a track value, and this banner is written
			// into the shareable artifact.
			abortBanner = abortedBanner(i, len(tracks), obs.Err,
				"an auth or throttle failure makes later lookups resemble misses")
			t.Logf("ABORT at sample %d of %d: %v", i, len(tracks), fatal)
			break
		}

		if obs.Err == "transport" {
			consecutiveTransportErrors++
			if consecutiveTransportErrors >= maxConsecutiveTransportErrors {
				abortBanner = abortedBanner(i, len(tracks), "transport",
					fmt.Sprintf("%d consecutive transport errors; the run is measuring the network",
						consecutiveTransportErrors))
				t.Logf("ABORT at sample %d: %d consecutive transport errors; "+
					"the run is measuring the network", i, consecutiveTransportErrors)
				break
			}
		} else {
			consecutiveTransportErrors = 0
		}
	}

	// The banner is PREPENDED to the rendered report rather than only logged,
	// because report.txt is the artifact that travels to an issue and a reader
	// there has no access to the test log. A truncated run that looks complete
	// would be read as poor coverage rather than as a dead credential.
	out := abortBanner + report.render()
	t.Logf("\n===== SURVEY REPORT (safe to share) =====\n%s", out)

	reportPath := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(reportPath, []byte(out), 0o600); err != nil {
		t.Errorf("write report: %v", err)
	}
	t.Logf("report written to %s", reportPath)
	t.Logf("captures written: %d (raw, keep out of the repo)", ct.count())
}

// abortedBanner renders the header prepended to a truncated run's report.
//
// Aggregate-only, like the report it fronts: a sample INDEX, a sentinel error
// CLASS, and a fixed explanation. No path, artist, title, album, or lyric text
// can reach it, so the "safe to share" label still holds on an aborted run.
func abortedBanner(idx, planned int, errClass, why string) string {
	if errClass == "" {
		errClass = "unknown"
	}
	return fmt.Sprintf(
		"***** RUN ABORTED - RESULTS ARE SUSPECT, NOT PARTIAL *****\n"+
			"stopped at sample %d of %d planned; cause: %s\n"+
			"%s, so the counts below UNDERSTATE coverage by an unknown amount.\n"+
			"Do not read the tier distribution or the miss rate as a measurement.\n"+
			"**********************************************************\n\n",
		idx, planned, errClass, why)
}

// findRepoRoot walks up from start looking for the directory holding go.mod.
//
// The root is located by MARKER, never by directory name. A name check
// ("does the path contain canticle/") is wrong twice over: the repo can be
// cloned to any path, and an unrelated directory that merely contains the name
// would be rejected. The marker is the only thing that actually identifies the
// module root.
func findRepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", start, err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return resolveSymlinks(dir), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found walking up from %q", start)
		}
		dir = parent
	}
}

// resolveSymlinks returns the fully-resolved path when it exists, and the input
// unchanged when it does not. Resolving matters for containment: on macOS a
// scratchpad under /tmp resolves to /private/tmp, and comparing an unresolved
// path against a resolved root can make two names for the same directory look
// like different places.
func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// ensureOutsideRepo rejects a probe directory that is the repo root or anything
// beneath it, returning the absolute, symlink-resolved path on success.
//
// This is a real containment check, not a substring match. Raw captures of a
// private music library landing anywhere inside the working tree are one
// `git add -A` away from a public surface, so the guard has to hold for the repo
// ROOT itself and for every path under it, whatever they are named.
func ensureOutsideRepo(dir, repoRoot string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve PLPROBE_DIR %q: %w", dir, err)
	}
	abs = resolveSymlinks(abs)

	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		// Different volumes (or otherwise unrelatable), so it cannot be inside.
		return abs, nil //nolint:nilerr // reason: an unrelatable path is provably not under repoRoot, which is exactly what this function checks; the Rel error is the evidence, not a failure.
	}
	// Rel yields "." for the root itself and a path starting with ".." only when
	// abs escapes repoRoot. Anything else is inside.
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"PLPROBE_DIR must not be the repo or under it: %s is inside %s (resolved to %s)",
			dir, repoRoot, abs)
	}
	return abs, nil
}

// surveySample performs one lookup and classifies the result. The second return
// is non-nil for a class that must abort the whole run.
func surveySample(ctx context.Context, c *Client, track models.Track, captureDir string, idx int) (sampleObservation, error) {
	songs, err := c.request(ctx, track, tierWordSync)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnauthorized):
			// The clientAppID is a hardcoded constant shared by every canticle
			// install (#607). If it is revoked mid-sweep every later lookup
			// resembles a miss, and the coverage number would read as poor
			// coverage rather than a dead credential.
			return sampleObservation{Err: "ErrUnauthorized"}, fmt.Errorf("credential rejected: %w", err)
		case errors.Is(err, ErrForbidden):
			// Per errors.go, a refused request shape rather than throttling. Means
			// the probe talks to the service differently than canticle does, which
			// invalidates the premise of the measurement.
			return sampleObservation{Err: "ErrForbidden"}, fmt.Errorf("request refused: %w", err)
		case errors.Is(err, ErrRateLimited):
			// Prod does not query petitlyrics, so a 429 at this pacing means the
			// floor is wrong. A real #535 finding worth stopping for.
			return sampleObservation{Err: "ErrRateLimited"}, fmt.Errorf("throttled at sample %d: %w", idx, err)
		case errors.Is(err, ErrNotFound):
			return sampleObservation{Err: "ErrNotFound"}, nil
		default:
			return sampleObservation{Err: "transport"}, nil
		}
	}

	candidate, err := selectCandidate(songs, track)
	if err != nil {
		return sampleObservation{Err: "no-candidate"}, nil
	}
	if candidate.LyricsData == "" {
		return sampleObservation{Err: "empty-payload"}, nil
	}

	raw, err := base64.StdEncoding.DecodeString(candidate.LyricsData)
	if err != nil {
		return sampleObservation{Err: "base64"}, nil
	}

	obs := sampleObservation{
		Tier:       classifyPayload(raw),
		IsOfficial: candidate.IsOfficial,
		Copyright:  candidate.Copyright,
	}

	switch obs.Tier {
	case tierWordSync:
		cues, timings, decErr := decodeWordSync(raw)
		if decErr != nil {
			obs.Err = "wsy-decode"
			return obs, nil
		}
		obs.CueCount = len(cues)
		obs.DistinctRatio = distinctStartRatio(cues, timings)
	case tierLineSync:
		// The #602 corpus. The capture transport already wrote the full response;
		// write the decoded binary payload separately so the follow-on has the
		// exact bytes without re-parsing the envelope.
		path := filepath.Join(captureDir, fmt.Sprintf("lsy-%04d.bin", idx))
		if writeErr := os.WriteFile(path, raw, 0o600); writeErr != nil {
			obs.Err = "lsy-write"
		}
	}

	return obs, nil
}

// distinctStartRatio reports the fraction of lines whose words do NOT all share
// one start time. A line where every word shares a timestamp is effectively
// line-level only, which the A2 writer (#480) has to handle per line rather than
// assuming uniform coverage.
//
// A ONE-WORD line necessarily has a single start time, so it counts as
// non-distinct and sits in the denominator alongside a multi-word line whose
// words all share a timestamp. That is deliberate: both are line-level-only for
// the writer's purposes, and the ratio measures usable word granularity, not the
// provider's intent. A corpus of short lines therefore reads as low coverage,
// which is the honest reading of what a per-word writer would get.
func distinctStartRatio(cues []models.Lines, timings []models.WordTiming) float64 {
	if len(cues) == 0 {
		return 0
	}
	starts := make(map[int]map[int]struct{}, len(cues))
	for _, wt := range timings {
		if starts[wt.Line] == nil {
			starts[wt.Line] = map[int]struct{}{}
		}
		starts[wt.Line][wt.StartMS] = struct{}{}
	}
	distinct := 0
	for i := range cues {
		if len(starts[i]) > 1 {
			distinct++
		}
	}
	return float64(distinct) / float64(len(cues))
}

// readTrackList parses artist<TAB>title<TAB>album per line.
//
// The open below is a variable path (gosec G304), which is fine here and needs
// no `//nolint`. gosec is excluded on `_test.go` by `.golangci.yml`, so a
// directive would suppress nothing and nolintlint would flag it as dead. On the
// merits, the path is built from PLPROBE_DIR, an operator-supplied scratchpad
// path for a manually-invoked probe; there is no untrusted input and no server
// context.
func readTrackList(path string) ([]models.Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open track list: %w", err)
	}
	defer func() { _ = f.Close() }()

	var tracks []models.Track
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		tr := models.Track{ArtistName: parts[0], TrackName: parts[1]}
		if len(parts) > 2 {
			tr.AlbumName = parts[2]
		}
		tracks = append(tracks, tr)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan track list: %w", err)
	}
	return tracks, nil
}

// TestEnsureOutsideRepo_RejectsEveryPathUnderTheRepo is the load-bearing test
// for the containment guard. The guard exists to keep raw captures of a private
// music library out of the working tree, where a single `git add -A` would put
// them on a public surface, so a guard that accepts the repo ROOT or an
// arbitrarily-named subdirectory of it is not a guard at all.
//
// The earlier substring form ("does the path contain canticle/internal") failed
// exactly these cases; each is pinned here so it cannot come back.
func TestEnsureOutsideRepo_RejectsEveryPathUnderTheRepo(t *testing.T) {
	repoRoot := t.TempDir()
	repoRoot = resolveSymlinks(repoRoot)
	outside := resolveSymlinks(t.TempDir())

	inside := []struct {
		name string
		dir  string
	}{
		{"the repo root itself", repoRoot},
		{"a plainly-named subdirectory", filepath.Join(repoRoot, "plprobe")},
		{"a scratch subdirectory", filepath.Join(repoRoot, "scratch")},
		{"a package directory", filepath.Join(repoRoot, "internal", "petitlyrics")},
		{"a docs directory", filepath.Join(repoRoot, "docs", "captures")},
		{"a deeply nested directory", filepath.Join(repoRoot, "a", "b", "c", "d")},
		{"an unclean path that resolves inside", filepath.Join(repoRoot, "x", "..", "y")},
	}
	for _, tc := range inside {
		t.Run("reject "+tc.name, func(t *testing.T) {
			got, err := ensureOutsideRepo(tc.dir, repoRoot)
			if err == nil {
				t.Fatalf("ensureOutsideRepo(%q) accepted a path inside the repo, returned %q", tc.dir, got)
			}
			// The message must name both sides, or an operator cannot tell which
			// of the two paths is the one they need to change.
			if !strings.Contains(err.Error(), repoRoot) {
				t.Errorf("error does not name the repo root: %v", err)
			}
		})
	}

	outsideCases := []struct {
		name string
		dir  string
	}{
		{"a sibling scratchpad", outside},
		{"a subdirectory of a sibling", filepath.Join(outside, "plprobe")},
		{"a path merely NAMED like the repo", filepath.Join(outside, "canticle", "internal")},
		{"a sibling sharing a name prefix", repoRoot + "-notes"},
	}
	for _, tc := range outsideCases {
		t.Run("accept "+tc.name, func(t *testing.T) {
			got, err := ensureOutsideRepo(tc.dir, repoRoot)
			if err != nil {
				t.Fatalf("ensureOutsideRepo(%q) rejected a path outside the repo: %v", tc.dir, err)
			}
			if !filepath.IsAbs(got) {
				t.Errorf("returned path is not absolute: %q", got)
			}
		})
	}
}

// TestEnsureOutsideRepo_ResolvesRelativeAndSymlinkedPaths pins that containment
// is decided on the RESOLVED path. A relative path or a symlink into the tree
// would otherwise walk straight past a check done on the raw string.
func TestEnsureOutsideRepo_ResolvesRelativeAndSymlinkedPaths(t *testing.T) {
	repoRoot := resolveSymlinks(t.TempDir())
	outside := resolveSymlinks(t.TempDir())

	// A symlink living outside the repo but POINTING inside it must be rejected:
	// writes through it land in the working tree.
	target := filepath.Join(repoRoot, "captures")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(outside, "sneaky")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ensureOutsideRepo(link, repoRoot); err == nil {
		t.Error("a symlink pointing into the repo was accepted")
	}

	// A relative path is resolved against the working directory, which during
	// this test is the package directory inside the repo.
	if _, err := ensureOutsideRepo(".", mustRepoRoot(t)); err == nil {
		t.Error(`ensureOutsideRepo(".") accepted the package directory itself`)
	}
}

// TestFindRepoRoot_LocatesByMarkerNotName pins that the root is found via go.mod
// rather than a hardcoded directory name, so the guard still works in a clone
// under any path.
func TestFindRepoRoot_LocatesByMarkerNotName(t *testing.T) {
	root := mustRepoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("located root %q has no go.mod: %v", root, err)
	}
	// The package directory must resolve to that same root.
	if _, err := os.Stat(filepath.Join(root, "internal", "petitlyrics")); err != nil {
		t.Errorf("located root %q does not contain this package: %v", root, err)
	}

	// A synthetic tree with no marker must fail rather than walking to /.
	bare := t.TempDir()
	deep := filepath.Join(bare, "a", "b")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// This only proves the negative when no ancestor of the temp dir carries a
	// go.mod, which is true of the OS temp root.
	if got, err := findRepoRoot(deep); err == nil && strings.HasPrefix(got, bare) {
		t.Errorf("findRepoRoot invented a root inside a marker-less tree: %q", got)
	}
}

func mustRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot(".")
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}
	return root
}

// TestAbortedBanner_TravelsWithTheReportAndStaysAggregate pins the two
// properties that make the banner worth having: it is PART of the rendered
// report (so it reaches an issue, where the test log does not), and it carries
// nothing but an index and an error class.
func TestAbortedBanner_TravelsWithTheReportAndStaysAggregate(t *testing.T) {
	banner := abortedBanner(34, 100, "ErrUnauthorized", "the credential died")

	for _, want := range []string{"ABORTED", "SUSPECT", "34", "100", "ErrUnauthorized"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner does not mention %q:\n%s", want, banner)
		}
	}

	// The banner fronts the report, so a reader sees the caveat before the counts.
	r := newSurveyReport()
	r.add(sampleObservation{Tier: tierWordSync})
	out := banner + r.render()
	if !strings.HasPrefix(out, "*****") {
		t.Errorf("banner is not the first thing in the artifact:\n%s", out)
	}
	if strings.Index(out, "ABORTED") > strings.Index(out, "samples:") {
		t.Error("the abort caveat appears after the counts it qualifies")
	}

	// An empty error class must still render a usable cause, not a blank.
	if got := abortedBanner(0, 10, "", "why"); strings.Contains(got, "cause: \n") {
		t.Errorf("empty error class rendered as a blank cause:\n%s", got)
	}
}

// TestDistinctStartRatio pins the arithmetic the #480 sizing decision rests on,
// including the documented treatment of single-word lines as non-distinct.
func TestDistinctStartRatio(t *testing.T) {
	cues := []models.Lines{{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"}}
	timings := []models.WordTiming{
		// Line 0: two words, distinct starts -> counts as distinct.
		{Line: 0, StartMS: 100}, {Line: 0, StartMS: 200},
		// Line 1: two words sharing one start -> line-level only.
		{Line: 1, StartMS: 300}, {Line: 1, StartMS: 300},
		// Line 2: a single word -> necessarily one start, so non-distinct.
		{Line: 2, StartMS: 400},
		// Line 3: no timings at all.
	}
	if got := distinctStartRatio(cues, timings); got != 0.25 {
		t.Errorf("distinctStartRatio = %v, want 0.25 (only line 0 is distinct)", got)
	}

	if got := distinctStartRatio(nil, timings); got != 0 {
		t.Errorf("distinctStartRatio(nil cues) = %v, want 0 (no divide by zero)", got)
	}
	if got := distinctStartRatio(cues, nil); got != 0 {
		t.Errorf("distinctStartRatio(no timings) = %v, want 0", got)
	}
}

// TestReadTrackList parses a synthetic list. The fixture is written at test time
// with obviously-synthetic values: no real library metadata belongs in the repo.
func TestReadTrackList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tracks.tsv")
	content := "ArtistA\tTitleA\tAlbumA\n" +
		"\n" +
		"ArtistB\tTitleB\n" +
		"malformed-single-field\n" +
		"  \n" +
		"ArtistC\tTitleC\tAlbumC\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tracks, err := readTrackList(path)
	if err != nil {
		t.Fatalf("readTrackList: %v", err)
	}
	if len(tracks) != 3 {
		t.Fatalf("got %d tracks, want 3 (blank and single-field lines skipped): %+v", len(tracks), tracks)
	}
	if tracks[0].ArtistName != "ArtistA" || tracks[0].TrackName != "TitleA" || tracks[0].AlbumName != "AlbumA" {
		t.Errorf("first track parsed incorrectly: %+v", tracks[0])
	}
	if tracks[1].AlbumName != "" {
		t.Errorf("two-field line should leave the album empty: %+v", tracks[1])
	}

	if _, err := readTrackList(filepath.Join(dir, "absent.tsv")); err == nil {
		t.Error("a missing track list must be an error, not an empty sweep")
	}
}
