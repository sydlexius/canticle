package worker

import (
	"errors"
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
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		var orig, repl string
		switch e := cur.(type) {
		case *fs.PathError:
			orig, repl = e.Error(), e.Op+": "+e.Err.Error()
		case *os.LinkError:
			orig, repl = e.Error(), e.Op+": "+e.Err.Error()
		default:
			continue
		}
		msg = strings.Replace(msg, orig, repl, 1)
	}
	if msg == err.Error() {
		return err
	}
	return &pathFreeError{msg: msg, err: err}
}
