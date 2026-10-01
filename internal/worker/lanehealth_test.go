package worker

import (
	"sync"
	"testing"
)

// TestLaneHealthTracksRebuild pins that LaneHealth reads the CURRENT
// orchestrator: after a rebuild the new lane set is reported, not the lanes the
// worker was constructed with (#488).
func TestLaneHealthTracksRebuild(t *testing.T) {
	w := New(nil, nil, nil, nil)
	before := w.LaneHealth()
	if len(before) != 1 {
		t.Fatalf("fresh worker lanes = %+v; want exactly the primary", before)
	}
	w.SetFallbackProviders(&driftStubProvider{name: "petitlyrics"})
	after := w.LaneHealth()
	if len(after) != 2 || after[1].Provider != "petitlyrics" {
		t.Fatalf("lanes after rebuild = %+v; want primary then petitlyrics", after)
	}
}

// TestLaneHealthConcurrentWithRebuild reads LaneHealth while the orchestrator
// is rebuilt repeatedly. Under -race an unlocked w.orch read fails this test.
func TestLaneHealthConcurrentWithRebuild(t *testing.T) {
	w := New(nil, nil, nil, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if len(w.LaneHealth()) == 0 {
						t.Error("LaneHealth returned no lanes")
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if err := w.rebuildOrchestrator(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
