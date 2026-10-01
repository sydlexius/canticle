//go:build !unix

package reports_test

import "errors"

// mkfifo is unavailable off unix; the caller skips the FIFO subtest.
func mkfifo(string) error { return errors.New("named pipes unsupported") }
