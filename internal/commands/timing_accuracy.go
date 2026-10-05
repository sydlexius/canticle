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
	"regexp"
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
	// WordResidualMS is required when the reference file carries word timings.
	WordResidualMS *int `toml:"word_residual_ms"`
}

type refManifest struct {
	Track []refTrack `toml:"track"`
}

// manifestKeyRe is the only shape of unknown key the loader will print.
var manifestKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// maxResidualMS caps a declared residual; anything larger is a unit mistake.
const maxResidualMS = 60000

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
	md, err := toml.Decode(string(raw), &m)
	if err != nil {
		return nil, nil, errors.New("manifest.toml could not be read or parsed")
	}
	if un := md.Undecoded(); len(un) > 0 {
		// A key is operator-authored text: echo it only when it is plainly a
		// schema-shaped token, never a quoted key that could carry a title.
		if k := un[0].String(); manifestKeyRe.MatchString(k) {
			return nil, nil, fmt.Errorf("manifest.toml has an unknown key: %s", k)
		}
		return nil, nil, errors.New("manifest.toml has an unknown key")
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
		case *t.LineResidualMS > maxResidualMS:
			return nil, nil, fmt.Errorf("manifest track #%d: line_residual_ms must not exceed %d", n, maxResidualMS)
		case t.WordResidualMS != nil && *t.WordResidualMS > maxResidualMS:
			return nil, nil, fmt.Errorf("manifest track #%d: word_residual_ms must not exceed %d", n, maxResidualMS)
		case t.WordResidualMS != nil && *t.WordResidualMS < 0:
			return nil, nil, fmt.Errorf("manifest track #%d: word_residual_ms must not be negative", n)
		case strings.TrimSpace(t.Title) == "":
			return nil, nil, fmt.Errorf("manifest track #%d: title is required", n)
		case t.DurationSecs < 0:
			return nil, nil, fmt.Errorf("manifest track #%d: duration_seconds must not be negative", n)
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

// refCues parses a reference body, dropping decorative cues (and counting
// them). A line with stacked timestamps expands to several cues that all carry
// the same absolute word stamps, which can belong to at most one of them, so
// words shared by more than one expanded cue are dropped from all of them
// (those lines are measured for line starts only); stacked counts the cues
// that lost their words this way.
func refCues(body []byte) (cues []timingacc.Cue, dropped, stacked int) {
	sig := map[string]int{}
	var sigs []string
	for _, l := range lyrics.ParseTimedLRC(string(body)).Lines {
		if l.Decorative {
			dropped++
			continue
		}
		c := timingacc.Cue{StartMS: l.StartMS, Text: l.Text}
		var sb strings.Builder
		for _, w := range l.Words {
			c.Words = append(c.Words, timingacc.Word{StartMS: w.StartMS, Text: w.Text})
			fmt.Fprintf(&sb, "%d:%s|", w.StartMS, w.Text)
		}
		sigs = append(sigs, sb.String())
		if len(c.Words) > 0 {
			sig[sigs[len(sigs)-1]]++
		}
		cues = append(cues, c)
	}
	for i := range cues {
		if len(cues[i].Words) > 0 && sig[sigs[i]] > 1 {
			cues[i].Words = nil
			stacked++
		}
	}
	return cues, dropped, stacked
}

// songCues converts a served song's line cues, dropping decorative ones.
func songCues(song models.Song) ([]timingacc.Cue, int) {
	var cues []timingacc.Cue
	dropped := 0
	kept := make(map[int]int, len(song.Subtitles.Lines)) // source line index -> cue index
	for li, l := range song.Subtitles.Lines {
		if timing.IsDecorative(l.Text) {
			dropped++
			continue
		}
		ms := l.Time.Minutes*60000 + l.Time.Seconds*1000 + l.Time.Hundredths*10
		if l.Time.Total > 0 {
			ms = int(math.Round(l.Time.Total * 1000))
		}
		kept[li] = len(cues)
		cues = append(cues, timingacc.Cue{StartMS: ms, Text: strings.TrimSpace(l.Text)})
	}
	for _, w := range song.WordTimings {
		if ci, ok := kept[w.Line]; ok {
			cues[ci].Words = append(cues[ci].Words, timingacc.Word{StartMS: w.StartMS, Text: strings.TrimSpace(w.Text)})
		}
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
	upstream     string
	cues         []timingacc.Cue
	ref          []timingacc.Cue
	residual     int
	wordResidual int
}

// acc is one report group's line and word statistics.
type acc struct {
	line timingacc.LineStats
	word timingacc.WordStats
}

func (a *acc) add(ref, prov []timingacc.Cue, lineRes, wordRes int) {
	a.line.AddTrack(ref, prov, lineRes)
	a.word.AddTrack(ref, prov, wordRes)
}

// hasWords reports whether any cue carries word timings.
func hasWords(cues []timingacc.Cue) bool {
	for _, c := range cues {
		if len(c.Words) > 0 {
			return true
		}
	}
	return false
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
	var dropRef, dropProv, dropStacked int
	anyRefWords := false
	for i, t := range tracks {
		cues, d, st := refCues(bodies[i])
		dropRef += d
		dropStacked += st
		if hasWords(cues) {
			anyRefWords = true
			if t.WordResidualMS == nil {
				_, _ = fmt.Fprintf(out, "timing-accuracy: manifest track #%d: word_residual_ms is required when the reference carries word timings\n", i+1)
				return 1
			}
		}
		refs = append(refs, loaded{cues, t})
	}

	rows := map[string]*acc{}
	tally := map[string]*laneTally{}
	var order []string
	add := func(key string) *acc {
		if rows[key] == nil {
			rows[key] = &acc{}
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
			dropProv += d
			if len(pc) == 0 {
				tl.notFound++
				continue
			}
			wr := 0
			if r.meta.WordResidualMS != nil {
				wr = *r.meta.WordResidualMS
			}
			results = append(results, served{song.Upstream, pc, r.cues, *r.meta.LineResidualMS, wr})
			rows[name].add(r.cues, pc, *r.meta.LineResidualMS, wr)
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
			add(name+"/"+up).add(s.ref, s.cues, s.residual, s.wordResidual)
		}
	}
	if interrupted {
		_, _ = fmt.Fprintln(out, "timing-accuracy: interrupted; results below are partial")
	}
	printAccuracy(out, len(refs), dropRef, dropProv, dropStacked, anyRefWords, skipped, order, rows, tally, len(lanes))
	if interrupted {
		return 1
	}
	measured := false
	for _, a := range rows {
		measured = measured || a.line.Matched > 0
	}
	if !measured {
		_, _ = fmt.Fprintln(out, "timing-accuracy: no lane produced a measurement")
		return 1
	}
	return 0
}

// printAccuracy writes the aggregate report: only lane/upstream tokens and
// numbers, never a track identity or lyric text.
func printAccuracy(out io.Writer, nRefs, dropRef, dropProv, dropStacked int, words bool, skipped, order []string, rows map[string]*acc, tally map[string]*laneTally, nLanes int) {
	kind := "line starts"
	if words {
		kind = "line starts and word starts"
	}
	_, _ = fmt.Fprintf(out, "timing-accuracy: %d reference tracks, %s, tolerance %dms\n", nRefs, kind, timingacc.WithinMS)
	_, _ = fmt.Fprintf(out, "decorative cues dropped: reference=%d provider=%d\n", dropRef, dropProv)
	if dropStacked > 0 {
		_, _ = fmt.Fprintf(out, "reference lines with stacked timestamps, words not measured: %d\n", dropStacked)
	}
	if len(skipped) > 0 {
		_, _ = fmt.Fprintf(out, "skipped lanes: %s\n", strings.Join(skipped, ","))
	}
	if nLanes == 0 {
		_, _ = fmt.Fprintln(out, "no lane was measured")
	}
	sort.Strings(order)
	for _, key := range order {
		_, _ = fmt.Fprintln(out, timingacc.FormatRow(key, rows[key].line))
		if words {
			_, _ = fmt.Fprintln(out, timingacc.FormatWordRow(key, rows[key].word))
		}
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
