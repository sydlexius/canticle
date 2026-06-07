// Command genlib generates a synthetic music library: tagged .mp3 files
// (ID3v2.4/UTF-8) with optional embedded USLT lyrics and optional .lrc
// sidecars, for load- and concurrency-testing mxlrcgo-svc against a #131-style
// large library without touching real music. It is a developer tool and is
// intentionally excluded from releases (GoReleaser builds only
// ./cmd/mxlrcgo-svc).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sydlexius/mxlrcgo-svc/internal/testutil"
)

func main() {
	out := flag.String("out", "synthetic-library", "output root directory")
	libraries := flag.Int("libraries", 1, "number of top-level libraries to create under -out")
	artists := flag.Int("artists", 10, "artists per library")
	albums := flag.Int("albums", 5, "albums per artist")
	tracks := flag.Int("tracks", 10, "tracks per album")
	embed := flag.Bool("embed-lyrics", false, "embed a USLT (unsynced) lyrics frame in every track")
	lrcEvery := flag.Int("lrc-every", 0, "write a stub .lrc sidecar for every Nth track (0 = none, 1 = all)")
	realistic := flag.Bool("realistic", false, "generate real catalog-matchable artist/title/album names (for end-to-end fetch testing) instead of synthetic placeholders; ignores -artists/-albums/-tracks")
	flag.Parse()

	if *libraries <= 0 {
		fmt.Fprintln(os.Stderr, "genlib: -libraries must be > 0")
		os.Exit(2)
	}

	total := 0
	for l := 1; l <= *libraries; l++ {
		root := *out
		if *libraries > 1 {
			root = filepath.Join(*out, fmt.Sprintf("Library %02d", l))
		}
		n, err := testutil.GenerateLibrary(root, testutil.GenSpec{
			Artists:     *artists,
			Albums:      *albums,
			Tracks:      *tracks,
			EmbedLyrics: *embed,
			LRCEvery:    *lrcEvery,
			Realistic:   *realistic,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "genlib: %v\n", err)
			os.Exit(1)
		}
		total += n
	}
	fmt.Printf("generated %d tracks across %d libraries under %s\n", total, *libraries, *out)
}
