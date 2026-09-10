// Command smokefixtures generates the live serve-smoke test library (#948): one
// silent, ID3-tagged MP3 per track in a local TOML list, each exactly the listed
// length, plus a nonsense-tagged negative control that must end as a miss. Each
// file's duration is re-read with the same reader serve uses and a mismatch is
// fatal. Output is refused inside the repository tree, and a non-empty output
// directory is refused unless -clean.
//
// It is a developer tool and is intentionally excluded from releases
// (GoReleaser builds only ./cmd/mxlrcgo-svc). Run it via `make smoke-fixtures`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/ffmpeg"
	"github.com/sydlexius/canticle/internal/scanner"
)

// options are the parsed command-line flags.
type options struct {
	out, tracks, ffmpegOverride, cacheDir string
	clean                                 bool
}

// ffmpegResolver is ffmpeg.Resolve's shape; a parameter so run is testable
// without the pinned download.
type ffmpegResolver func(ctx context.Context, override string, opts ffmpeg.Options) (string, error)

func main() {
	var o options
	flag.StringVar(&o.out, "out", "/tmp/canticle-smoke-fixtures", "output directory (must be outside the repository)")
	flag.StringVar(&o.tracks, "tracks", "smoke-fixtures.local.toml", "TOML track list ([[track]] artist/album/title/duration)")
	flag.StringVar(&o.ffmpegOverride, "ffmpeg", "", "explicit ffmpeg path (default: pinned cache, then PATH, then a checksum-pinned download)")
	flag.StringVar(&o.cacheDir, "ffmpeg-cache", defaultCacheDir(), "cache directory for the auto-provisioned ffmpeg build")
	flag.BoolVar(&o.clean, "clean", false, "replace the fixtures (and their sidecars) listed in the manifest a previous run wrote to -out; unlisted files are left alone, and a non-empty -out with no manifest is refused")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, o, ffmpeg.Resolve); err != nil {
		fmt.Fprintln(os.Stderr, "smokefixtures:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, resolve ffmpegResolver) error {
	out, err := expandHome(o.out)
	if err != nil {
		return err
	}
	tracksPath, err := expandHome(o.tracks)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	if err := RefuseInsideRepo(out, cwd); err != nil {
		return err
	}
	list, err := LoadTracks(tracksPath)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", tracksPath, errNoTracks)
	}
	if err != nil {
		return err
	}
	list = WithControl(list)
	if err := PrepareOut(out, o.clean); err != nil {
		return err
	}

	bin, err := resolve(ctx, o.ffmpegOverride, ffmpeg.Options{CacheDir: o.cacheDir})
	if err != nil {
		return fmt.Errorf("resolve ffmpeg: %w", err)
	}
	g := Generator{Encode: ffmpegEncoder(bin), ReadDuration: audioSeconds}
	paths, err := g.Run(ctx, list, out)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Println(p)
	}
	fmt.Printf("wrote %d fixtures (incl. 1 negative control) to %s; durations verified\n", len(paths), out)
	return nil
}

// audioSeconds is the production duration reader: the same one serve uses.
func audioSeconds(p string) (int, error) {
	secs, _, _, err := scanner.ReadAudioDuration(p)
	return secs, err
}

// expandHome expands a leading "~/" to the user's home directory. make and a
// quoted shell word both pass "~" through literally.
func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %s: %w", p, err)
	}
	return filepath.Join(home, p[2:]), nil
}

// defaultCacheDir is the per-user cache for the pinned ffmpeg build; "" (skip the
// cache and download steps) when no user cache dir is known.
func defaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "canticle", "ffmpeg")
}
