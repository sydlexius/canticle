// Command vocalcalib calibrates the sung-vocal-peak threshold used by the
// instrumental detector's three-gate decision (internal/detector.Instrumental).
// It has three modes:
//
//   - sample:   draw a randomized set of known-labeled tracks from a (seeded
//     copy of a) work_queue database, run the live detector sidecar against
//     each, and emit internal/vocalcalib.LabeledScore JSONL.
//   - sweep:    read a LabeledScore JSONL file and sweep the vocal-peak
//     threshold to find the highest value that keeps the positive-error-rate
//     at or below a target ceiling (internal/vocalcalib.SelectThreshold).
//   - validate: read a LabeledScore JSONL file and check a fixed threshold
//     against a positive-error-rate ceiling (internal/vocalcalib.Validate).
//
// It is a developer-only operational tool (used to recalibrate the vocal-peak
// gate, see #384/#510) and is intentionally excluded from releases
// (GoReleaser builds only ./cmd/mxlrcgo-svc).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/ffmpeg"
	"github.com/sydlexius/canticle/internal/vocalcalib"
)

func main() {
	mode := flag.String("mode", "", "operation mode: sample|sweep|validate")

	// sample mode flags.
	dbPath := flag.String("db", "", "sample: path to a (seeded copy of a) work_queue SQLite database")
	configPath := flag.String("config", "", "sample: path to config.toml (defaults to XDG resolution)")
	label := flag.String("label", "", "sample: label to draw, \"vocal\" or \"instrumental\"")
	n := flag.Int("n", 100, "sample: number of tracks to draw")
	seed := flag.Int64("seed", 1, "sample: PRNG seed for reproducible draws")
	out := flag.String("out", "", "sample: output JSONL path (defaults to stdout)")

	// sweep mode flags.
	in := flag.String("in", "", "sweep|validate: input LabeledScore JSONL path")
	minConfidence := flag.Float64("min-confidence", 0, "sweep|validate: music-gate MinConfidence")
	speechMax := flag.Float64("speech-max", 0, "sweep|validate: speech-gate SpeechMax")
	maxErr := flag.Float64("max-err", 0.01, "sweep|validate: maximum acceptable positive-error-rate")
	bottomK := flag.Int("bottom-k", 10, "sweep: number of lowest-vocal-peak positives to print for outlier inspection")

	// validate mode flags.
	threshold := flag.Float64("threshold", 0, "validate: fixed vocal-peak threshold to check")

	flag.Parse()

	var err error
	switch *mode {
	case "sample":
		err = runSample(sampleArgs{
			dbPath:     *dbPath,
			configPath: *configPath,
			label:      *label,
			n:          *n,
			seed:       *seed,
			out:        *out,
		})
	case "sweep":
		err = runSweep(sweepArgs{
			in:            *in,
			minConfidence: *minConfidence,
			speechMax:     *speechMax,
			maxErr:        *maxErr,
			bottomK:       *bottomK,
		})
	case "validate":
		err = runValidate(validateArgs{
			in:            *in,
			minConfidence: *minConfidence,
			speechMax:     *speechMax,
			threshold:     *threshold,
			maxErr:        *maxErr,
		})
	default:
		fmt.Fprintln(os.Stderr, "vocalcalib: -mode must be one of sample, sweep, validate")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vocalcalib: %v\n", err)
		os.Exit(1)
	}
}

// sampleArgs holds the parsed flags for -mode sample.
type sampleArgs struct {
	dbPath     string
	configPath string
	label      string
	n          int
	seed       int64
	out        string
}

// runSample draws a randomized set of labeled rows from the work_queue table,
// runs the live detector sidecar against each reachable audio file, and emits
// one vocalcalib.LabeledScore JSONL line per successfully classified track.
func runSample(a sampleArgs) error {
	if strings.TrimSpace(a.dbPath) == "" {
		return fmt.Errorf("sample: -db is required")
	}
	if a.label != "vocal" && a.label != "instrumental" {
		return fmt.Errorf("sample: -label must be \"vocal\" or \"instrumental\"")
	}
	if a.n <= 0 {
		return fmt.Errorf("sample: -n must be > 0")
	}

	ctx := context.Background()

	cfg, err := config.Load(a.configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if strings.TrimSpace(cfg.InstrumentalDetector.ClassifierURL) == "" {
		return fmt.Errorf("sample: instrumental detector is not configured (set instrumental_detector.classifier_url)")
	}
	ffmpegPath, err := resolveFFmpeg(ctx, cfg)
	if err != nil {
		return fmt.Errorf("resolve ffmpeg: %w", err)
	}
	det, err := newAudioDetector(cfg, ffmpegPath)
	if err != nil {
		return fmt.Errorf("construct audio detector: %w", err)
	}

	sqlDB, err := db.OpenReadOnly(ctx, a.dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = sqlDB.Close() }() //nolint:errcheck // reason: best-effort close on exit

	paths, err := candidatePaths(ctx, sqlDB, a.label)
	if err != nil {
		return fmt.Errorf("query candidates: %w", err)
	}

	rng := rand.New(rand.NewSource(a.seed)) //nolint:gosec // reason: reproducible sample draw, not a security-sensitive PRNG use
	rng.Shuffle(len(paths), func(i, j int) { paths[i], paths[j] = paths[j], paths[i] })

	w := os.Stdout
	if strings.TrimSpace(a.out) != "" {
		f, ferr := os.Create(a.out) //nolint:gosec // reason: operator-supplied output path for a dev-only tool
		if ferr != nil {
			return fmt.Errorf("create output: %w", ferr)
		}
		defer func() { _ = f.Close() }() //nolint:errcheck // reason: best-effort close on exit
		w = f
	}

	written := 0
	for _, p := range paths {
		if written >= a.n {
			break
		}
		if _, statErr := os.Stat(p); statErr != nil {
			continue
		}
		res, detErr := det.Detect(ctx, p)
		if detErr != nil {
			fmt.Fprintf(os.Stderr, "vocalcalib: sample: detect %s: %v\n", p, detErr)
			continue
		}
		score := vocalcalib.LabeledScore{
			MusicSum:        res.Confidence,
			VocalPeak:       res.VocalConfidence,
			SpeechMean:      res.SpeechConfidence,
			VocalClass:      res.WinningVocalClass,
			DetectorVersion: res.Version,
			Label:           a.label,
		}
		if writeErr := vocalcalib.WriteJSONL(w, score); writeErr != nil {
			return fmt.Errorf("write score: %w", writeErr)
		}
		written++
	}
	fmt.Fprintf(os.Stderr, "vocalcalib: sample: wrote %d/%d requested scores (label=%s, candidates=%d)\n", written, a.n, a.label, len(paths))
	return nil
}

// candidatePaths returns the source_path of every row matching label, in
// whatever order SQLite returns them; the caller shuffles in Go with an
// explicit seed for reproducible draws.
func candidatePaths(ctx context.Context, sqlDB *sql.DB, label string) ([]string, error) {
	var query string
	switch label {
	case "vocal":
		query = `SELECT source_path FROM work_queue WHERE outcome_type IN ('synced','unsynced') AND TRIM(COALESCE(source_path,'')) <> ''`
	case "instrumental":
		query = `SELECT source_path FROM work_queue WHERE outcome_type='instrumental' AND instrumental_result IS NULL AND TRIM(COALESCE(source_path,'')) <> ''`
	default:
		return nil, fmt.Errorf("candidatePaths: unknown label %q", label)
	}
	rows, err := sqlDB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // reason: best-effort close on exit

	var paths []string
	for rows.Next() {
		var p string
		if scanErr := rows.Scan(&p); scanErr != nil {
			return nil, scanErr
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// resolveFFmpeg mirrors internal/commands.resolveFFmpeg: the verification
// override takes precedence, then the detector's, resolved to an absolute
// executable path with the auto-provisioning cache beside the database.
func resolveFFmpeg(ctx context.Context, cfg config.Config) (string, error) {
	override := strings.TrimSpace(cfg.Verification.FFmpegPath)
	if override == "" {
		override = strings.TrimSpace(cfg.InstrumentalDetector.FFmpegPath)
	}
	cacheDir := filepath.Join(filepath.Dir(cfg.DB.Path), "ffmpeg")
	return ffmpeg.Resolve(ctx, override, ffmpeg.Options{CacheDir: cacheDir})
}

// newAudioDetector mirrors internal/commands.newAudioDetector's config
// mapping (commands.go:1506-1525) so this tool exercises the exact same
// detector construction serve mode uses.
func newAudioDetector(cfg config.Config, ffmpegPath string) (detector.Detector, error) {
	return detector.NewHTTPDetector(detector.Config{
		ClassifierURL:         cfg.InstrumentalDetector.ClassifierURL,
		SampleDurationSeconds: cfg.InstrumentalDetector.SampleDurationSeconds,
		MinConfidence:         cfg.InstrumentalDetector.MinConfidence,
		InstrumentalClasses:   cfg.InstrumentalDetector.InstrumentalClasses,
		VocalClasses:          cfg.InstrumentalDetector.VocalClasses,
		VocalMaxConfidence:    cfg.InstrumentalDetector.VocalMaxConfidence,
		SpeechClasses:         cfg.InstrumentalDetector.SpeechClasses,
		SpeechMaxConfidence:   cfg.InstrumentalDetector.SpeechMaxConfidence,
		SpreadSamples:         cfg.InstrumentalDetector.SpreadSamples,
		FFmpegPath:            ffmpegPath,
		FFprobePath:           cfg.InstrumentalDetector.FFprobePath,
		CooldownSeconds:       cfg.InstrumentalDetector.CooldownSeconds,
	})
}

// sweepArgs holds the parsed flags for -mode sweep.
type sweepArgs struct {
	in            string
	minConfidence float64
	speechMax     float64
	maxErr        float64
	bottomK       int
}

// runSweep reads a LabeledScore JSONL file, sweeps the vocal-peak threshold,
// and prints the chosen operating point plus the bottom-K lowest-vocal-peak
// positives for outlier inspection.
func runSweep(a sweepArgs) error {
	if strings.TrimSpace(a.in) == "" {
		return fmt.Errorf("sweep: -in is required")
	}
	scores, err := readScores(a.in)
	if err != nil {
		return err
	}

	sel, err := vocalcalib.SelectThreshold(scores, vocalcalib.Gates{MinConfidence: a.minConfidence, SpeechMax: a.speechMax}, a.maxErr)
	if err != nil {
		return fmt.Errorf("select threshold: %w", err)
	}

	fmt.Printf("threshold=%.6f pos_err_rate=%.4f neg_recovery=%.4f pos_n=%d neg_n=%d curve_points=%d\n",
		sel.Threshold, sel.PosErrRate, sel.NegRecovery, sel.PosN, sel.NegN, len(sel.Curve))

	var pos []vocalcalib.LabeledScore
	for _, s := range scores {
		if s.Label == "vocal" {
			pos = append(pos, s)
		}
	}
	sort.Slice(pos, func(i, j int) bool { return pos[i].VocalPeak < pos[j].VocalPeak })
	k := a.bottomK
	if k > len(pos) {
		k = len(pos)
	}
	fmt.Printf("bottom-%d positives by vocal_peak:\n", k)
	for i := 0; i < k; i++ {
		s := pos[i]
		fmt.Printf("  vocal_peak=%.6f music_sum=%.6f speech_mean=%.6f vocal_class=%s\n", s.VocalPeak, s.MusicSum, s.SpeechMean, s.VocalClass)
	}
	return nil
}

// validateArgs holds the parsed flags for -mode validate.
type validateArgs struct {
	in            string
	minConfidence float64
	speechMax     float64
	threshold     float64
	maxErr        float64
}

// runValidate reads a LabeledScore JSONL file, checks the fixed threshold
// against the positive-error-rate ceiling, and prints the report. It returns
// an error (non-zero exit) when the report does not pass.
func runValidate(a validateArgs) error {
	if strings.TrimSpace(a.in) == "" {
		return fmt.Errorf("validate: -in is required")
	}
	scores, err := readScores(a.in)
	if err != nil {
		return err
	}

	r := vocalcalib.Validate(scores, vocalcalib.Gates{MinConfidence: a.minConfidence, SpeechMax: a.speechMax}, a.threshold, a.maxErr)
	fmt.Printf("threshold=%.6f pos_n=%d pos_err_n=%d pos_err_rate=%.4f neg_n=%d neg_recovered_n=%d neg_recovery=%.4f pass=%t\n",
		r.Threshold, r.PosN, r.PosErrN, r.PosErrRate, r.NegN, r.NegRecoveredN, r.NegRecovery, r.Pass)
	if !r.Pass {
		return fmt.Errorf("validate: FAIL (pos_err_rate=%.4f, ceiling=%.4f)", r.PosErrRate, a.maxErr)
	}
	return nil
}

// readScores opens path and reads it as LabeledScore JSONL.
func readScores(path string) ([]vocalcalib.LabeledScore, error) {
	f, err := os.Open(path) //nolint:gosec // reason: operator-supplied input path for a dev-only tool
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // reason: best-effort close on exit

	scores, err := vocalcalib.ReadJSONL(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return scores, nil
}
