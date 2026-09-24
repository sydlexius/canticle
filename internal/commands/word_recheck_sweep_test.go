package commands

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/worker"
)

// fixedGen is a wordGenerationSource with a set value.
type fixedGen int64

func (g fixedGen) WordGeneration() int64 { return int64(g) }

// wordSweepDB opens a real SQLite DB seeded with n settled line-synced rows,
// each a full recheck candidate (ids 1..n).
func wordSweepDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	dbh, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	for i := 1; i <= n; i++ {
		if _, err := dbh.Exec(`INSERT INTO work_queue (id, artist, title, artist_key, title_key, source_path, status, outcome_type, timing_outcome, completed_at)
		      VALUES (?, 'a', ?, 'a', ?, ?, 'done', 'synced', 'ok', ?)`,
			i, fmt.Sprint(i), fmt.Sprint(i), fmt.Sprintf("/fixture/%d.flac", i), fmt.Sprintf("2026-01-%02dT00:00:00Z", i)); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	return dbh
}

func wordSweepCfg(enabled bool, batch int) config.Config {
	cfg := config.Config{}
	cfg.Output.WordSyncMode = config.WordSyncModeSidecar
	cfg.WordSyncRecheck.Enabled = enabled
	cfg.WordSyncRecheck.Batch = batch
	return cfg
}

func wordSweepJob(t *testing.T, dbh *sql.DB, cfg config.Config, gen wordGenerationSource) *wordRecheckSweepJob {
	t.Helper()
	j, ok := newWordRecheckSweepJob(dbh, cfg, gen)
	if !ok {
		t.Fatal("sweep did not start")
	}
	return j
}

func cycle(t *testing.T, j *wordRecheckSweepJob) wordRecheckSweepResult {
	t.Helper()
	res, err := j.runCycle(context.Background())
	if err != nil {
		t.Fatalf("runCycle: %v", err)
	}
	return res
}

func countQueued(t *testing.T, dbh *sql.DB) int {
	t.Helper()
	n, err := queue.NewDBQueue(dbh).CountWordRecheckQueued(context.Background(), nil)
	if err != nil {
		t.Fatalf("count queued: %v", err)
	}
	return n
}

// captureSlog routes the default logger to a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestWordRecheckSweepGateWritesNothing: disabled (the default) and
// word_sync_mode=off both refuse at construction, so no cycle can ever run, and
// the off refusal is a single Warn for the process, not one per cycle.
func TestWordRecheckSweepGateWritesNothing(t *testing.T) {
	dbh := wordSweepDB(t, 3)
	before := dumpTable(t, dbh, "work_queue")
	logs := captureSlog(t)

	if _, ok := newWordRecheckSweepJob(dbh, wordSweepCfg(false, 10), fixedGen(1)); ok {
		t.Fatal("disabled sweep started")
	}
	off := wordSweepCfg(true, 10)
	off.Output.WordSyncMode = config.WordSyncModeOff
	if _, ok := newWordRecheckSweepJob(dbh, off, fixedGen(1)); ok {
		t.Fatal("sweep started under word_sync_mode=off")
	}
	if got := strings.Count(logs.String(), "word_sync_mode is off"); got != 1 {
		t.Fatalf("off refusal logged %d times; want exactly 1:\n%s", got, logs)
	}
	if after := dumpTable(t, dbh, "work_queue"); after != before {
		t.Fatalf("a refused sweep wrote work_queue:\nbefore %s\nafter  %s", before, after)
	}
}

// TestWordRecheckSweepBatchFallback: a Config built in code (batch 0) still
// admits the documented default rather than nothing forever.
func TestWordRecheckSweepBatchFallback(t *testing.T) {
	j := wordSweepJob(t, wordSweepDB(t, 0), wordSweepCfg(true, 0), fixedGen(1))
	if j.batch != defaultWordRecheckBatch {
		t.Fatalf("batch = %d; want %d", j.batch, defaultWordRecheckBatch)
	}
}

// TestWordRecheckSweepCapHoldsAcrossCycles: never more than batch rows in
// recheck mode, counting rows another path (the CLI) queued, and a settled row
// frees exactly its slot. Also covers the full-cap log line: it fires once on
// the transition into "full" (cycle 2), not again while it stays full.
func TestWordRecheckSweepCapHoldsAcrossCycles(t *testing.T) {
	dbh := wordSweepDB(t, 6)
	// Row 6 was queued by the CLI; it occupies a slot.
	if _, err := dbh.Exec(`UPDATE work_queue SET status = 'deferred', word_timing_state = 'queued' WHERE id = 6`); err != nil {
		t.Fatal(err)
	}
	logs := captureSlog(t)
	j := wordSweepJob(t, dbh, wordSweepCfg(true, 3), fixedGen(1))

	if res := cycle(t, j); res.Admitted != 2 || res.InFlight != 1 {
		t.Fatalf("cycle 1 = %+v; want 2 admitted beside 1 in flight", res)
	}
	if n := countQueued(t, dbh); n != 3 {
		t.Fatalf("queued after cycle 1 = %d; want 3", n)
	}
	if res := cycle(t, j); res.Admitted != 0 {
		t.Fatalf("cycle 2 admitted %d at a full cap", res.Admitted)
	}
	if res := cycle(t, j); res.Admitted != 0 {
		t.Fatalf("cycle 2b admitted %d at a full cap", res.Admitted)
	}
	if got := strings.Count(logs.String(), "at capacity"); got != 1 {
		t.Fatalf("full-cap log fired %d times across two full cycles; want exactly 1:\n%s", got, logs)
	}
	// The worker settles row 1 (oldest completion, so the first admitted).
	q := queue.NewDBQueue(dbh)
	if _, err := dbh.Exec(`UPDATE work_queue SET status = 'processing' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := q.SettleWordRecheck(context.Background(), 1, queue.WordTimingServed, 1); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if res := cycle(t, j); res.Admitted != 1 {
		t.Fatalf("cycle 3 admitted %d; want exactly the freed slot", res.Admitted)
	}
	if n := countQueued(t, dbh); n != 3 {
		t.Fatalf("queued after cycle 3 = %d; want 3", n)
	}
}

// TestWordRecheckSweepCapIgnoresPruneRetiredRow is I1 (hostile review, #1048
// slice 7): prune.retireUnresolvable retires a row whose source file vanished
// to status='done' while leaving word_timing_state='queued' (#1039), so that
// row can never drain on its own. Before this fix the sweep's cap counted it
// via CountWordRecheckQueued (any status), permanently holding its slot and
// drifting admissions toward zero as such rows accumulated. It must not
// consume a slot: with batch=1 and one prune-retired row present, the sweep
// must still admit a fresh candidate.
func TestWordRecheckSweepCapIgnoresPruneRetiredRow(t *testing.T) {
	dbh := wordSweepDB(t, 2)
	// Row 1 stands in for a prune-retired recheck row: flipped, then the source
	// vanished and prune.retireUnresolvable retired it (status='done',
	// word_timing_state left 'queued', exactly what retireUnresolvableSQL does).
	if _, err := dbh.Exec(`UPDATE work_queue SET status = 'done', word_timing_state = 'queued', last_error = 'source file no longer exists' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	j := wordSweepJob(t, dbh, wordSweepCfg(true, 1), fixedGen(1))

	res := cycle(t, j)
	if res.InFlight != 0 {
		t.Fatalf("in_flight = %d; want 0 (the prune-retired row must not count as in flight)", res.InFlight)
	}
	if res.Admitted != 1 {
		t.Fatalf("admitted %d; want 1 (row 2, the only real candidate, since the retired row left the cap empty)", res.Admitted)
	}
	var state string
	if err := dbh.QueryRow(`SELECT word_timing_state FROM work_queue WHERE id = 2`).Scan(&state); err != nil || state != queue.WordTimingQueued {
		t.Fatalf("row 2 state = %q (%v); want queued", state, err)
	}
}

// TestWordRecheckSweepDoesNotReadmitAnUnflippedRow is PROBE-K: the worker's
// wait-budget un-flip returns a row to done with no verdict, which the
// candidate predicate alone reads as never examined. The flip's checked_at
// stamp holds it out until the cooldown passes.
func TestWordRecheckSweepDoesNotReadmitAnUnflippedRow(t *testing.T) {
	dbh := wordSweepDB(t, 1)
	j := wordSweepJob(t, dbh, wordSweepCfg(true, 5), fixedGen(1))
	// The flip stamps with the queue's wall clock, so the fake clock starts there.
	now := time.Now()
	j.now = func() time.Time { return now }

	if res := cycle(t, j); res.Admitted != 1 {
		t.Fatalf("first cycle admitted %d; want 1", res.Admitted)
	}
	q := queue.NewDBQueue(dbh)
	if _, err := dbh.Exec(`UPDATE work_queue SET status = 'processing' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	released, err := q.DeferWordRecheck(context.Background(), 1, time.Hour, 0, "lane down")
	if err != nil || !released {
		t.Fatalf("un-flip = %v, %v; want released", released, err)
	}
	for i := range 3 {
		now = now.Add(time.Hour)
		if res := cycle(t, j); res.Admitted != 0 {
			t.Fatalf("cycle %d re-admitted the un-flipped row", i+2)
		}
	}
	now = now.Add(wordRecheckReadmitCooldown)
	if res := cycle(t, j); res.Admitted != 1 {
		t.Fatalf("after the cooldown admitted %d; want the row back", res.Admitted)
	}
}

// TestWordRecheckSweepToleratesAScanReopen (#1039/#1047): a scan collision
// reopens a sweep-queued row for an ordinary fetch. Its slot frees, and once the
// ordinary fetch settles it the flip's stamp holds it out rather than the next
// cycle re-queuing it (the churn a frequently rescanned library would cause).
func TestWordRecheckSweepToleratesAScanReopen(t *testing.T) {
	ctx := context.Background()
	dbh := wordSweepDB(t, 1)
	j := wordSweepJob(t, dbh, wordSweepCfg(true, 5), fixedGen(1))
	if res := cycle(t, j); res.Admitted != 1 {
		t.Fatalf("first cycle admitted %d; want 1", res.Admitted)
	}
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := queue.ReopenDoneRowTx(ctx, tx, 1, time.Now()); err != nil || !ok {
		t.Fatalf("reopen = %v, %v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if res := cycle(t, j); res.InFlight != 0 || res.Admitted != 0 {
		t.Fatalf("while reopened = %+v; want the slot freed and nothing admitted", res)
	}
	if _, err := dbh.Exec(`UPDATE work_queue SET status = 'processing' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := queue.NewDBQueue(dbh).Complete(ctx, 1); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := dbh.Exec(`UPDATE work_queue SET outcome_type = 'synced' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if res := cycle(t, j); res.Admitted != 0 {
		t.Fatal("the ordinarily re-settled row was re-queued on the next cycle")
	}
}

// TestWordRecheckSweepGenerationFromLiveLanes: the generation is the worker's,
// from the lanes serve built, so adding a word-capable lane re-opens 'absent'
// verdicts stamped under the smaller set and leaves current ones alone.
func TestWordRecheckSweepGenerationFromLiveLanes(t *testing.T) {
	w := worker.New(nil, nil, providers.New(providers.Musixmatch, noopFetcher{}), nil)
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, noopFetcher{}))
	live := providers.WordGeneration([]string{providers.Musixmatch, providers.PetitLyrics})
	if w.WordGeneration() != live {
		t.Fatalf("worker generation %d; want the live lane set's %d", w.WordGeneration(), live)
	}
	stale := providers.WordGeneration([]string{providers.Musixmatch})

	dbh := wordSweepDB(t, 2)
	if _, err := dbh.Exec(`UPDATE work_queue SET word_timing_state = 'absent', word_timing_generation = CASE id WHEN 1 THEN ? ELSE ? END`, stale, live); err != nil {
		t.Fatal(err)
	}
	j := wordSweepJob(t, dbh, wordSweepCfg(true, 5), w)
	if res := cycle(t, j); res.Admitted != 1 {
		t.Fatalf("admitted %d; want only the stale-generation row", res.Admitted)
	}
	var state string
	if err := dbh.QueryRow(`SELECT word_timing_state FROM work_queue WHERE id = 1`).Scan(&state); err != nil || state != queue.WordTimingQueued {
		t.Fatalf("row 1 state = %q (%v); want queued", state, err)
	}
}

// TestRunWordRecheckSweepLoopRunsAtStartupAndStopsOnCancel: the first cycle
// runs before any tick, and cancel ends the loop.
func TestRunWordRecheckSweepLoopRunsAtStartupAndStopsOnCancel(t *testing.T) {
	dbh := wordSweepDB(t, 2)
	j := wordSweepJob(t, dbh, wordSweepCfg(true, 5), fixedGen(1))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runWordRecheckSweepLoop(ctx, j, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for countQueued(t, dbh) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("startup cycle never admitted the candidates")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop on cancel")
	}
}
