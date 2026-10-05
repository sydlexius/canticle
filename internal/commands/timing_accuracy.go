package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/innertube"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/timing"
	"github.com/sydlexius/canticle/internal/timingacc"
)

// TimingAccCmd measures per-lane line-start error against a hand-verified
// reference set (#1117). It fetches and compares; it writes nothing anywhere.
type TimingAccCmd struct {
	RefDir     string `arg:"positional,required" help:"local reference directory holding manifest.toml and the reference .lrc/.elrc files (keep it outside any git tree)"`
	Lanes      string `arg:"--lanes" help:"comma-separated lanes to measure (default: every known provider)" default:""`
	Token      string `arg:"--token" help:"optional Musixmatch API token override, visible in shell history and process listings; prefer MUSIXMATCH_TOKEN / MXLRC_API_TOKEN or the config file (the encrypted store is not consulted)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// refTrack is one manifest entry. LineResidualMS is a pointer so a missing
// value is a load error rather than a silent zero.
type refTrack struct {
	ID             string `toml:"id"`
	File           string `toml:"file"`
	Artist         string `toml:"artist"`
	Title          string `toml:"title"`
	Album          string `toml:"album"`
	DurationSecs   int    `toml:"duration_seconds"`
	LineResidualMS *int   `toml:"line_residual_ms"`
	// WordResidualMS is parsed for the manifest format but unused until the
	// word-start slice.
	WordResidualMS *int `toml:"word_residual_ms"`
}

type refManifest struct {
	Track []refTrack `toml:"track"`
}

var errNotRegular = errors.New("not a readable regular file")

// readRegular reads name through root, refusing a non-regular file (symlink,
// FIFO, device, directory) before opening it.
func readRegular(root *os.Root, name string) ([]byte, error) {
	fi, err := root.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return readRegularHandle(root, name, fi)
}

// readRegularHandle reads name from one handle, opened non-blocking so a FIFO
// swapped in after the Lstat cannot hang the run. The handle itself must be a
// regular file and the very file the Lstat saw (fi), which also refuses a
// swapped-in symlink: os.Root follows one that stays inside the root.
func readRegularHandle(root *os.Root, name string, fi os.FileInfo) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errNotRegular
	}
	defer func() { _ = f.Close() }()
	hfi, err := f.Stat()
	if err != nil || !hfi.Mode().IsRegular() || !os.SameFile(fi, hfi) {
		return nil, errNotRegular
	}
	return io.ReadAll(f)
}

// loadRefSet reads the manifest and every reference file through one os.Root.
// Errors never carry a path, id, artist or title (they reach stdout).
func loadRefSet(dir string) ([]refTrack, [][]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, errors.New("reference directory could not be opened")
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat(".git"); err == nil {
		return nil, nil, errors.New("reference directory holds a .git entry; keep the reference set outside any git checkout")
	}
	raw, err := readRegular(root, "manifest.toml")
	if err != nil {
		return nil, nil, errors.New("manifest.toml could not be read or parsed")
	}
	var m refManifest
	if _, err = toml.Decode(string(raw), &m); err != nil {
		return nil, nil, errors.New("manifest.toml could not be read or parsed")
	}
	if len(m.Track) == 0 {
		return nil, nil, errors.New("manifest.toml lists no [[track]] entries")
	}
	seen := make(map[string]bool, len(m.Track))
	for i, t := range m.Track {
		n := i + 1
		switch {
		case t.ID == "":
			return nil, nil, fmt.Errorf("manifest track #%d: id is required", n)
		case seen[t.ID]:
			return nil, nil, fmt.Errorf("manifest track #%d: duplicate id", n)
		case t.File == "" || !filepath.IsLocal(t.File):
			return nil, nil, fmt.Errorf("manifest track #%d: file must be a relative path inside the reference directory", n)
		case t.LineResidualMS == nil:
			return nil, nil, fmt.Errorf("manifest track #%d: line_residual_ms is required", n)
		case *t.LineResidualMS < 0:
			return nil, nil, fmt.Errorf("manifest track #%d: line_residual_ms must not be negative", n)
		}
		seen[t.ID] = true
	}
	bodies := make([][]byte, len(m.Track))
	for i, t := range m.Track {
		if bodies[i], err = readRegular(root, t.File); err != nil {
			return nil, nil, errors.New("a reference file could not be read")
		}
	}
	return m.Track, bodies, nil
}

// refCues parses a reference body, dropping decorative cues (and counting them).
func refCues(body []byte) ([]timingacc.Cue, int) {
	var cues []timingacc.Cue
	dropped := 0
	for _, l := range lyrics.ParseTimedLRC(string(body)).Lines {
		if l.Decorative {
			dropped++
			continue
		}
		c := timingacc.Cue{StartMS: l.StartMS, Text: l.Text}
		for _, w := range l.Words {
			c.Words = append(c.Words, timingacc.Word{StartMS: w.StartMS, Text: w.Text})
		}
		cues = append(cues, c)
	}
	return cues, dropped
}

// songCues converts a served song's line cues, dropping decorative ones.
func songCues(song models.Song) ([]timingacc.Cue, int) {
	var cues []timingacc.Cue
	dropped := 0
	for _, l := range song.Subtitles.Lines {
		if timing.IsDecorative(l.Text) {
			dropped++
			continue
		}
		ms := l.Time.Minutes*60000 + l.Time.Seconds*1000 + l.Time.Hundredths*10
		if l.Time.Total > 0 {
			ms = int(math.Round(l.Time.Total * 1000))
		}
		cues = append(cues, timingacc.Cue{StartMS: ms, Text: strings.TrimSpace(l.Text)})
	}
	return cues, dropped
}

// accuracyConfig copies cfg with api.cooldown raised to >= 1s so no lane is unpaced.
func accuracyConfig(cfg config.Config) config.Config {
	if cfg.API.Cooldown < 1 {
		cfg.API.Cooldown = 1
	}
	return cfg
}

// accuracyLanes builds the requested known, enabled lanes; a disabled lane or a
// tokenless Musixmatch is skipped and reported. cfg is not mutated.
func accuracyLanes(cfg config.Config, token string, names []string, newFetcher func(string) musixmatch.Fetcher) (lanes []providers.LyricsProvider, skipped []string) {
	paced := accuracyConfig(cfg)
	for _, name := range names {
		n := providers.NormalizeName(name)
		switch {
		case providerDisabledIn(n, cfg.Providers.Disabled):
			skipped = append(skipped, n+"(disabled)")
			continue
		case n == providers.Musixmatch && strings.TrimSpace(token) == "":
			skipped = append(skipped, n+"(no token)")
			continue
		}
		if p := buildProvider(n, paced, token, newFetcher); p != nil {
			lanes = append(lanes, p)
		}
	}
	return lanes, skipped
}

// errClass is the only thing logged about a lane error: a raw error string can
// embed a query URL carrying the reference track's identity. Both petitlyrics
// outage sentinels wrap ErrNotFound, so they are tested before the miss case.
func errClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	case errors.Is(err, petitlyrics.ErrProviderUnavailable), errors.Is(err, petitlyrics.ErrOutageLatched):
		return "unavailable"
	case errors.Is(err, musixmatch.ErrNotFound), errors.Is(err, petitlyrics.ErrNotFound), errors.Is(err, innertube.ErrNotFound):
		return "not_found"
	case errors.Is(err, musixmatch.ErrRateLimited), errors.Is(err, petitlyrics.ErrRateLimited), errors.Is(err, innertube.ErrRateLimited):
		return "rate_limited"
	default:
		return "error"
	}
}

// parseLaneNames normalizes and de-duplicates --lanes, skipping empty
// elements; ok is false when a name is not a known provider.
func parseLaneNames(raw string) (names []string, ok bool) {
	seen := map[string]bool{}
	for _, n := range strings.Split(raw, ",") {
		n = providers.NormalizeName(n)
		switch {
		case n == "" || seen[n]:
			continue
		case !providers.IsKnown(n):
			return nil, false
		}
		seen[n] = true
		names = append(names, n)
	}
	return names, true
}

func runTimingAccuracyCmd(ctx context.Context, out io.Writer, args TimingAccCmd, newFetcher func(string) musixmatch.Fetcher) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	token := args.Token
	if token == "" {
		token = cfg.API.Token
	}
	names := providers.Known()
	if strings.TrimSpace(args.Lanes) != "" {
		var ok bool
		if names, ok = parseLaneNames(args.Lanes); !ok {
			_, _ = fmt.Fprintln(out, "timing-accuracy: --lanes names an unknown provider")
			return 2
		}
	}
	lanes, skipped := accuracyLanes(cfg, token, names, newFetcher)
	return runTimingAccuracy(ctx, out, args, lanes, skipped)
}

type served struct {
	upstream string
	cues     []timingacc.Cue
	ref      []timingacc.Cue
	residual int
}

// laneTally counts what a lane did NOT serve, split by cause.
type laneTally struct {
	notFound, failed, notAsked int
	stopped                    string // why the lane was not asked again; empty if it was
}

// runTimingAccuracy is the testable core: lane-outer, track-inner, sequential.
// A throttled or unavailable lane is not asked again. No writes, no database, aggregate stdout.
func runTimingAccuracy(ctx context.Context, out io.Writer, args TimingAccCmd, lanes []providers.LyricsProvider, skipped []string) int {
	tracks, bodies, err := loadRefSet(args.RefDir)
	if err != nil {
		_, _ = fmt.Fprintf(out, "timing-accuracy: %v\n", err)
		return 1
	}
	type loaded struct {
		cues []timingacc.Cue
		meta refTrack
	}
	refs := make([]loaded, 0, len(tracks))
	dropped := 0
	for i, t := range tracks {
		cues, d := refCues(bodies[i])
		dropped += d
		refs = append(refs, loaded{cues, t})
	}

	rows := map[string]*timingacc.LineStats{}
	tally := map[string]*laneTally{}
	var order []string
	add := func(key string) *timingacc.LineStats {
		if rows[key] == nil {
			rows[key] = &timingacc.LineStats{}
			order = append(order, key)
		}
		return rows[key]
	}
	interrupted := false
laneLoop:
	for _, lane := range lanes {
		name := lane.Name()
		add(name)
		tl := &laneTally{}
		tally[name] = tl
		var results []served
		for i, r := range refs {
			if ctx.Err() != nil {
				interrupted = true
				break laneLoop
			}
			song, err := lane.FindLyrics(ctx, models.Track{
				ArtistName:  r.meta.Artist,
				TrackName:   r.meta.Title,
				AlbumName:   r.meta.Album,
				TrackLength: r.meta.DurationSecs,
			})
			if err != nil {
				class := errClass(err)
				slog.Debug("timing-accuracy lane error", "lane", name, "class", class)
				switch class {
				case "context":
					interrupted = true
					break laneLoop
				case "not_found":
					tl.notFound++
				default:
					tl.failed++
				}
				if class == "rate_limited" || class == "unavailable" {
					tl.stopped = strings.Replace(class, "rate_limited", "throttled", 1)
					tl.notAsked = len(refs) - i - 1
					break
				}
				continue
			}
			pc, d := songCues(song)
			dropped += d
			if len(pc) == 0 {
				tl.notFound++
				continue
			}
			results = append(results, served{song.Upstream, pc, r.cues, *r.meta.LineResidualMS})
			rows[name].AddTrack(r.cues, pc, *r.meta.LineResidualMS)
		}
		// Per-upstream rows only for a lane that names any upstream; a served
		// result that names none sorts under an explicit marker.
		named := false
		for _, s := range results {
			named = named || s.upstream != ""
		}
		if !named {
			continue
		}
		for _, s := range results {
			up := s.upstream
			if up == "" {
				up = "(upstream not named)"
			}
			add(name+"/"+up).AddTrack(s.ref, s.cues, s.residual)
		}
	}
	if interrupted {
		_, _ = fmt.Fprintln(out, "timing-accuracy: interrupted; results below are partial")
	}
	printAccuracy(out, len(refs), dropped, skipped, order, rows, tally, len(lanes))
	if interrupted {
		return 1
	}
	return 0
}

// printAccuracy writes the aggregate report: only lane/upstream tokens and
// numbers, never a track identity or lyric text.
func printAccuracy(out io.Writer, nRefs, dropped int, skipped, order []string, rows map[string]*timingacc.LineStats, tally map[string]*laneTally, nLanes int) {
	_, _ = fmt.Fprintf(out, "timing-accuracy: %d reference tracks, line starts, tolerance %dms\n", nRefs, timingacc.WithinMS)
	_, _ = fmt.Fprintf(out, "decorative cues dropped (both sides): %d\n", dropped)
	if len(skipped) > 0 {
		_, _ = fmt.Fprintf(out, "skipped lanes: %s\n", strings.Join(skipped, ","))
	}
	if nLanes == 0 {
		_, _ = fmt.Fprintln(out, "no lane was measured")
	}
	sort.Strings(order)
	for _, key := range order {
		_, _ = fmt.Fprintln(out, timingacc.FormatRow(key, *rows[key]))
		tl := tally[key]
		if tl == nil {
			continue
		}
		if tl.notFound > 0 || tl.failed > 0 {
			_, _ = fmt.Fprintf(out, "%s: not_found=%d failed=%d\n", key, tl.notFound, tl.failed)
		}
		if tl.stopped != "" {
			_, _ = fmt.Fprintf(out, "%s: stopped early (%s); remaining tracks not asked: %d\n", key, tl.stopped, tl.notAsked)
		}
	}
}
