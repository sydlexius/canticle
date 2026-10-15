package repair

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrDuplicateKey reports that a second repair was registered under a key already
	// taken.
	ErrDuplicateKey = errors.New("repair: duplicate key")
	// ErrNilRepair reports that Register was given a nil Repair.
	ErrNilRepair = errors.New("repair: nil repair")
)

// Registry maps each repair's Key to it. Its owner (serve) builds it and
// injects it where it is needed; there is no package-level registry. It is
// safe for concurrent use, and the zero value is an empty Registry.
type Registry struct {
	mu    sync.RWMutex
	byKey map[Key]Repair
	order []Key
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds r under its Info().Key. A nil repair, an Info that fails
// Validate, or a key already registered is an error and leaves the Registry
// unchanged.
func (g *Registry) Register(r Repair) error {
	if r == nil {
		return ErrNilRepair
	}
	info := r.Info()
	if err := info.Validate(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.byKey[info.Key]; dup {
		return fmt.Errorf("%w: %s", ErrDuplicateKey, info.Key)
	}
	if g.byKey == nil {
		g.byKey = make(map[Key]Repair)
	}
	g.byKey[info.Key] = r
	g.order = append(g.order, info.Key)
	return nil
}

// Get returns the repair registered under key, and false when there is none.
func (g *Registry) Get(key Key) (Repair, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r, ok := g.byKey[key]
	return r, ok
}

// Keys returns the registered keys in registration order.
func (g *Registry) Keys() []Key {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]Key(nil), g.order...)
}
