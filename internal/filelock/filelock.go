// Package filelock provides small advisory cross-process locks on sidecar
// lock files. Locks are taken with a bounded, non-blocking poll so a caller on
// the hook hot path can never deadlock: when the wait elapses the caller is
// told the lock was not acquired and decides what to do (usually proceed
// best-effort, or skip optional work).
//
// The lock lives on a separate file (never on the data file itself) because
// the data files are replaced by rename, which would orphan a lock held on the
// old inode.
//
// On unix this is flock(2). Elsewhere it is a no-op that always reports
// success, matching the previous (unlocked) behaviour.
package filelock

import (
	"os"
	"time"
)

// pollInterval is how often a contended lock is retried.
const pollInterval = 2 * time.Millisecond

// noop is the release func returned when there is nothing to release.
func noop() {}

// Exclusive takes an exclusive lock on lockPath, waiting up to wait. The
// returned release func is always safe to call (a no-op when ok is false).
func Exclusive(lockPath string, wait time.Duration) (release func(), ok bool) {
	return acquire(lockPath, true, wait)
}

// RemoveIfUnlocked deletes the lock file at lockPath only if no one holds it
// (an exclusive lock is taken without waiting first). For GC of stale lock
// files, whose mtime never changes while they're in use. Reports whether the
// file was removed. On Windows (no-op locks) it simply removes the file.
func RemoveIfUnlocked(lockPath string) bool {
	release, ok := acquire(lockPath, true, 0)
	if !ok {
		return false
	}
	err := os.Remove(lockPath)
	release()
	return err == nil
}

// Shared takes a shared lock on lockPath, waiting up to wait. Many holders can
// share it; it excludes Exclusive holders. release is always safe to call.
func Shared(lockPath string, wait time.Duration) (release func(), ok bool) {
	return acquire(lockPath, false, wait)
}
