package commands

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/repair"
	"github.com/sydlexius/canticle/internal/server"
)

// blockingRepair blocks in Run until its context is canceled.
type blockingRepair struct {
	runs    atomic.Int32
	started chan struct{}
}

func (b *blockingRepair) Info() repair.Info {
	return repair.Info{Key: "block-fix", Title: "Block fix"}
}

func (b *blockingRepair) Pending(context.Context, repair.Options) (int64, error) { return 0, nil }

func (b *blockingRepair) Run(ctx context.Context, _ repair.Options) (repair.Result, error) {
	b.runs.Add(1)
	close(b.started)
	<-ctx.Done()
	return repair.NewResult(), fmt.Errorf("blocked: %w", ctx.Err())
}

// TestServeRepairRegistryShipsEmpty pins the set of repairs serve registers.
// It is empty today; each tenant slice (#1546, #1548, #1554) that registers a
// repair in serveRepairRegistry updates the expected set here.
func TestServeRepairRegistryShipsEmpty(t *testing.T) {
	var want []repair.Key
	if keys := serveRepairRegistry().Keys(); len(keys) != len(want) {
		t.Fatalf("serve registers %d repairs (%v), want %d", len(keys), keys, len(want))
	}
	s := newServeRepairs(serveRepairRegistry())
	if st := s.runner.Snapshot().State; st != repair.StateIdle {
		t.Fatalf("state at boot = %q, want idle", st)
	}
	if _, err := s.runner.Start("block-fix", repair.Options{}); err == nil {
		t.Fatal("Start of an unregistered key succeeded")
	}
}

// TestServeRepairsOptionMountsRoutes proves option hands a working registry
// and runner to the web UI: with a registered repair the confirm route
// answers 200, which it cannot do if option drops either one.
func TestServeRepairsOptionMountsRoutes(t *testing.T) {
	reg := repair.NewRegistry()
	if err := reg.Register(&blockingRepair{started: make(chan struct{})}); err != nil {
		t.Fatal(err)
	}
	s := newServeRepairs(reg)
	h := server.NewHandler(&serveTestAuth{}, nil, t.TempDir(),
		server.WithWebUI(config.Config{}, "vtest"), s.option(""))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repairs/block-fix", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /repairs/block-fix = %d, want 200", rec.Code)
	}
}

func TestServeRepairsNothingRunsAtBoot(t *testing.T) {
	reg := repair.NewRegistry()
	b := &blockingRepair{started: make(chan struct{})}
	if err := reg.Register(b); err != nil {
		t.Fatal(err)
	}
	s := newServeRepairs(reg)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	s.closeOnDone(ctx, &wg)
	cancel()
	wg.Wait()
	if n := b.runs.Load(); n != 0 {
		t.Fatalf("a registered repair ran %d times without a Start", n)
	}
}

func TestServeRepairsShutdownCancelsLiveRun(t *testing.T) {
	reg := repair.NewRegistry()
	b := &blockingRepair{started: make(chan struct{})}
	if err := reg.Register(b); err != nil {
		t.Fatal(err)
	}
	s := newServeRepairs(reg)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	s.closeOnDone(ctx, &wg)
	if _, err := s.runner.Start("block-fix", repair.Options{}); err != nil {
		t.Fatal(err)
	}
	<-b.started
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("shutdown did not return within the bound after cancel")
	}
	if st := s.runner.Snapshot().State; st == repair.StateRunning {
		t.Fatalf("run still %q after shutdown", st)
	}
}
