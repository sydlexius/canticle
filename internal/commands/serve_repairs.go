package commands

import (
	"context"
	"sync"

	"github.com/sydlexius/canticle/internal/repair"
	"github.com/sydlexius/canticle/internal/server"
	"github.com/sydlexius/canticle/internal/web"
)

// serveRepairs hosts the operator repair registry and its one-at-a-time runner
// for serve (#1545, epic #1537). The registry ships empty: nothing is
// registered, so no repair can start, and nothing starts at boot, on upgrade
// or on a timer. A run begins only when an operator posts to /repairs/{key}.
type serveRepairs struct {
	reg    *repair.Registry
	runner *repair.Runner
}

// serveRepairRegistry builds the registry serve hosts. It ships empty: no
// repair is registered yet. This is the one place a tenant slice adds its
// registration (#1546, #1548, #1554); each slice adds the dependencies its
// repair needs (the opened database, cfg) as parameters here and passes them
// from runServe, and updates the expected key set in its test.
func serveRepairRegistry() *repair.Registry {
	return repair.NewRegistry()
}

// newServeRepairs builds the runner over reg with the default in-memory run
// store. The caller owns reg; runServe passes serveRepairRegistry().
func newServeRepairs(reg *repair.Registry) *serveRepairs {
	return &serveRepairs{reg: reg, runner: repair.NewRunner(reg, nil)}
}

// option hands the registry and runner to the web UI. dbPath is the folder an
// applying run's backup goes to.
func (s *serveRepairs) option(dbPath string) server.Option {
	return server.WithRepairs(web.RepairDeps{Registry: s.reg, Runner: s.runner, DBPath: dbPath})
}

// closeOnDone cancels a live run once ctx (serve's run context) is done and
// waits for it, on wg so serve's own wait covers it. The wait is the Runner's
// close bound (5s), running alongside (not inside) the 10s HTTP drain: a
// run honors its cancel at the next row boundary, so 5s is ample, and a run
// that ignores it is logged and abandoned rather than hanging shutdown.
func (s *serveRepairs) closeOnDone(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		// Close logs its own timeout warning and returns no other error, so
		// there is nothing left to report here (and no error text to log).
		_ = s.runner.Close()
	}()
}
