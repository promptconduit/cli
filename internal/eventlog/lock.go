package eventlog

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/filelock"
)

// Append vs. rename coordination.
//
// The pruner and LockedRewrite replace a log by renaming a rewritten temp file
// over it. A writer that opened the OLD file just before the rename would
// append into the orphaned inode and its line would be lost. To close that
// window, every appender holds a SHARED advisory lock on a sidecar lock file
// while it opens and writes, and a rewriter holds the EXCLUSIVE lock only for
// its final carry-over + rename (milliseconds; the slow fsync of the big temp
// file happens before it takes the lock).
//
// Appenders never block for long and never drop a line: after appendLockWait
// they append without the lock (the pre-lock behaviour). A rewriter that can't
// get the exclusive lock within renameLockWait abandons its pass, leaving the
// log untouched. On Windows the lock is a no-op (see internal/filelock).

const (
	appendLockWait = 500 * time.Millisecond
	renameLockWait = 3 * time.Second
)

var errAppendLockBusy = errors.New("eventlog: appenders hold the log lock; rewrite abandoned")

// appendLockPath is the sidecar lock for a log: <dir>/.<name>.lock.
func appendLockPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
}

// lockForAppend takes the shared append lock for path. The returned release is
// always safe to call; when the lock couldn't be taken in time the append goes
// ahead unlocked.
func lockForAppend(path string) func() {
	release, _ := filelock.Shared(appendLockPath(path), appendLockWait)
	return release
}

// lockAppendsForRename takes the exclusive append lock for path, holding off
// appenders while a rewrite does its final carry-over and rename.
func lockAppendsForRename(path string) (func(), bool) {
	return filelock.Exclusive(appendLockPath(path), renameLockWait)
}
