package commands

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/worker"
)

// TestRunServe_WiresInstrumentalTitleMode proves the serve graph hands
// instrumental_title.mode to the worker through the normalizing accessor
// (#1468): a mode written with stray case and spaces still turns the rule on,
// and with the key absent (off) the same row is fetched as it always was. The
// title and artist are invented.
func TestRunServe_WiresInstrumentalTitleMode(t *testing.T) {
	for _, tc := range []struct{ name, section, want string }{
		{"strong", "[instrumental_title]\nmode = \" Strong \"\n\n", "[source:title-rule]"},
		{"all", "[instrumental_title]\nmode = \"all\"\n\n", "[source:title-rule]"},
		{"legacy strong_weak alias", "[instrumental_title]\nmode = \"strong_weak\"\n\n", "[source:title-rule]"},
		{"off by default", "", "words from the provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			t.Setenv("MXLRC_DOCKER", "")
			t.Setenv("MUSIXMATCH_TOKEN", "test-token") // the fetcher is faked
			t.Setenv("MXLRC_SECRETS_KEY_FILE", filepath.Join(t.TempDir(), "test.key"))
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "serve.db")
			sqlDB, err := dbtest.OpenAt(context.Background(), t, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			track := models.Track{ArtistName: "Tin Lantern Band", TrackName: "Glass Harbor (Instrumental)"}
			if _, err := queue.NewDBQueue(sqlDB).Enqueue(context.Background(), models.Inputs{Track: track, Outdir: dir, Filename: "row.lrc"}, 1); err != nil {
				t.Fatal(err)
			}
			_ = sqlDB.Close()
			held, err := net.Listen("tcp", "127.0.0.1:0") // a held port makes runServe exit at the bind, after the wiring
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = held.Close() })
			cfgPath := filepath.Join(dir, "config.toml")
			cfg := tc.section + "[db]\npath = " + tomlString(dbPath) + "\n\n[providers]\nprimary = \"musixmatch\"\nfallback_order = [\"musixmatch\"]\n\n[server]\naddr = " + tomlString(held.Addr().String()) + "\n"
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			var body string
			serveWiringProbe = func(w *worker.Worker, _ lyrics.Writer) {
				if err := w.RunOnce(context.Background()); err != nil {
					t.Errorf("worker RunOnce: %v", err)
				}
				b, err := os.ReadFile(filepath.Join(dir, "row.txt"))
				if err != nil {
					t.Errorf("read row.txt: %v", err)
				}
				body = string(b)
			}
			t.Cleanup(func() { serveWiringProbe = func(*worker.Worker, lyrics.Writer) {} })
			song := models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "words from the provider"}}
			var out bytes.Buffer
			_ = runServe(context.Background(), &out, ServeCmd{ConfigPath: cfgPath},
				func(string) musixmatch.Fetcher { return blockedSongFetcher{song: song} },
				func(...string) lyrics.Writer { return lyrics.NewLRCWriter() })
			if !strings.Contains(body, tc.want) {
				t.Errorf("row.txt = %q; want it to contain %q", body, tc.want)
			}
		})
	}
}

// TestTitleRuleOn: the switch follows the accessor, not the raw field. A Config
// built in code can hold a mode the loader would have normalized or reset, and
// the raw field then disagrees with the accessor in both directions.
func TestTitleRuleOn(t *testing.T) {
	// on is the rule at all; weak is the Weak tier, all's alone (#1471).
	// strong_weak is the old name for all.
	type want struct{ on, weak bool }
	for mode, want := range map[config.InstrumentalTitleMode]want{
		"": {}, "off": {}, " OFF ": {}, "sometimes": {},
		"strong": {on: true}, " Strong ": {on: true},
		"ALL": {on: true, weak: true}, "STRONG_WEAK": {on: true, weak: true},
	} {
		cfg := config.Config{InstrumentalTitle: config.InstrumentalTitleConfig{Mode: mode}}
		if got := titleRuleOn(cfg); got != want.on {
			t.Errorf("titleRuleOn(mode %q) = %v, want %v", mode, got, want.on)
		}
		if got := titleRuleWeakOn(cfg); got != want.weak {
			t.Errorf("titleRuleWeakOn(mode %q) = %v, want %v", mode, got, want.weak)
		}
	}
}
