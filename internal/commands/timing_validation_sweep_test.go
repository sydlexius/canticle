package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/revalidate"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
	"github.com/sydlexius/canticle/internal/timing"
)

// timingSweepCfg builds a config with the sweep on and its database under dir.
func timingSweepCfg(dbPath string, mutate func(*config.Config)) config.Config {
	cfg := config.Config{}
	cfg.DB.Path = dbPath
	cfg.TimingValidation.Enabled = true
	cfg.TimingValidation.RevalidateExisting = true
	cfg.TimingValidation.RevalidateBatch = 10
	cfg.TimingValidation.OnMisSynced = config.TimingActionDemote
	cfg.TimingValidation.OnCategorical = config.TimingActionQuarantine
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// THE CONFIG GATE
// ---------------------------------------------------------------------------

// TestNewTimingSweepJobRequiresBothFlags pins the two-key gate. It mirrors
// realign's: the master switch says the feature is live, revalidate_existing
// says an unattended pass may move or delete files written before the
// accept-time guard existed. Either alone must NOT start a file-moving loop.
func TestNewTimingSweepJobRequiresBothFlags(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sweep.db")
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	for _, tc := range []struct {
		name       string
		enabled    bool
		revalidate bool
		want       bool
	}{
		{"both off", false, false, false},
		{"enabled only", true, false, false},
		{"revalidate_existing only", false, true, false},
		{"both on", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := timingSweepCfg(dbPath, func(c *config.Config) {
				c.TimingValidation.Enabled = tc.enabled
				c.TimingValidation.RevalidateExisting = tc.revalidate
			})
			_, ok := newTimingSweepJob(context.Background(), sqlDB, cfg)
			if ok != tc.want {
				t.Errorf("newTimingSweepJob started = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestNewTimingSweepJobRejectsAnIllegalAction proves an invalid configuration
// refuses to START rather than running and failing per file. `demote` on the
// categorical arm is the case that matters: a categorical lyric is another
// song's words, so demoting it would write the WRONG words as a .txt beside the
// audio -- silently, forever, unattended.
func TestNewTimingSweepJobRejectsAnIllegalAction(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sweep.db")
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	cfg := timingSweepCfg(dbPath, func(c *config.Config) {
		c.TimingValidation.OnCategorical = config.TimingAction("demote")
	})
	if _, ok := newTimingSweepJob(context.Background(), sqlDB, cfg); ok {
		t.Error("sweep started with demote on the categorical arm; it would write another song's words beside the audio")
	}
}

// TestNewTimingSweepJobDefaultsANonPositiveBatch keeps a Config built in code
// (never through config.Load) from spinning a loop that drains nothing while
// still waking on every tick.
func TestNewTimingSweepJobDefaultsANonPositiveBatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sweep.db")
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	job, ok := newTimingSweepJob(context.Background(), sqlDB, timingSweepCfg(dbPath, func(c *config.Config) {
		c.TimingValidation.RevalidateBatch = 0
	}))
	if !ok {
		t.Fatal("sweep refused to start")
	}
	if job.batch != defaultTimingSweepBatch {
		t.Errorf("batch = %d, want the default %d", job.batch, defaultTimingSweepBatch)
	}
}

// TestResolveTimingSweepIntervalNeverReturnsZero pins the ticker rail. The sweep
// reuses the scan interval, which is zero in scan-once mode, and
// time.NewTicker(0) PANICS -- which would take serve mode down at startup. A
// scan-once deployment still has a backlog worth draining, so zero falls back to
// a cadence rather than to "never".
func TestResolveTimingSweepIntervalNeverReturnsZero(t *testing.T) {
	for _, in := range []time.Duration{0, -time.Second, -time.Hour} {
		if got := resolveTimingSweepInterval(in); got <= 0 {
			t.Errorf("resolveTimingSweepInterval(%v) = %v; time.NewTicker would panic", in, got)
		}
	}
	if got := resolveTimingSweepInterval(90 * time.Minute); got != 90*time.Minute {
		t.Errorf("a positive interval was not preserved: %v", got)
	}
}

// ---------------------------------------------------------------------------
// THE STAMP: what makes the sweep converge
// ---------------------------------------------------------------------------

// TestTimingRecordForAlwaysStamps is the convergence rail at the record layer.
// A row leaves the backlog by carrying a verdict, so EVERY finding must map to a
// non-empty Outcome -- SetTimingOutcome is a no-op on an empty one, which would
// leave the row in the backlog to be re-judged at the head of every cycle
// forever.
func TestTimingRecordForAlwaysStamps(t *testing.T) {
	for _, outcome := range []timing.TimingOutcome{
		timing.Ok, timing.MisSynced, timing.Categorical, timing.Degenerate,
		timing.UnknownDuration, "no_audio", "no_sidecar", "errored", "",
	} {
		rec := timingRecordFor(revalidate.Finding{Outcome: outcome})
		if rec.Outcome == "" {
			t.Errorf("finding %q produced an empty Outcome; SetTimingOutcome no-ops on it and the row never retires", outcome)
		}
		if rec.EvaluatedAt.IsZero() {
			t.Errorf("finding %q produced no EvaluatedAt", outcome)
		}
	}
}

// TestTimingRecordForNormalizesNonTimingVerdicts keeps the timing_outcome column
// a CLOSED vocabulary. /metrics groups by this column (#629), so inventing
// label values here would emit series nothing else in the system produces.
func TestTimingRecordForNormalizesNonTimingVerdicts(t *testing.T) {
	for _, outcome := range []timing.TimingOutcome{"no_audio", "no_sidecar", "errored", ""} {
		rec := timingRecordFor(revalidate.Finding{Outcome: outcome, Overrun: 9, Ratio: 9})
		if rec.Outcome != string(timing.UnknownDuration) {
			t.Errorf("finding %q stamped %q, want %q", outcome, rec.Outcome, timing.UnknownDuration)
		}
		if rec.Measured {
			t.Errorf("finding %q claimed Measured; no comparison happened", outcome)
		}
	}
	// A real verdict keeps its magnitudes.
	rec := timingRecordFor(revalidate.Finding{Outcome: timing.MisSynced, Overrun: 12.5, Ratio: 1.2})
	if !rec.Measured || rec.Magnitude != 12.5 || rec.Ratio != 1.2 {
		t.Errorf("a measured verdict lost its magnitudes: %+v", rec)
	}
}

// ---------------------------------------------------------------------------
// A FULL CYCLE, END TO END
// ---------------------------------------------------------------------------

// seedBacklogRow enqueues a completed synced row with no timing verdict -- one
// member of exactly the population ListTimingBacklog returns.
func seedBacklogRow(t *testing.T, q *queue.DBQueue, audioPath, artist, title string) {
	t.Helper()
	ctx := context.Background()
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: artist, TrackName: title},
		SourcePath: audioPath,
	}, 1)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if err := q.SetOutcomeType(ctx, item.ID, "synced"); err != nil {
		t.Fatalf("set outcome type: %v", err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

// sweepFixture builds a library root with one audio file and one MisSynced .lrc,
// a database holding its backlog row, and the job that would judge it.
func sweepFixture(t *testing.T, mutate func(*config.Config)) (*timingSweepJob, *queue.DBQueue, string, string) {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	dbPath := filepath.Join(base, "sweep.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(base, "music")
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	audio := filepath.Join(dir, "track.mp3")
	if err := os.WriteFile(audio, []byte("not really audio"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	lrc := filepath.Join(dir, "track.lrc")
	// Last cue at 2:30 against a 120s track: past Tolerance, under
	// CategoricalRatio, so MisSynced.
	if err := os.WriteFile(lrc, []byte("[00:10.00]alpha\n[02:30.00]beta\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "music", models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}

	q := queue.NewDBQueue(sqlDB)
	seedBacklogRow(t, q, audio, "Artist", "Title")

	job, ok := newTimingSweepJob(context.Background(), sqlDB, timingSweepCfg(dbPath, mutate))
	if !ok {
		t.Fatal("sweep refused to start")
	}
	// The real duration source is audiodur, which is cold in a test (every
	// lookup misses -> unknown duration -> fail open, and nothing would ever be
	// remediated). Swap in a lookup that knows this fixture's length, so the
	// remediation path is actually exercised rather than passing vacuously.
	job.rev = revalidate.New(
		func(context.Context, string, int64, int64) (int, bool, error) { return 120, true, nil },
		revalidate.Options{
			Roots:             []string{root},
			MisSyncedAction:   revalidate.Action(config.TimingActionDemote),
			CategoricalAction: revalidate.Action(config.TimingActionQuarantine),
			QuarantineDir:     filepath.Join(base, "quarantine"),
		},
	)
	return job, q, root, lrc
}

// TestRunCycleJudgesRemediatesAndStamps is the end-to-end rail: one cycle takes a
// backlog row all the way through to a remediated file and a retired row.
func TestRunCycleJudgesRemediatesAndStamps(t *testing.T) {
	ctx := context.Background()
	job, q, _, lrc := sweepFixture(t, nil)

	before, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("count backlog: %v", err)
	}
	if before != 1 {
		t.Fatalf("backlog = %d, want 1 before the cycle", before)
	}

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Counts.MisSynced != 1 {
		t.Errorf("MisSynced = %d, want 1", res.Counts.MisSynced)
	}
	if res.Remedied != 1 {
		t.Errorf("Remedied = %d, want 1", res.Remedied)
	}
	if res.Stamped != 1 {
		t.Errorf("Stamped = %d, want 1", res.Stamped)
	}
	if res.Failed != 0 {
		t.Errorf("Failed = %d, want 0", res.Failed)
	}
	// The demote wrote the words as .txt and moved the .lrc aside.
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Error("the MisSynced .lrc is still in place; the demote did not remove it")
	}
	txt := lrc[:len(lrc)-len(".lrc")] + ".txt"
	if _, err := os.Stat(txt); err != nil {
		t.Errorf("the demoted .txt was not written: %v", err)
	}
	// And the row is retired: the sweep converges.
	after, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("count backlog: %v", err)
	}
	if after != 0 {
		t.Errorf("backlog = %d after the cycle, want 0; the row was not retired and would be re-judged forever", after)
	}
	if res.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", res.Remaining)
	}
}

// TestRunCycleIsIdleWhenTheBacklogIsEmpty is the "drains, then idles" half of
// the acceptance criteria. A converged install must cost one query per cycle and
// touch no file at all.
func TestRunCycleIsIdleWhenTheBacklogIsEmpty(t *testing.T) {
	ctx := context.Background()
	job, _, _, _ := sweepFixture(t, nil)

	if _, err := job.runCycle(ctx); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	// The first cycle remediated, so it legitimately appended to the backup
	// trail. What must not grow is what an IDLE cycle adds: this pass runs
	// forever on a converged install, so a trail that grew every cycle would
	// fill the config directory with records of nothing.
	trailBefore := int64(-1)
	if fi, err := os.Stat(job.backupPath); err == nil {
		trailBefore = fi.Size()
	}

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if res.Counts.Scanned != 0 || res.Stamped != 0 || res.Remedied != 0 {
		t.Errorf("a converged cycle did work: %+v", res)
	}
	trailAfter := int64(-1)
	if fi, err := os.Stat(job.backupPath); err == nil {
		trailAfter = fi.Size()
	}
	if trailAfter != trailBefore {
		t.Errorf("an idle cycle appended to the backup trail (%d -> %d bytes)", trailBefore, trailAfter)
	}
}

// TestRunCycleOffActionsRecordButNeverTouchAFile pins the observability-only
// mode the docs recommend as the way to see what a sweep WOULD do. Both actions
// off must still judge and stamp -- otherwise the mode would never converge and
// would re-judge the same rows forever.
func TestRunCycleOffActionsRecordButNeverTouchAFile(t *testing.T) {
	ctx := context.Background()
	job, q, root, lrc := sweepFixture(t, nil)
	// Rebuild the revalidator with both arms off, keeping the same fixture.
	job.rev = revalidate.New(
		func(context.Context, string, int64, int64) (int, bool, error) { return 120, true, nil },
		revalidate.Options{
			Roots:             []string{root},
			MisSyncedAction:   revalidate.ActionOff,
			CategoricalAction: revalidate.ActionOff,
			QuarantineDir:     filepath.Join(t.TempDir(), "quarantine"),
		},
	)

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Counts.MisSynced != 1 {
		t.Errorf("MisSynced = %d, want 1: the verdict must still be reached", res.Counts.MisSynced)
	}
	if res.Remedied != 0 {
		t.Errorf("Remedied = %d, want 0 with both actions off", res.Remedied)
	}
	if res.Stamped != 1 {
		t.Errorf("Stamped = %d, want 1: an off-mode row must still retire, or the sweep never converges", res.Stamped)
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("the .lrc was touched with both actions off: %v", err)
	}
	remaining, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("backlog = %d, want 0", remaining)
	}
}

// TestRunCycleLeavesAFailedRemediationUnstamped is the apply-before-stamp rail.
// A stamp says "judged and acted on", so a row whose file could NOT be moved
// must stay in the backlog to be retried -- stamping it would retire the row
// while the offending .lrc is still sitting on disk, permanently.
func TestRunCycleLeavesAFailedRemediationUnstamped(t *testing.T) {
	ctx := context.Background()
	job, q, root, lrc := sweepFixture(t, nil)

	// Make the demote fail: point the quarantine root at a path that cannot be
	// created, because a plain FILE occupies it.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	job.rev = revalidate.New(
		func(context.Context, string, int64, int64) (int, bool, error) { return 120, true, nil },
		revalidate.Options{
			Roots:             []string{root},
			MisSyncedAction:   revalidate.Action(config.TimingActionDemote),
			CategoricalAction: revalidate.Action(config.TimingActionQuarantine),
			QuarantineDir:     filepath.Join(blocked, "quarantine"),
		},
	)

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("Failed = %d, want 1; the fixture did not actually fail, so this test proves nothing", res.Failed)
	}
	if res.Stamped != 0 {
		t.Errorf("Stamped = %d, want 0: a row whose remediation failed must stay in the backlog for a retry", res.Stamped)
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("the .lrc vanished despite the failure: %v", err)
	}
	remaining, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("backlog = %d, want 1: the failed row was retired and its bad .lrc would never be revisited", remaining)
	}
}

// TestRunCycleStampsAnUnjudgeableRow is the head-of-line rail. A row whose
// sidecar is gone can never reach a verdict, and the backlog is ordered
// oldest-first -- so if it is not retired it occupies a batch slot every cycle
// FOREVER and the rest of the backlog is never reached.
func TestRunCycleStampsAnUnjudgeableRow(t *testing.T) {
	ctx := context.Background()
	job, q, _, lrc := sweepFixture(t, nil)
	if err := os.Remove(lrc); err != nil {
		t.Fatalf("remove lrc: %v", err)
	}

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Counts.NoSidecar != 1 {
		t.Errorf("NoSidecar = %d, want 1", res.Counts.NoSidecar)
	}
	if res.Stamped != 1 {
		t.Errorf("Stamped = %d, want 1", res.Stamped)
	}
	remaining, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("backlog = %d, want 0: an unjudgeable row head-of-lines every future cycle", remaining)
	}
}

// TestRunCycleRespectsTheBatchBudget keeps one cycle bounded. The budget is what
// makes the sweep safe on a parked array: a cycle's cost must scale with the
// batch, never with the size of the backlog.
func TestRunCycleRespectsTheBatchBudget(t *testing.T) {
	ctx := context.Background()
	job, q, root, _ := sweepFixture(t, func(c *config.Config) {
		c.TimingValidation.RevalidateBatch = 2
	})
	// Four more rows, five in total, against a budget of two.
	dir := filepath.Join(root, "album")
	for i := range 4 {
		name := filepath.Join(dir, "extra"+string(rune('a'+i)))
		if err := os.WriteFile(name+".mp3", []byte("not really audio"), 0o600); err != nil {
			t.Fatalf("write audio: %v", err)
		}
		if err := os.WriteFile(name+".lrc", []byte("[00:10.00]alpha\n[02:30.00]beta\n"), 0o600); err != nil {
			t.Fatalf("write lrc: %v", err)
		}
		seedBacklogRow(t, q, name+".mp3", "Artist", "Extra"+string(rune('a'+i)))
	}

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Counts.Scanned > 2 {
		t.Errorf("scanned %d files against a budget of 2; the batch does not bound the cycle", res.Counts.Scanned)
	}
	if res.Remaining != 3 {
		t.Errorf("Remaining = %d, want 3 (5 seeded, 2 judged)", res.Remaining)
	}
}

// ---------------------------------------------------------------------------
// THE LOOP
// ---------------------------------------------------------------------------

// fakeSweeper counts cycles and can fail on demand.
type fakeSweeper struct {
	cycles chan struct{}
	err    error
}

func (f *fakeSweeper) runCycle(context.Context) (timingSweepResult, error) {
	select {
	case f.cycles <- struct{}{}:
	default:
	}
	return timingSweepResult{}, f.err
}

// TestRunTimingSweepLoopRunsAtStartupAndStopsOnCancel pins both loop rails: a
// cycle runs immediately (a fresh backlog begins draining rather than waiting
// out a full interval), and cancellation returns so serve mode's wg.Wait
// unblocks on shutdown.
func TestRunTimingSweepLoopRunsAtStartupAndStopsOnCancel(t *testing.T) {
	f := &fakeSweeper{cycles: make(chan struct{}, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTimingSweepLoop(ctx, f, time.Hour)
	}()

	select {
	case <-f.cycles:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no cycle ran at startup; a fresh backlog would wait out a full interval")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not return on cancel; serve mode's wg.Wait would hang on shutdown")
	}
}

// TestRunTimingSweepCycleSwallowsAFailure keeps a transient error from killing
// the goroutine: a failed cycle must wait for the next interval, not end the
// sweep for the daemon's lifetime.
func TestRunTimingSweepCycleSwallowsAFailure(t *testing.T) {
	f := &fakeSweeper{cycles: make(chan struct{}, 1), err: errors.New("database is busy")}
	// The assertion is that this returns rather than panicking or blocking.
	runTimingSweepCycle(context.Background(), f)
}

// TestNewTimingSweepJobRefusesAQuarantineInsideALibrary is the containment rail,
// and it matters MORE for the sweep than for the CLI. QuarantineDir defaults to
// <db-dir>/quarantine, so an install whose database sits under a library root
// (a Docker bind of /config into the music tree, say) would have this unattended
// pass move rejected sidecars INTO the library -- where the watcher then sees
// them as new files and a later cycle re-judges and re-quarantines its own
// output, deeper each time.
//
// It is a REGRESSION TEST for a real defect in this file's first draft: Options
// was built without Roots, so Validate's containment check ranged over an empty
// slice and passed vacuously. The check existed and could not fail.
func TestNewTimingSweepJobRefusesAQuarantineInsideALibrary(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	// The library root CONTAINS the database directory, so <db-dir>/quarantine
	// lands inside the library.
	root := filepath.Join(base, "music")
	dbDir := filepath.Join(root, "config")
	if err := os.MkdirAll(dbDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "sweep.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	if _, err := library.New(sqlDB).Add(ctx, root, "music", models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}

	if _, ok := newTimingSweepJob(ctx, sqlDB, timingSweepCfg(dbPath, nil)); ok {
		t.Error("the sweep started with its quarantine root inside a library; it would move rejected sidecars into the music tree and re-quarantine its own output")
	}
}

// ---------------------------------------------------------------------------
// THE COLD-CACHE RAIL: a duration miss must RESOLVE, not become a verdict
// ---------------------------------------------------------------------------

// TestBankingDurationLookupFillsAColdCache is the regression test for the defect
// that made this whole feature inert.
//
// audiodur.Lookup is a pure SQL read with no fill path, and the population this
// sweep judges is precisely the one no other component fills: a file that
// already carries a sidecar is skipped ~200 lines before the scanner's
// enrichment probe, so it was never duration-probed by an older build (#684).
// With a raw Lookup every candidate missed, every candidate failed open, and the
// sweep stamped the entire backlog unknown_duration while remediating nothing --
// then reported "converged" having judged nothing, with each stamp retiring its
// row permanently.
//
// This exercises the PRODUCTION lookup against a real audio fixture, not a stub:
// a miss must come back with a real duration AND leave the cache warm.
func TestBankingDurationLookupFillsAColdCache(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(base, "bank.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	// A real FLAC header: 44100 Hz, 4410000 samples = exactly 100 seconds.
	if err := testutil.WriteFLACFile(base, "track.flac", 44100, 4410000); err != nil {
		t.Fatalf("write flac: %v", err)
	}
	audio := filepath.Join(base, "track.flac")
	info, err := os.Stat(audio)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mtime, size := info.ModTime().UnixNano(), info.Size()

	store := audiodur.New(sqlDB, scanner.DurationReaderVersion)
	// Precondition: the cache really is cold. Without this the test could pass
	// against a warm cache and prove nothing about the banking path.
	if _, found, err := store.Lookup(ctx, audio, mtime, size); err != nil || found {
		t.Fatalf("precondition: cache should be cold, got found=%v err=%v", found, err)
	}

	lookup := bankingDurationLookup(store)
	seconds, found, err := lookup(ctx, audio, mtime, size)
	if err != nil {
		t.Fatalf("banking lookup: %v", err)
	}
	if !found {
		t.Fatal("a cold cache reported no duration; every candidate would fail open and the sweep would judge nothing")
	}
	if seconds != 100 {
		t.Errorf("duration = %d, want 100", seconds)
	}
	// And it BANKED it: the next raw lookup must hit, so the probe is paid once
	// per file version rather than once per cycle (the #684 bargain).
	banked, found, err := store.Lookup(ctx, audio, mtime, size)
	if err != nil || !found {
		t.Fatalf("the duration was not banked (found=%v err=%v); every cycle would re-read the header and hold the array awake", found, err)
	}
	if banked != 100 {
		t.Errorf("banked duration = %d, want 100", banked)
	}
}

// TestBankingDurationLookupDegradesToAMiss keeps every failure on the fail-open
// path. Banking may only ever turn an unknown into a known; it must never turn a
// known into a wrong verdict, and an unreadable or unparsable file must read as
// the miss it already was rather than as an error that abandons the cycle.
func TestBankingDurationLookupDegradesToAMiss(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(base, "bank.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	notAudio := filepath.Join(base, "track.mp3")
	if err := os.WriteFile(notAudio, []byte("not really audio at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	lookup := bankingDurationLookup(audiodur.New(sqlDB, scanner.DurationReaderVersion))

	for _, tc := range []struct{ name, path string }{
		{"unparsable file", notAudio},
		{"missing file", filepath.Join(base, "gone.flac")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seconds, found, err := lookup(ctx, tc.path, 0, 0)
			if err != nil {
				t.Errorf("error = %v; an unreadable file must degrade to a miss, not abandon the cycle", err)
			}
			if found || seconds != 0 {
				t.Errorf("got (%d, %v), want (0, false): the sidecar must be left unjudged", seconds, found)
			}
		})
	}
}

// TestBankingDurationLookupPropagatesAStoreFailure keeps the transient/persistent
// split intact. A broken duration store says nothing about any one file, so it
// must propagate and abandon the cycle rather than triggering a library-wide
// header read or stamping rows that were never judged.
func TestBankingDurationLookupPropagatesAStoreFailure(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(base, "bank.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Closing the database makes every Lookup fail at the store layer.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	lookup := bankingDurationLookup(audiodur.New(sqlDB, scanner.DurationReaderVersion))
	if _, _, err := lookup(ctx, filepath.Join(base, "track.flac"), 1, 1); err == nil {
		t.Error("a store failure returned no error; the sweep would stamp rows it never judged")
	}
}

// TestRunCycleUsesTheProductionDurationWiring closes the gap the fixture
// substitution leaves open, and it is the test that actually pins the fix.
//
// Every other end-to-end test here REPLACES job.rev with a hand-built
// Revalidator, so none of them exercises the duration source newTimingSweepJob
// actually wires. That substitution is what let the original defect ship: with
// a raw audiodur.Lookup (a pure SQL read that never fills itself) the whole
// suite passed while the feature judged nothing on a real install. Reverting the
// banking wrapper must REDDEN a test, and this is that test -- it builds the job
// through the production constructor and touches job.rev not at all.
//
// The fixture is a real FLAC (44100 Hz, 4410000 samples = 100s) beside a .lrc
// whose last cue sits at 1:50 -- past 100s + Tolerance, so a sweep that
// genuinely resolves the duration MUST reach MisSynced and demote it. A sweep
// that cannot resolve it reads unknown_duration and does nothing.
func TestRunCycleUsesTheProductionDurationWiring(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	dbPath := filepath.Join(base, "sweep.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	root := filepath.Join(base, "music")
	dir := filepath.Join(root, "album")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := testutil.WriteFLACFile(dir, "track.flac", 44100, 4410000); err != nil {
		t.Fatalf("write flac: %v", err)
	}
	audio := filepath.Join(dir, "track.flac")
	lrc := filepath.Join(dir, "track.lrc")
	// 1:50 = 110s against the 100s fixture: the 10s overrun clears Tolerance (2s)
	// while ratio 1.1 stays under CategoricalRatio (1.5), so this is MisSynced --
	// the arm that DEMOTES, which is what makes the remediation assertion below
	// meaningful. (2:30 would be ratio 1.5 exactly, which classifies Categorical.)
	if err := os.WriteFile(lrc, []byte("[00:10.00]alpha\n[01:50.00]beta\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "music", models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	seedBacklogRow(t, q, audio, "Artist", "Title")

	// The PRODUCTION constructor, and no substitution afterwards.
	job, ok := newTimingSweepJob(ctx, sqlDB, timingSweepCfg(dbPath, nil))
	if !ok {
		t.Fatal("sweep refused to start")
	}

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Counts.UnknownDuration > 0 {
		t.Errorf("the production wiring could not resolve a duration (%d unknown); on a real install this retires the whole backlog while remediating nothing",
			res.Counts.UnknownDuration)
	}
	if res.Counts.MisSynced != 1 {
		t.Errorf("MisSynced = %d, want 1: the sweep did not reach the verdict its own fixture guarantees", res.Counts.MisSynced)
	}
	if res.Remedied != 1 {
		t.Errorf("Remedied = %d, want 1", res.Remedied)
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Error("the MisSynced .lrc is still in place; the production path did not remediate it")
	}
}

// untaggedMP3Bytes builds a real MPEG-1 Layer III stream carrying NO tag block:
// 128kbps, 44100Hz, no padding, so each frame is 417 bytes and 26.12ms long.
// Tag-based readers reject such a file outright; its duration is nonetheless
// perfectly parsable from the frames.
func untaggedMP3Bytes(frames int) []byte {
	const frameSize = 417
	out := make([]byte, 0, frames*frameSize)
	for range frames {
		frame := make([]byte, frameSize)
		frame[0], frame[1], frame[2], frame[3] = 0xFF, 0xFB, 0x90, 0x00
		out = append(out, frame...)
	}
	return out
}

// TestBankingDurationLookupResolvesUntaggedAudio is the regression test for a
// population the first banking fix still starved.
//
// scanner.ReadAudioFacts reads TAGS first and treats their absence as fatal, so
// a valid, perfectly parsable file carrying no tag block yielded no duration at
// all -- the candidate failed open and its row was stamped unknown_duration,
// which retires it permanently. Untagged files are exactly the ones most likely
// to carry a hand-made or scraped sidecar, i.e. the backlog this sweep exists to
// judge, so the original Critical survived for precisely the wrong population.
// FLAC hid the problem in testing because its STREAMINFO doubles as a tag block.
func TestBankingDurationLookupResolvesUntaggedAudio(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(base, "bank.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // reason: test cleanup

	// 383 frames at 26.12ms is ~10 seconds.
	audio := filepath.Join(base, "untagged.mp3")
	if err := os.WriteFile(audio, untaggedMP3Bytes(383), 0o600); err != nil {
		t.Fatalf("write mp3: %v", err)
	}
	info, err := os.Stat(audio)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	lookup := bankingDurationLookup(audiodur.New(sqlDB, scanner.DurationReaderVersion))
	seconds, found, err := lookup(ctx, audio, info.ModTime().UnixNano(), info.Size())
	if err != nil {
		t.Fatalf("banking lookup: %v", err)
	}
	if !found || seconds <= 0 {
		t.Fatalf("an untagged but valid MP3 resolved to (%d, %v); its row would be stamped unknown_duration and retired forever, and untagged files are the likeliest to carry the sidecars this sweep judges", seconds, found)
	}
	if seconds != 10 {
		t.Errorf("duration = %d, want 10", seconds)
	}
}

// TestTimingOutcomeIsTerminalKeepsErroredRetriable separates a fact about the
// FILE from a fact about the ATTEMPT.
//
// The stamp is one-way: it removes a row from ListTimingBacklog forever. Every
// verdict but one settles the file -- ok/mis_synced/categorical/degenerate are
// judgments, and no_audio/no_sidecar are filesystem facts. "errored" is neither:
// judge counts it when the .lrc cannot be read or parsed, which is equally what
// a temporarily unavailable mount or a transient I/O error looks like. Stamping
// it means one bad moment permanently exempts a sidecar from the unattended
// pass, recoverable only by running the CLI by hand.
func TestTimingOutcomeIsTerminalKeepsErroredRetriable(t *testing.T) {
	settled := []timing.TimingOutcome{
		timing.Ok, timing.MisSynced, timing.Categorical, timing.Degenerate,
		timing.UnknownDuration, "no_audio", "no_sidecar",
	}
	for _, outcome := range settled {
		if !timingOutcomeIsTerminal(revalidate.Finding{Outcome: outcome}) {
			t.Errorf("outcome %q was treated as retriable; it is a settled fact and its row must retire, or it holds a batch slot forever", outcome)
		}
	}
	if timingOutcomeIsTerminal(revalidate.Finding{Outcome: "errored"}) {
		t.Error("an errored finding was stamped as terminal; a transient read failure would exempt that sidecar from the sweep permanently")
	}
}

// TestRunCycleLeavesAnErroredRowUnstamped is the end-to-end half: an unreadable
// .lrc must stay in the backlog for the next cycle rather than being retired.
func TestRunCycleLeavesAnErroredRowUnstamped(t *testing.T) {
	ctx := context.Background()
	job, q, root, lrc := sweepFixture(t, nil)

	// A .lrc the parser cannot make sense of: no cue carries a timestamp, so
	// EvaluateLRCFile fails and judge counts Errored.
	if err := os.Remove(lrc); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(lrc, 0o750); err != nil {
		// A directory where the sidecar should be: unreadable as a file.
		t.Fatalf("mkdir: %v", err)
	}
	_ = root

	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Counts.Errored != 1 {
		t.Fatalf("Errored = %d, want 1; the fixture did not actually error, so this test proves nothing", res.Counts.Errored)
	}
	if res.Stamped != 0 {
		t.Errorf("Stamped = %d, want 0: an errored row must stay retriable", res.Stamped)
	}
	remaining, cerr := q.CountTimingBacklog(ctx)
	if cerr != nil {
		t.Fatalf("count: %v", cerr)
	}
	if remaining != 1 {
		t.Errorf("backlog = %d, want 1: the row was retired on a transient failure and only the CLI could bring it back", remaining)
	}
}

// TestRunCycleSkipsWhenALibraryStartsContainingTheQuarantineRoot pins the
// containment check against the LIVE library set rather than the startup one.
//
// Libraries are mutable at runtime. The roots feeding Validate are resolved once
// at construction, so an operator who adds a root that CONTAINS
// <db-dir>/quarantine while serve is running would keep passing a check made
// against a stale list -- and the sweep would move rejected sidecars INTO the
// music library, where the watcher sees them as new files and a later cycle
// re-quarantines its own output, deeper each time, until the daemon restarts.
//
// The cycle must SKIP rather than remediate: nothing is stamped, so every row
// stays in the backlog and is re-judged once the configuration is sane again.
func TestRunCycleSkipsWhenALibraryStartsContainingTheQuarantineRoot(t *testing.T) {
	ctx := context.Background()
	job, q, _, lrc := sweepFixture(t, nil)

	// Add a library root that contains the quarantine directory, exactly as an
	// operator could at runtime. The job was constructed before this existed.
	quarantineParent := filepath.Dir(job.opts.QuarantineDir)
	if _, err := job.libs.Add(ctx, quarantineParent, "engulfing", models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}

	before, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	res, err := job.runCycle(ctx)
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	if res.Remedied != 0 {
		t.Errorf("Remedied = %d, want 0: the sweep remediated into a directory now inside a library", res.Remedied)
	}
	if res.Stamped != 0 {
		t.Errorf("Stamped = %d, want 0: a skipped cycle must retire nothing", res.Stamped)
	}
	if _, serr := os.Stat(lrc); serr != nil {
		t.Errorf("the sidecar was moved despite an unsafe configuration: %v", serr)
	}
	after, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("backlog %d -> %d; a skipped cycle must leave every row to be re-judged", before, after)
	}
}
