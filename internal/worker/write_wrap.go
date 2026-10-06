package worker

import (
	"io/fs"
	"os"
	"strings"
)

// pathFreeError reports an OS error without the library path it embeds. The
// Unwrap chain is kept so errors.Is/As (ErrKeptBetter, syscall errnos) still
// work; only the recorded message changes (#1336).
type pathFreeError struct {
	msg string
	err error
}

func (e *pathFreeError) Error() string { return e.msg }
func (e *pathFreeError) Unwrap() error { return e.err }

// scrubWritePaths rebuilds each *fs.PathError, *os.LinkError and
// *os.SyscallError in err's chain as "op: cause" so a filename or directory
// (which may contain ": ") never reaches last_error or a /metrics label. The
// full path stays in the caller's Warn log attributes.
func scrubWritePaths(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	var walk func(error)
	walk = func(cur error) {
		switch e := cur.(type) {
		case *fs.PathError:
			msg = strings.Replace(msg, e.Error(), e.Op+": "+e.Err.Error(), 1)
		case *os.LinkError:
			msg = strings.Replace(msg, e.Error(), e.Op+": "+e.Err.Error(), 1)
		}
		// Both Unwrap shapes: errors.Join and multi-%w wraps expose []error.
		switch u := cur.(type) {
		case interface{ Unwrap() error }:
			if inner := u.Unwrap(); inner != nil {
				walk(inner)
			}
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				if inner != nil {
					walk(inner)
				}
			}
		}
	}
	walk(err)
	if msg == err.Error() {
		return err
	}
	return &pathFreeError{msg: msg, err: err}
}
