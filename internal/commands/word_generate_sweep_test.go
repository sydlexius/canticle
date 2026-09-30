package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
)

const genSweepWordGen = 5

// fakeGenerator records what it was offered and reports handle(ids) handled.
type fakeGenerator struct {
	version int64
	offered [][]int64
	handle  func([]int64) []int64
	err     error
}

func (f *fakeGenerator) Version() int64 { return f.version }

func (f *fakeGenerator) Generate(_ context.Context, ids []int64) ([]int64, error) {
	f.offered = append(f.offered, slices.Clone(ids))
	return f.handle(ids), f.err
}

// genSweepDB seeds n line-arm candidates (ids 1..n).
func genSweepDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	dbh, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "gs.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	for i := 1; i <= n; i++ {
		if _, err := dbh.Exec(`INSERT INTO work_queue (id, artist, title, artist_key, title_key, source_path, status, outcome_type, sync_tier,
		      word_timing_state, word_timing_generation, completed_at)
		      VALUES (?, 'a', ?, 'a', ?, '/m/x.flac', 'done', 'synced', 'line', 'absent', ?, '2026-01-01T00:00:00Z')`,
			i, fmt.Sprint(i), fmt.Sprint(i), genSweepWordGen); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	return dbh
}

func genSweepCfg(budget int) config.Config {
	cfg := config.Config{}
	cfg.WordSyncGenerate.Enabled = true
	cfg.WordSyncGenerate.URL = "http://aligner.invalid:9"
	cfg.WordSyncGenerate.BudgetPerCycle = budget
	return cfg
}

func stampedCount(t *testing.T, dbh *sql.DB) int {
	t.Helper()
	var n int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE word_generate_version IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWordGenerateSweep_RefusesWithoutConsumer: disabled, a blank URL, and
// (the only production state until #1008) no generator all refuse at
// construction, so nothing is selected or stamped; the no-consumer refusal says why.
func TestWordGenerateSweep_RefusesWithoutConsumer(t *testing.T) {
	dbh := genSweepDB(t, 2)
	gen := &fakeGenerator{version: 1, handle: func(ids []int64) []int64 { return ids }}
	off := genSweepCfg(10)
	off.WordSyncGenerate.Enabled = false
	blank := genSweepCfg(10)
	blank.WordSyncGenerate.URL = "  "
	for name, c := range map[string]struct {
		cfg config.Config
		gen wordSyncGenerator
	}{"disabled": {off, gen}, "blank-url": {blank, gen}, "no-generator": {genSweepCfg(10), nil}} {
		buf := captureSlog(t)
		if _, ok := newWordGenerateSweepJob(dbh, c.cfg, fixedGen(genSweepWordGen), c.gen); ok {
			t.Fatalf("%s: sweep started", name)
		}
		if name == "no-generator" && !strings.Contains(buf.String(), "generation has no consumer yet") {
			t.Fatalf("no-generator refusal not logged: %q", buf.String())
		}
	}
	if len(gen.offered) != 0 || stampedCount(t, dbh) != 0 {
		t.Fatalf("offered %v, stamped %d; want nothing", gen.offered, stampedCount(t, dbh))
	}
}

// TestWordGenerateSweep_StampsOnlyHandled: a cycle offers at most the budget,
// stamps only what the generator reports handled (even beside an error, and
// never an id it was not offered), and (review F3) re-offers an unhandled row
// only behind rows not yet offered, so at budget 1 it cannot hold the slot.
func TestWordGenerateSweep_StampsOnlyHandled(t *testing.T) {
	ctx := context.Background()
	dbh := genSweepDB(t, 3)
	gen := &fakeGenerator{version: 2, err: errors.New("aligner busy"),
		handle: func(ids []int64) []int64 { return []int64{ids[0], 3} }} // 3 exists but was not offered
	j, ok := newWordGenerateSweepJob(dbh, genSweepCfg(2), fixedGen(genSweepWordGen), gen)
	if !ok {
		t.Fatal("sweep did not start")
	}
	n, err := j.runCycle(ctx)
	if n != 1 || err == nil || !strings.Contains(err.Error(), "aligner busy") {
		t.Fatalf("cycle 1 = %d, %v; want 1 stamped and the generator error", n, err)
	}
	if !slices.Equal(gen.offered[0], []int64{1, 2}) || stampedCount(t, dbh) != 1 {
		t.Fatalf("offered %v, stamped %d; want [1 2] and 1", gen.offered[0], stampedCount(t, dbh))
	}
	gen.err, gen.handle, j.budget = nil, func([]int64) []int64 { return nil }, 1
	for range 2 {
		if n, err := j.runCycle(ctx); n != 0 || err != nil {
			t.Fatalf("cycle = %d, %v", n, err)
		}
	}
	if got := gen.offered[1:]; !slices.Equal(got[0], []int64{3}) || !slices.Equal(got[1], []int64{2}) {
		t.Fatalf("budget-1 cycles offered %v; want [3] then [2] (2 was offered in cycle 1)", got)
	}
}

// TestRunWordGenerateSweepLoop: the first cycle runs before any tick, ctx
// ending inside Generate still stamps what it handled, and ends the loop.
func TestRunWordGenerateSweepLoop(t *testing.T) {
	dbh := genSweepDB(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gen := &fakeGenerator{version: 1, handle: func(ids []int64) []int64 { cancel(); return ids }}
	j, ok := newWordGenerateSweepJob(dbh, genSweepCfg(5), fixedGen(genSweepWordGen), gen)
	if !ok {
		t.Fatal("sweep did not start")
	}
	runWordGenerateSweepLoop(ctx, j, time.Hour) // returns only because ctx ended
	if got := stampedCount(t, dbh); got != 2 {
		t.Fatalf("startup cycle stamped %d rows; want 2 despite the cancel", got)
	}
}
