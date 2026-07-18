package watcher

import (
	"errors"
	"fmt"
	"syscall"
)

// annotateWatchErr explains an inotify watch-quota failure in terms an operator
// can act on, and passes every other error through untouched.
//
// The kernel reports an exhausted inotify watch quota as ENOSPC, which
// stringifies as "no space left on device". On a media server with terabytes
// free that reads as a disk problem, and the resulting investigation goes
// looking at storage rather than at a sysctl. The raw error is technically
// accurate and practically misleading, which is the worst combination.
//
// Two details the message carries deliberately:
//
//   - fs.inotify.max_user_watches is a per-UID kernel limit, NOT a per-process
//     or per-container one. Every container running as the same uid draws from
//     one pool, so this process can be refused a watch purely because a
//     different one exhausted the quota first. A container CPU or memory limit
//     cannot prevent it, which is a common wrong turn.
//   - the watch count this process asked for, so the reader can immediately see
//     whether this process is the greedy one or an innocent bystander.
//
// The original error is wrapped, not replaced, so errors.Is still matches.
func annotateWatchErr(err error, dirs int) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.ENOSPC) {
		return err
	}
	return fmt.Errorf(
		"%w -- inotify watch limit reached while registering %d directories. "+
			"This is not a disk-space problem despite the errno. "+
			"fs.inotify.max_user_watches is a per-UID kernel limit shared by every "+
			"process running as this user, so another container may have consumed it; "+
			"a container CPU/memory cap cannot prevent this. "+
			"Raise fs.inotify.max_user_watches, narrow the configured library roots, "+
			"or lower %s to keep this process within budget",
		err, dirs, EnvMaxDirs)
}
