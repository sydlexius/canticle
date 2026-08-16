package petitlyrics

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
)

// TestProbeBSurvey performs ONE paced live sweep against the real petitlyrics
// API and reports Probe B's isOfficial-agreement measurement, stratified by
// popularity band (issue #614).
//
// It is gated on PLPROBEB=1 and makes real outbound requests. It is meant to
// run ON the prod host (Unraid), against the live SQLite DB and the sidecars
// already on disk, INSIDE the container: see the path-spelling warning below.
// Run it explicitly:
//
//	PLPROBEB_DB=/path/to/canticle.db PLPROBEB_DIR=/path/to/scratchpad/probeb \
//	  PLPROBEB=1 go test -run TestProbeBSurvey ./internal/petitlyrics -v -timeout 6h
//
// PLPROBEB_SAMPLES optionally overrides the sample size (default
// defaultProbeBSamples).
//
// ============================================================================
// PATH-SPELLING WARNING -- READ BEFORE RUNNING (do not remap library roots).
// ============================================================================
// scan_results.outdir/file_path are written by the SCANNER, which runs inside
// the serve container and therefore stores the CONTAINER path spelling (e.g.
// "/Share/Music/..."), never the host spelling (e.g. "/mnt/user/Share/Music"
// or similar bind-mount target). This binary must run WHERE THOSE PATHS
// RESOLVE -- inside the same container, or bind-mounted identically -- with
// no path translation layer. Remapping a root to a host spelling before
// reading a sidecar does not just fail loudly: it reads a DIFFERENT (or
// absent) file, or nothing at all, and the metric silently never moves while
// the code runs without error. There is no config knob here for this on
// purpose: translating paths is the bug, not a feature to add.
// ============================================================================
func TestProbeBSurvey(t *testing.T) {
	if os.Getenv("PLPROBEB") != "1" {
		t.Skip("live probe: set PLPROBEB=1 to run")
	}

	dbPath := os.Getenv("PLPROBEB_DB")
	if dbPath == "" {
		t.Fatal("PLPROBEB_DB must be set to the canticle SQLite database path")
	}

	dir := os.Getenv("PLPROBEB_DIR")
	if dir == "" {
		t.Fatal("PLPROBEB_DIR must be set to a scratchpad directory outside the repo")
	}
	repoRoot, err := findRepoRoot(".")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	dir, err = ensureOutsideRepo(dir, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create scratchpad dir: %v", err)
	}

	sampleSize := defaultProbeBSamples
	if v := os.Getenv("PLPROBEB_SAMPLES"); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil {
			t.Fatalf("PLPROBEB_SAMPLES %q is not an integer: %v", v, parseErr)
		}
		sampleSize = n
	}
	if sampleSize < minProbeBSamples || sampleSize > maxProbeBSamples {
		t.Fatalf("PLPROBEB_SAMPLES=%d out of allowed range [%d, %d]",
			sampleSize, minProbeBSamples, maxProbeBSamples)
	}

	ctx := context.Background()
	sqlDB, err := db.OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("open database read-only: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	counts, err := artistTrackCounts(ctx, sqlDB)
	if err != nil {
		t.Fatalf("compute artist track counts: %v", err)
	}
	if len(counts) < minProbeBSamples {
		t.Fatalf("library has only %d distinct artists; too few to band meaningfully "+
			"(need at least %d)", len(counts), minProbeBSamples)
	}
	bands := bandTracksByArtist(counts)

	rows, err := sampleDoneRows(ctx, sqlDB, sampleSize, bands)
	if err != nil {
		t.Fatalf("sample scan_results rows: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no eligible rows found (status='done' with non-empty artist_key/title_key); nothing to sample")
	}
	t.Logf("sampled %d rows (of %d requested) with an existing local sidecar", len(rows), sampleSize)

	c := NewClient()
	c.WithMinInterval(DefaultMinInterval)

	report := newProbeBReport()
	consecutiveTransportErrors := 0
	var abortBanner string

	for i, row := range rows {
		obs, fatal := probeBSample(ctx, c, row, bands)
		report.add(obs)

		if fatal != nil {
			abortBanner = abortedBanner(i, len(rows), obs.Err,
				"an auth or throttle failure makes later lookups resemble misses")
			t.Logf("ABORT at sample %d of %d: %v", i, len(rows), fatal)
			break
		}

		if obs.Err == "transport" {
			consecutiveTransportErrors++
			if consecutiveTransportErrors >= maxConsecutiveTransportErrors {
				abortBanner = abortedBanner(i, len(rows), "transport",
					fmt.Sprintf("%d consecutive transport errors; the run is measuring the network",
						consecutiveTransportErrors))
				t.Logf("ABORT at sample %d: %d consecutive transport errors", i, consecutiveTransportErrors)
				break
			}
		} else {
			consecutiveTransportErrors = 0
		}
	}

	out := abortBanner + report.render()
	t.Logf("\n===== PROBE B REPORT (safe to share) =====\n%s", out)

	reportPath := filepath.Join(dir, "probeb-report.txt")
	if err := os.WriteFile(reportPath, []byte(out), 0o600); err != nil {
		t.Errorf("write report: %v", err)
	}
	t.Logf("report written to %s", reportPath)
}

// defaultProbeBSamples is the default sample size when PLPROBEB_SAMPLES is
// unset.
//
// Sizing tradeoff (see issue #614's own caution): the issue's original
// estimate of the isOfficial=1 rate was roughly 3 in 14 (about 21%), which
// would need on the order of 140 samples to reach ~30 official hits. A later
// 70-track sweep measured roughly 49% isOfficial=1 on mainstream word-sync
// material -- a much higher rate -- but that sample was drawn from a single
// popularity band, so it must NOT be assumed to generalize to the bottom
// band, which is exactly the population this probe needs to say something
// about. 150 is a middle ground: enough to populate all three bands at the
// higher observed rate, short of the ~140-times-3 that would be needed to
// guarantee the same statistical power at the pessimistic rate in EVERY band
// simultaneously. A caller with more budget should raise PLPROBEB_SAMPLES;
// the report always states its own sample size per band so a thin cell is
// visible rather than hidden.
const defaultProbeBSamples = 150

// minProbeBSamples is a floor below which the tercile split and the
// isOfficial cross-tabulation would have too few cells to say anything.
const minProbeBSamples = 20

// maxProbeBSamples caps a single run independently of what is requested, so a
// misconfigured value cannot turn into an unbounded multi-day sweep against a
// single-egress paced lane. At the client's 30s floor, 500 samples is
// already about 4.2 hours.
const maxProbeBSamples = 500

// probeBRow is one sampled scan_results row: the minimum needed to look up
// the on-disk sidecar and issue a provider request. No lyric text lives on
// this type.
type probeBRow struct {
	artistKey string
	track     models.Track
	outdir    string
	filename  string
}

// artistTrackCounts returns, for every non-empty artist_key in scan_results,
// how many tracks that artist has in the library -- the popularity proxy
// documented on bandTracksByArtist. Computed over the WHOLE table (every
// status), because the proxy is about what the collector acquired, not about
// what has already been fetched.
func artistTrackCounts(ctx context.Context, sqlDB *sql.DB) (map[string]int, error) {
	rows, err := sqlDB.QueryContext(ctx,
		`SELECT artist_key, COUNT(*) FROM scan_results WHERE artist_key != '' GROUP BY artist_key`)
	if err != nil {
		return nil, fmt.Errorf("query artist track counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("scan artist track count row: %w", err)
		}
		counts[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate artist track counts: %w", err)
	}
	return counts, nil
}

// sampleDoneRows draws up to n rows at random from scan_results whose status
// is 'done' -- the status the worker sets after a sidecar was successfully
// written (see internal/scan.StatusDone) -- and whose artist_key/title_key
// are populated. Filtering to 'done' is deliberate: this probe needs a
// trusted sidecar ALREADY on disk to score against, and a 'done' row is the
// library's own record that one was written, without this probe having to
// re-derive that by re-deriving SidecarName and re-checking every candidate
// extension.
func sampleDoneRows(ctx context.Context, sqlDB *sql.DB, n int, bands map[string]int) ([]probeBRow, error) {
	rows, err := sqlDB.QueryContext(ctx,
		`SELECT artist_key, artist, title, album, outdir, filename
           FROM scan_results
          WHERE status = 'done' AND artist_key != '' AND title_key != ''`)
	if err != nil {
		return nil, fmt.Errorf("query done rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var all []probeBRow
	for rows.Next() {
		var r probeBRow
		if err := rows.Scan(&r.artistKey, &r.track.ArtistName, &r.track.TrackName,
			&r.track.AlbumName, &r.outdir, &r.filename); err != nil {
			return nil, fmt.Errorf("scan done row: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate done rows: %w", err)
	}

	// The order sampled tracks are visited in has no bearing on correctness or
	// reproducibility here -- this is a one-shot manual probe, not a
	// reproducibility-critical test -- so math/rand's default source is fine.
	rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return quotaByBand(all, bands, n), nil
}

// quotaByBand takes an equal quota from each popularity band, redistributing any
// shortage to the bands that still have rows.
//
// A GLOBAL shuffle is biased for this measurement, which CodeRabbit caught on PR
// #772: rows are per-TRACK, so an artist with more completed tracks contributes
// proportionally more of them, and the top band -- which is defined by having
// more tracks -- crowds out the bottom. The bottom band is exactly the population
// this probe needs to speak about, since the whole question is whether isOfficial
// discriminates WITHIN a band rather than merely tracking popularity.
//
// A shortage is redistributed rather than left as a gap: if one band has fewer
// eligible rows than its quota, the remainder goes to the bands that can fill it,
// so a small band costs coverage only in that band. The report prints per-band
// counts either way, so a thin cell stays visible rather than being papered over
// by the redistribution.
func quotaByBand(all []probeBRow, bands map[string]int, n int) []probeBRow {
	if n <= 0 || len(all) == 0 {
		return nil
	}
	if len(all) <= n {
		return all
	}

	byBand := make([][]probeBRow, bandCount)
	for _, r := range all {
		b := bands[r.artistKey]
		if b < 0 || b >= bandCount {
			b = 0
		}
		byBand[b] = append(byBand[b], r)
	}

	// Round-robin across bands rather than computing quotas arithmetically. It
	// redistributes a shortage for free -- an exhausted band is simply skipped --
	// and avoids an off-by-one when n does not divide evenly by bandCount.
	out := make([]probeBRow, 0, n)
	idx := make([]int, bandCount)
	for len(out) < n {
		progressed := false
		for b := range byBand {
			if idx[b] >= len(byBand[b]) {
				continue
			}
			out = append(out, byBand[b][idx[b]])
			idx[b]++
			progressed = true
			if len(out) == n {
				break
			}
		}
		if !progressed {
			break // every band exhausted
		}
	}
	return out
}

// sidecarPath returns the on-disk path of the trusted sidecar for row, trying
// the .lrc spelling first and falling back to .txt -- the same pair
// oppositeSidecar in internal/lyrics treats as mutually exclusive on write.
// Returns "" when neither exists.
// It DISTINGUISHES "absent" from "unreadable", which both reviewers caught on
// PR #772 and which matters more here than it looks. Treating every os.Stat
// error as absence means a permission or I/O fault on the .lrc silently falls
// through to the .txt -- scoring a DIFFERENT file than intended -- or reports
// "no sidecar" for a file that is present but unreadable. Either way the run
// completes, the report looks healthy, and the number is quietly wrong. A
// broken sidecar environment must be visible as its own error class, not
// absorbed into the miss bucket.
//
// The second return is an error class for the observation: "" when the path is
// usable, "no-sidecar" when neither file exists, "sidecar-stat" when one exists
// but could not be examined.
func sidecarPath(row probeBRow) (string, string) {
	base := strings.TrimSuffix(row.filename, filepath.Ext(row.filename))
	if base == "" {
		return "", "no-sidecar"
	}
	lrcPath := filepath.Join(row.outdir, base+".lrc")
	switch _, err := os.Stat(lrcPath); {
	case err == nil:
		return lrcPath, ""
	case !errors.Is(err, os.ErrNotExist):
		// Present-but-unexaminable. Do NOT fall through to the .txt: that would
		// score a different artifact than the one the row actually settled with.
		return "", "sidecar-stat"
	}
	txtPath := filepath.Join(row.outdir, base+".txt")
	switch _, err := os.Stat(txtPath); {
	case err == nil:
		return txtPath, ""
	case !errors.Is(err, os.ErrNotExist):
		return "", "sidecar-stat"
	}
	return "", "no-sidecar"
}

// readTrustedText reads the sidecar at path and returns its plain-text
// content for scoring: an .lrc is stripped of timestamps, word markers, and
// decorative cues via the same lyrics.PlainBody a demotion would persist; a
// .txt is read as-is (an instrumental marker line simply fails to overlap
// with any real candidate text, so it needs no special casing here).
func readTrustedText(path string) (string, error) {
	if filepath.Ext(path) == ".lrc" {
		synced, err := lyrics.ReadSyncedLRC(path)
		if err != nil {
			return "", fmt.Errorf("probeb: read synced sidecar: %w", err)
		}
		return lyrics.PlainBody(synced), nil
	}
	// The open below is a variable path (gosec G304), which needs no
	// `//nolint`: gosec is excluded on `_test.go` files by .golangci.yml (see
	// readTrackList in probe_survey_test.go for the identical rationale), and
	// on the merits path is derived from outdir/filename read from this
	// probe's own operator-supplied database -- an offline manual tool with
	// no untrusted caller.
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("probeb: read plain sidecar: %w", err)
	}
	return string(b), nil
}

// candidateText extracts plain text from a decoded petitlyrics payload for
// scoring, or ("", false) when the tier carries no usable text (line-sync:
// per decode.go, its text region does not decode cleanly and is not
// attempted here).
func candidateText(raw []byte) (string, bool) {
	switch classifyPayload(raw) {
	case tierUnsynced:
		return decodeUnsynced(raw), true
	case tierWordSync:
		cues, _, err := decodeWordSync(raw)
		if err != nil {
			return "", false
		}
		lines := make([]string, 0, len(cues))
		for _, l := range cues {
			lines = append(lines, l.Text)
		}
		return strings.Join(lines, "\n"), true
	default: // tierLineSync
		return "", false
	}
}

// probeBSample performs one lookup and scores it against the trusted sidecar.
// The second return is non-nil for an error class that must abort the whole
// run, mirroring surveySample's contract exactly.
func probeBSample(ctx context.Context, c *Client, row probeBRow, bands map[string]int) (probeBObservation, error) {
	band := bands[row.artistKey]

	path, pathErr := sidecarPath(row)
	if pathErr != "" {
		// No usable sidecar despite status='done'. Either it was moved or
		// removed since the scan (no-sidecar), or it exists and could not be
		// examined (sidecar-stat) -- kept distinct so a broken filesystem is
		// not silently counted as missing data. Either way there is no ground
		// truth to score against, so spend no network call on it.
		return probeBObservation{Err: pathErr}, nil
	}
	trusted, err := readTrustedText(path)
	if err != nil {
		return probeBObservation{Err: "sidecar-read"}, nil
	}
	if strings.TrimSpace(trusted) == "" {
		return probeBObservation{Err: "empty-sidecar"}, nil
	}

	songs, err := c.request(ctx, row.track, tierWordSync)
	if err != nil {
		switch {
		// MUST precede ErrNotFound: ErrProviderUnavailable wraps it (see
		// surveySample's identical ordering note).
		case errors.Is(err, ErrProviderUnavailable):
			return probeBObservation{Err: "ErrProviderUnavailable"}, fmt.Errorf(
				"sustained zero-result run (application id revoked?): %w", err)
		case errors.Is(err, ErrUnauthorized):
			return probeBObservation{Err: "ErrUnauthorized"}, fmt.Errorf("credential rejected: %w", err)
		case errors.Is(err, ErrForbidden):
			return probeBObservation{Err: "ErrForbidden"}, fmt.Errorf("request refused: %w", err)
		case errors.Is(err, ErrRateLimited):
			return probeBObservation{Err: "ErrRateLimited"}, fmt.Errorf("throttled: %w", err)
		case errors.Is(err, ErrNotFound):
			return probeBObservation{Band: band, Err: "ErrNotFound"}, nil
		default:
			return probeBObservation{Err: "transport"}, nil
		}
	}

	candidate, err := selectCandidate(songs, row.track)
	if err != nil {
		return probeBObservation{Band: band, Err: "no-candidate"}, nil
	}
	if candidate.LyricsData == "" {
		return probeBObservation{Band: band, Err: "empty-payload"}, nil
	}

	raw, err := base64.StdEncoding.DecodeString(candidate.LyricsData)
	if err != nil {
		return probeBObservation{Band: band, Err: "base64"}, nil
	}

	text, ok := candidateText(raw)
	if !ok {
		// Line-sync tier carries no usable text to score. This is not an
		// error in the ordinary sense -- it is a real result, "petitlyrics'
		// best tier here cannot be compared" -- but it cannot be scored, so
		// it is counted separately rather than silently dropped.
		return probeBObservation{Band: band, Err: "tier2-no-text"}, nil
	}

	score := scoreAgreement(trusted, text)
	return probeBObservation{
		Band:       band,
		IsOfficial: candidate.IsOfficial,
		Score:      score,
		Scored:     true,
	}, nil
}

// TestSampleDoneRowsUsesBandQuota guards the CALL SITE, not just the helper.
//
// TestQuotaByBand exercises quotaByBand directly, so it stays green even if
// sampleDoneRows stops calling it -- verified by mutation: reverting the call
// site to the old global-shuffle truncation passed the whole suite. That is the
// same shape of gap that shipped a defect on #770 (the seam was tested, the
// wiring was not), so it gets its own test here.
//
// It runs against real SQLite rather than a fake, matching the repo's
// integration-test convention.
func TestSampleDoneRowsUsesBandQuota(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "probeb.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	// scan_results.library_id carries a foreign key, so the parent row has to
	// exist before any child insert.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path) VALUES (1, 'test', '/m')`); err != nil {
		t.Fatalf("insert library: %v", err)
	}

	// A lopsided library: the top-band artist has many more completed tracks,
	// which is precisely what biases a global shuffle.
	insert := func(artistKey string, n int) {
		t.Helper()
		for i := range n {
			if _, err := sqlDB.ExecContext(ctx,
				`INSERT INTO scan_results (library_id, file_path, artist, title, artist_key, title_key, status, outdir, filename)
				 VALUES (1, ?, ?, ?, ?, ?, 'done', '/out', ?)`,
				fmt.Sprintf("/m/%s-%d.flac", artistKey, i), artistKey, fmt.Sprintf("t%d", i),
				artistKey, fmt.Sprintf("t%d", i), fmt.Sprintf("%s-%d.lrc", artistKey, i),
			); err != nil {
				t.Fatalf("insert %s row %d: %v", artistKey, i, err)
			}
		}
	}
	insert("bottom", 4)
	insert("top", 60)

	bands := map[string]int{"bottom": 0, "top": 2}

	rows, err := sampleDoneRows(ctx, sqlDB, 8, bands)
	if err != nil {
		t.Fatalf("sampleDoneRows: %v", err)
	}
	if len(rows) != 8 {
		t.Fatalf("sampled %d rows, want 8", len(rows))
	}

	counts := map[int]int{}
	for _, r := range rows {
		counts[bands[r.artistKey]]++
	}
	// Under a global shuffle the bottom band (4 of 64 rows, ~6%) would contribute
	// roughly 0-1 of 8. Under the quota it contributes all 4 it has.
	if counts[0] != 4 {
		t.Errorf("bottom band got %d rows, want all 4 it had -- sampleDoneRows is not "+
			"applying the band quota; counts=%v", counts[0], counts)
	}
}
