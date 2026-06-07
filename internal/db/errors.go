package db

import (
	"errors"

	sqlite "modernc.org/sqlite"
)

// sqliteBusy is SQLITE_BUSY, the result code returned when an operation could
// not acquire a needed database lock. Extended result codes (SQLITE_BUSY_*)
// carry this value in their low byte, so callers mask before comparing.
const sqliteBusy = 5

// IsSQLiteBusy reports whether err, or any error it wraps, is a SQLITE_BUSY
// error from the modernc.org/sqlite driver.
func IsSQLiteBusy(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code()&0xff == sqliteBusy
	}
	return false
}
