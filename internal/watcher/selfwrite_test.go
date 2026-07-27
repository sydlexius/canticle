package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

// tempRoot returns a temp directory with symlinks resolved.
//
// This matters, and it is not cosmetic. On macOS t.TempDir() hands back a
// /var/folders/... path while /var is a symlink to /private/var, so the
// filesystem events notify delivers report the /private/var form. eventTarget's
// containment check is purely lexical, so every such event resolves to no
// library and is discarded before it can ever reach dispatch -- which means a
// test rooted at the unresolved path observes no scan no matter what the code
// does, and an assertion of "no rescan happened" would pass vacuously.
// Resolving the root up front is what gives these tests teeth.
func tempRoot(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return resolved
}

// startWatcher runs a Watcher over root with self-write suppression enabled and
// returns the channels tests observe: the directories scanned (post-debounce)
// and the raw paths dropped as self-generated. It waits for the watch
// registration to settle before returning, since notify delivers nothing for a
// change that lands before the watch exists.
//
// Shared by the self-write suppression tests so the "external change still
// fires" test and the "self-write does not fire" test run against an
// IDENTICAL harness -- the comparison is only meaningful if the only
// difference between them is who performed the write.
func startWatcher(t *testing.T, root string, reg *selfwrite.Registry) (scanned <-chan string, suppressed <-chan string) {
	t.Helper()
	scans := make(chan string, 8)
	drops := make(chan string, 32)
	w := New(
		Config{Debounce: 20 * time.Millisecond, MaxDirs: defaultMaxDirs},
		fakeLister{libs: []models.Library{{ID: 3, Path: root}}},
		func(_ context.Context, _ models.Library, path string) error {
			select {
			case scans <- path:
			default:
			}
			return nil
		},
		nil,
	)
	w.SetSelfWriteRegistry(reg)
	w.suppressed = func(p string) {
		select {
		case drops <- p:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-runErr
	})

	// Allow watch registration, then DRAIN. macOS FSEvents replays events that
	// predate the watch (the temp directory's own creation, and anything a test
	// seeded before starting the watcher), and they arrive after registration
	// settles. Without this drain a "no rescan happened" assertion would be
	// measuring that backlog rather than the write under test -- which is exactly
	// how a broken suppression would look identical to a working one.
	time.Sleep(400 * time.Millisecond)
	drain(scans)
	drain(drops)
	return scans, drops
}

// drain empties a buffered channel without blocking.
func drain(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// writeSong is the song WriteLRC turns into a synced .lrc sidecar. Timestamps
// sit well inside the stated audio duration so the accept-time timing guard
// (#439) promotes it rather than demoting to .txt, which would write a
// different extension than the test asserts on.
func writeSong(artist, title string) models.Song {
	return models.Song{
		Track: models.Track{ArtistName: artist, TrackName: title, TrackLength: 180},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Time: models.Time{Total: 1, Seconds: 1}, Text: "one"},
			{Time: models.Time{Total: 5, Seconds: 5}, Text: "two"},
		}},
		AudioDurationSeconds: 180,
	}
}

// TestExternalChangeStillTriggersRescan is the guard against the failure mode
// that would make this whole fix worthless: a filter that quiets the disk by
// breaking the watcher. A registry is attached and populated with an unrelated
// self-write, then a genuine THIRD-PARTY write lands in the same watched
// directory. The rescan must still fire.
//
// This is exercised end to end through real notify event delivery -- not argued
// from the code, and not asserted against a hand-fed fake event -- because the
// claim being tested is precisely that the suppression path does not eat events
// it was never meant to see.
func TestExternalChangeStillTriggersRescan(t *testing.T) {
	root := tempRoot(t)
	reg := selfwrite.New(time.Minute)
	// A live, non-empty registry: the external write must survive suppression
	// that is switched on and actively holding entries, not merely an empty set.
	reg.Record(filepath.Join(root, "ours.lrc"))

	scanned, _ := startWatcher(t, root, reg)

	// A third party (a tagger, a file copy, the user) adds a file. Nothing about
	// it was ever recorded, so the watcher must react.
	external := filepath.Join(root, "third-party.flac")
	if err := os.WriteFile(external, []byte("x"), 0o644); err != nil {
		t.Fatalf("external write: %v", err)
	}

	select {
	case got := <-scanned:
		if got != root {
			t.Errorf("scanned path = %q; want %q", got, root)
		}
	case <-time.After(5 * time.Second):
		t.Skip("no filesystem event delivered within 5s (best-effort watcher; may be unsupported here)")
	}
}

// TestExternalChangeToTheSameDirectoryAsASelfWriteStillRescans is the sharper
// version of the above: the self-write and the external change land in the SAME
// directory. A suppression keyed on the directory (rather than the file) would
// pass the previous test and fail this one, silently swallowing every external
// change to any album canticle recently wrote a sidecar into.
func TestExternalChangeToTheSameDirectoryAsASelfWriteStillRescans(t *testing.T) {
	root := tempRoot(t)
	reg := selfwrite.New(time.Minute)
	scanned, drops := startWatcher(t, root, reg)

	// Our own write first, so the directory is "recently written by canticle".
	w := lyrics.NewLRCWriter()
	w.SetSelfWriteRegistry(reg)
	if err := w.WriteLRC(writeSong("A", "Ours"), "ours.lrc", root); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	waitFor(t, func() bool { return len(drops) > 0 }, "the self-write's own events to be suppressed")

	// Now a third party writes into that same directory.
	if err := os.WriteFile(filepath.Join(root, "theirs.flac"), []byte("x"), 0o644); err != nil {
		t.Fatalf("external write: %v", err)
	}

	select {
	case got := <-scanned:
		if got != root {
			t.Errorf("scanned path = %q; want %q", got, root)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("external change in a directory canticle just wrote to did NOT trigger a rescan; suppression is over-broad")
	}
}

// TestFullWriteCycleTriggersNoRescan is the fix itself: an entire WriteLRC
// atomic-write sequence -- temp Create/Write, Rename, the target Remove, the
// final Create, and the opposite-extension Remove -- must produce no rescan of
// the directory it landed in.
//
// The pre-existing .txt is deliberately present so the opposite-extension
// Remove actually happens. That event, and the final .lrc Create, are the two
// the ".tmp pattern" non-fix leaves behind.
func TestFullWriteCycleTriggersNoRescan(t *testing.T) {
	root := tempRoot(t)
	// The .txt the write will supersede, created BEFORE the watcher starts so its
	// own creation event is not what the test observes.
	if err := os.WriteFile(filepath.Join(root, "song.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed .txt: %v", err)
	}

	reg := selfwrite.New(time.Minute)
	scanned, drops := startWatcher(t, root, reg)

	w := lyrics.NewLRCWriter()
	w.SetSelfWriteRegistry(reg)
	if err := w.WriteLRC(writeSong("A", "Song"), "song.lrc", root); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "song.lrc")); err != nil {
		t.Fatalf("the .lrc was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "song.txt")); !os.IsNotExist(err) {
		t.Fatalf("the opposite-extension .txt survived; the Remove event under test never happened")
	}

	// Wait until events have actually been delivered and dropped, so a passing
	// assertion below means "suppressed", not "nothing arrived yet".
	waitFor(t, func() bool { return len(drops) > 0 }, "the write's filesystem events to reach the watcher")

	// Then wait out several debounce windows. Any surviving event would have
	// armed a timer and fired a scan well inside this.
	time.Sleep(300 * time.Millisecond)

	select {
	case got := <-scanned:
		t.Fatalf("a completed sidecar write triggered a rescan of %q; the self-trigger loop is intact", got)
	default:
	}

	// Every path the write touched must have been recognized, not just the .tmp.
	seen := map[string]bool{}
	for len(drops) > 0 {
		seen[filepath.Base(<-drops)] = true
	}
	// OR, not AND: each path must be recognized INDEPENDENTLY. With && the
	// assertion only fires when both are missing, so a fix that recognized the
	// .lrc but not the opposite-extension .txt Remove would pass here -- which is
	// exactly the half-fix this test exists to catch.
	if !seen["song.lrc"] || !seen["song.txt"] {
		t.Errorf("suppressed only %v; the final .lrc Create and the opposite .txt Remove must be recognized too, not just the .tmp", seen)
	}
}

// TestExpiredSelfWriteNoLongerSuppresses closes the loop on expiry at the
// watcher level: once a recorded entry ages out, an event for that exact path
// is external again. Without this, a missed event would leave a real file
// permanently invisible to the watcher.
func TestExpiredSelfWriteNoLongerSuppresses(t *testing.T) {
	root := tempRoot(t)
	reg := selfwrite.New(30 * time.Millisecond)
	scanned, drops := startWatcher(t, root, reg)

	// Prove notify delivery works HERE before the timeout below is allowed to
	// mean anything. The sibling tests t.Skip on this same 5s timeout because
	// delivery is best-effort and may be unsupported on a given platform; this
	// test t.Fatals instead, which is only defensible once delivery is
	// established. Without the sentinel, an environment with no working watcher
	// fails this test for a reason that has nothing to do with expiry.
	sentinel := filepath.Join(root, "sentinel.flac")
	reg.Record(sentinel)
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatalf("sentinel write: %v", err)
	}
	waitFor(t, func() bool { return len(drops) > 0 }, "the sentinel event to be suppressed")

	target := filepath.Join(root, "aged.flac")
	reg.Record(target)
	// Let the entry age out before the change lands.
	time.Sleep(60 * time.Millisecond)
	if reg.Suppress(target) {
		t.Fatal("entry did not expire; the rest of this test would prove nothing")
	}

	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-scanned:
		if got != root {
			t.Errorf("scanned path = %q; want %q", got, root)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a change to a path whose self-write record EXPIRED did not trigger a rescan; suppression is permanent")
	}
}

// TestNilRegistryPreservesPreFixBehavior: with no registry attached (the fetch
// CLI, and serve before the watcher is wired) every event is external, exactly
// as before #685.
func TestNilRegistryPreservesPreFixBehavior(t *testing.T) {
	root := tempRoot(t)
	scanned, drops := startWatcher(t, root, nil)

	if err := os.WriteFile(filepath.Join(root, "x.flac"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-scanned:
		if got != root {
			t.Errorf("scanned path = %q; want %q", got, root)
		}
	case <-time.After(5 * time.Second):
		t.Skip("no filesystem event delivered within 5s (best-effort watcher; may be unsupported here)")
	}
	if len(drops) != 0 {
		t.Errorf("%d events suppressed with a nil registry; want 0", len(drops))
	}
}
