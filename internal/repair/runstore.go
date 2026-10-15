package repair

import (
	"errors"
	"sync"
	"time"
)

// ErrBusy is the refusal of a Start while a run is live.
var ErrBusy = errors.New("repair: a run is already in progress")

// State is a run's state.
type State string

// The states of a run. A runner that has never started one reports idle.
const (
	StateIdle     State = "idle"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateCanceled State = "canceled"
	StateFailed   State = "failed"
)

// Failure is a fixed code saying why a run failed. It replaces the error's
// text, which may hold a library path and is never recorded.
type Failure string

// The failure codes. A run that did not fail has none.
const (
	FailureNone          Failure = ""
	FailureError         Failure = "error"
	FailurePanic         Failure = "panic"
	FailureInvalidResult Failure = "invalid_result"
)

// Snapshot is the state of the current or last run. It carries counts and
// fixed identifiers only, never a path, artist, title, lyric line or error
// text.
type Snapshot struct {
	// ID tells runs apart; it is assigned by the RunStore at Begin.
	ID      int64
	Key     Key
	State   State
	Failure Failure
	Apply   bool
	// Done and Total are the last progress report (Total <= 0: unknown).
	Done, Total int64
	// Rate is Done per second since Started, up to Finished for a run that has
	// ended. Computed by Runner.Snapshot; a RunStore does not store it.
	Rate float64
	// Counts are the run's final counts once it has ended: partial for a
	// canceled run, and for a failed run that returned any; none after a panic
	// or a Result that failed Check.
	Counts   []Count
	Started  time.Time
	Finished time.Time
}

// RunStore holds run state. MemStore keeps it in memory; a durable jobs table
// (#979) can implement the same methods. It admits one running run at a time,
// and every write after Begin names the run's id, so a run that has ended
// cannot change a newer one.
//
// A durable store must, when it is opened, mark a row a dead process left
// "running" as failed, or Begin answers ErrBusy forever and nothing can clear
// it. The runner calls Begin and Finish under its lock, so a slow store delays
// Cancel and Close: keep both short.
type RunStore interface {
	// Begin records s as the running run and returns its id, or ErrBusy when
	// a run is recorded running. It replaces the previous run's snapshot.
	Begin(s Snapshot) (int64, error)
	// Progress records done of total for run id. It is called from the run's
	// goroutine and must not block (a durable store throttles its writes).
	Progress(id, done, total int64)
	// Finish records run id's outcome; s is its final snapshot, including the
	// last Done and Total the runner relayed, so a store that throttled
	// Progress still keeps the final progress.
	Finish(id int64, s Snapshot)
	// Load returns the current or last run, State idle when there is none.
	Load() Snapshot
}

// MemStore is the in-memory RunStore. State survives until the next Begin,
// not a restart. The zero value is ready to use.
type MemStore struct {
	mu     sync.Mutex
	lastID int64
	cur    Snapshot
}

// Begin implements RunStore.
func (m *MemStore) Begin(s Snapshot) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur.State == StateRunning {
		return 0, ErrBusy
	}
	m.lastID++
	s.ID, s.State = m.lastID, StateRunning
	m.cur = s
	return s.ID, nil
}

// Progress implements RunStore.
func (m *MemStore) Progress(id, done, total int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur.ID == id && m.cur.State == StateRunning {
		m.cur.Done, m.cur.Total = done, total
	}
}

// Finish implements RunStore.
func (m *MemStore) Finish(id int64, s Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur.ID == id && m.cur.State == StateRunning {
		s.ID = id
		s.Counts = append([]Count(nil), s.Counts...)
		m.cur = s
	}
}

// Load implements RunStore.
func (m *MemStore) Load() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur
	if s.State == "" {
		s.State = StateIdle
	}
	s.Counts = append([]Count(nil), s.Counts...)
	return s
}
