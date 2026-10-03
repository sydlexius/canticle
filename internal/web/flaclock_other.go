//go:build !linux && !darwin

package web

import "os"

// Windows (and any platform without flock here) takes no liveness lock and
// never sweeps: a dir left by a process that died without Close stays until
// the OS temp cleanup removes it. Removing a sibling's dir there could not tell
// a dead one from a live one.

func lockFlacDir(string) (*os.File, error) { return nil, nil }

func flacDirDead(string) bool { return false }
