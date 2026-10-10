//go:build windows

package filelock

import "time"

// Windows: no portable advisory lock in the stdlib. Report success so callers
// keep their previous (unlocked, best-effort) behaviour.
func acquire(lockPath string, exclusive bool, wait time.Duration) (func(), bool) {
	return noop, true
}
