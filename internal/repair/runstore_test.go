package repair

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func count(s Snapshot, key CountKey) (int64, bool) {
	for _, c := range s.Counts {
		if c.Key == key {
			return c.N, true
		}
	}
	return 0, false
}

func TestMemStoreIgnoresAnEndedRun(t *testing.T) {
	var m MemStore
	id1, err := m.Begin(Snapshot{Key: "settle-tiers"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Begin(Snapshot{Key: "tag-reread"}); !errors.Is(err, ErrBusy) {
		t.Errorf("Begin while running = %v, want ErrBusy", err)
	}
	m.Finish(id1, Snapshot{Key: "settle-tiers", State: StateDone})
	id2, err := m.Begin(Snapshot{Key: "tag-reread"})
	if err != nil || id2 == id1 {
		t.Fatalf("Begin after Finish = %d, %v", id2, err)
	}
	m.Progress(id1, 9, 9)
	m.Finish(id1, Snapshot{Key: "settle-tiers", State: StateFailed})
	if s := m.Load(); s.ID != id2 || s.State != StateRunning || s.Done != 0 {
		t.Errorf("an ended run changed the next one: %+v", s)
	}
}

// TestSnapshotHasNoFreeText: every leaf of a Snapshot is a fixed identifier,
// a number, a flag or a time, so it has nowhere to carry a path or a name.
func TestSnapshotHasNoFreeText(t *testing.T) {
	allowed := map[reflect.Type]bool{
		reflect.TypeFor[Key]():       true,
		reflect.TypeFor[State]():     true,
		reflect.TypeFor[Failure]():   true,
		reflect.TypeFor[CountKey]():  true,
		reflect.TypeFor[int64]():     true,
		reflect.TypeFor[float64]():   true,
		reflect.TypeFor[bool]():      true,
		reflect.TypeFor[time.Time](): true,
	}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		switch {
		case allowed[typ]:
		case typ.Kind() == reflect.Slice:
			walk(path+"[]", typ.Elem())
		case typ.Kind() == reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				walk(path+"."+typ.Field(i).Name, typ.Field(i).Type)
			}
		default:
			t.Errorf("Snapshot%s is a %s, which can carry free text", path, typ)
		}
	}
	walk("", reflect.TypeFor[Snapshot]())
}

func TestMemStoreLoadCopiesCounts(t *testing.T) {
	var m MemStore
	id, _ := m.Begin(Snapshot{})
	m.Finish(id, Snapshot{State: StateDone, Counts: []Count{{Key: "checked", N: 1}}})
	m.Load().Counts[0].N = 99
	if n, _ := count(m.Load(), "checked"); n != 1 {
		t.Errorf("a caller's write reached the store: checked = %d", n)
	}
}
