// Package repair is the contract every operator fix implements so the CLI and
// the serve web UI can start it through one implementation: a Repair reports
// what it would change (Pending), runs with the same Options, and returns a Result of
// named counts. A Registry, owned and injected by its caller, maps each
// repair's Key to it.
//
// This is a leaf package: it imports nothing from this module, so internal/web
// and internal/commands can both depend on it without an import cycle.
//
// Rules every implementation keeps:
//   - Nothing runs unasked. A repair runs only when an operator starts it,
//     never on upgrade, boot or a timer.
//   - Counts only. A Result and a progress report carry counts and fixed
//     identifiers, never a path, artist, title or lyric line.
//   - Errors are not display text. An error from Pending or Run may wrap an os
//     error whose text holds a library path, so a caller (the CLI, the serve
//     runner, a web handler) records a failure state plus the repair's Key and
//     never logs or displays the text of a Pending or Run error.
//   - All of serve shares one database connection, so a run holds no cursor
//     or transaction across file I/O.
package repair

import (
	"context"
	"errors"
	"fmt"
)

// maxIdentLen bounds a Key or a CountKey.
const maxIdentLen = 48

var (
	// ErrInvalidInfo reports that a repair's Info failed Validate.
	ErrInvalidInfo = errors.New("repair: invalid info")
	// ErrInvalidResult reports that a Result carries a count key that is malformed or
	// that the repair's Info did not declare.
	ErrInvalidResult = errors.New("repair: invalid result")
	// ErrBackupRequired reports that an applying run of a repair that needs a backup
	// was given no backup path.
	ErrBackupRequired = errors.New("repair: a backup path is required to apply")
)

// Key identifies a repair: a lowercase slug of letters, digits and hyphens,
// starting with a letter. It is used as a URL path segment.
type Key string

// Valid reports whether k is a well-formed slug.
func (k Key) Valid() bool { return validIdent(string(k), '-') }

// CountKey is a fixed identifier for one count in a Result: lowercase letters,
// digits and underscores, starting with a letter. It cannot hold a path
// separator, a dot, a space or a capital, and a repair declares its keys as
// constants in Info.Counts, so a count is never keyed by library data.
type CountKey string

// Valid reports whether c is a well-formed identifier.
func (c CountKey) Valid() bool { return validIdent(string(c), '_') }

// validIdent: s is 1..maxIdentLen of [a-z0-9] plus sep, starting with [a-z].
func validIdent(s string, sep byte) bool {
	if s == "" || len(s) > maxIdentLen || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		b := s[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != sep {
			return false
		}
	}
	return true
}

// Info describes a repair. Every string here is static text the repair's
// author writes, never data read from the library or the database.
type Info struct {
	// Key is the registry key and URL slug.
	Key Key
	// Title is the short name shown on its prompt and run page.
	Title string
	// Description says what the repair changes.
	Description string
	// CostNote, when set, says what a run costs beyond its pending count
	// (for example, that it walks the whole library).
	CostNote string
	// ReadsFiles is true when a run (dry run included) reads library files,
	// which may wake a spun-down array; the UI shows a cost warning.
	ReadsFiles bool
	// NeedsBackup is true when an applying run writes a backup first and
	// refuses to apply without a backup path (see CheckOptions).
	NeedsBackup bool
	// Counts declares, in display order, every count key a Result of this
	// repair may carry.
	Counts []CountKey
}

// Validate reports a malformed Info: an invalid Key, an empty Title, or a
// count key that is invalid or declared twice.
func (i Info) Validate() error {
	if !i.Key.Valid() {
		return fmt.Errorf("%w: key %q is not a lowercase slug", ErrInvalidInfo, i.Key)
	}
	if i.Title == "" {
		return fmt.Errorf("%w: %s has no title", ErrInvalidInfo, i.Key)
	}
	seen := make(map[CountKey]bool, len(i.Counts))
	for _, c := range i.Counts {
		if !c.Valid() {
			return fmt.Errorf("%w: %s declares an invalid count key", ErrInvalidInfo, i.Key)
		}
		if seen[c] {
			return fmt.Errorf("%w: %s declares count key %s twice", ErrInvalidInfo, i.Key, c)
		}
		seen[c] = true
	}
	return nil
}

// declares reports whether c is one of i.Counts.
func (i Info) declares(c CountKey) bool {
	for _, d := range i.Counts {
		if d == c {
			return true
		}
	}
	return false
}

// Progress receives a running repair's progress: done items of total. A
// total <= 0 means the total is not known yet. It is called from the run's
// goroutine and must not block.
type Progress func(done, total int64)

// Options control one run, and the Pending count shown before it: a caller
// passes the same Options to Pending and then to Run, so the count an
// operator confirms and the run that follows act on the same rows.
//
// Options grows a field when a repair needs a parameter (for example a
// cutoff or a row limit). A new field's zero value must mean "no parameter",
// so adding one breaks no caller, and a repair ignores the fields it does not
// use. Typed fields are preferred over an opaque key/value bag: the compiler
// checks them, and there are no parameter names to validate or to leak.
type Options struct {
	// Apply writes changes; false is a dry run that only counts.
	Apply bool
	// Backup is the file an applying run writes its backup to, resolved by
	// the caller (the CLI's --backup flag, or a default beside the
	// database). Ignored by a dry run and by a repair that needs no backup.
	Backup string
	// Progress, when non-nil, receives progress reports.
	Progress Progress
}

// Report forwards to o.Progress when it is set.
func (o Options) Report(done, total int64) {
	if o.Progress != nil {
		o.Progress(done, total)
	}
}

// CheckOptions refuses an applying run of a repair that needs a backup when
// no backup path is given. A caller checks before starting the run.
func CheckOptions(info Info, o Options) error {
	if o.Apply && info.NeedsBackup && o.Backup == "" {
		return fmt.Errorf("%w (%s)", ErrBackupRequired, info.Key)
	}
	return nil
}

// Repair is one operator fix.
type Repair interface {
	// Info describes the repair. It returns the same value on every call.
	Info() Info
	// Pending counts what a Run with the same Options would act on, from the
	// database alone: it opens no library file, so it runs with the library
	// root absent and never wakes a spun-down array. It ignores o.Apply,
	// o.Backup and o.Progress.
	Pending(ctx context.Context, o Options) (int64, error)
	// Run performs the repair (a dry run unless o.Apply) and returns its
	// counts. When ctx is canceled it stops at the next row boundary and
	// returns the partial counts with an error wrapping ctx.Err(); rows
	// already committed stay committed and no row is left half-applied.
	Run(ctx context.Context, o Options) (Result, error)
}

// Count is one named count in a Result.
type Count struct {
	Key CountKey
	N   int64
}

// Result is a run's outcome: an ordered list of counts. Its only text is
// CountKey values, and Add stores any CountKey, so the guarantee that a
// Result is keyed only by the repair's declared identifiers comes from Check:
// a runner MUST call Check before it shows or stores a Result. The zero value
// is an empty Result.
//
// A copied Result shares its counts with the original, so treat a returned
// Result as read-only, as Info.Counts (which aliases the repair's own
// declaration) is.
type Result struct {
	counts []Count
}

// NewResult returns a Result holding a zero count for each key, in order, so
// a count the run never touched is still reported. Pass the repair's
// Info.Counts.
func NewResult(keys ...CountKey) Result {
	r := Result{counts: make([]Count, 0, len(keys))}
	for _, k := range keys {
		r.Add(k, 0)
	}
	return r
}

// Add adds n to the count for key, appending the key at the end when the
// Result does not hold it yet.
func (r *Result) Add(key CountKey, n int64) {
	for i := range r.counts {
		if r.counts[i].Key == key {
			r.counts[i].N += n
			return
		}
	}
	r.counts = append(r.counts, Count{Key: key, N: n})
}

// Get returns the count for key and whether the Result holds it.
func (r Result) Get(key CountKey) (int64, bool) {
	for _, c := range r.counts {
		if c.Key == key {
			return c.N, true
		}
	}
	return 0, false
}

// Counts returns a copy of the counts in order.
func (r Result) Counts() []Count {
	return append([]Count(nil), r.counts...)
}

// Check reports a count key that is malformed or that info does not
// declare, so a Result is keyed only by the repair's fixed identifiers. The
// error names the repair, never the offending key, which may be data.
func (r Result) Check(info Info) error {
	for i, c := range r.counts {
		if !c.Key.Valid() || !info.declares(c.Key) {
			return fmt.Errorf("%w: %s count %d has an undeclared key", ErrInvalidResult, info.Key, i)
		}
	}
	return nil
}
