// Package metaindex holds the pieces of the `scan index-metadata` walk that
// do not belong to the command: today, the helper that fills an empty
// recording ID on a scan_results row from the tag cache (#1520). It takes an
// open database and never opens one, loads config or prints.
//
// A library path never reaches a log field or an error: every log line and
// every returned error carries a fixed reason and at most a path-free cause
// (the driver error, which does not echo a bound path), because this code is
// also run behind the web runner where output and logs carry counts only. The
// returned error is not display text.
package metaindex
