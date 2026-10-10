//go:build !windows

package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func acquire(lockPath string, exclusive bool, wait time.Duration) (func(), bool) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return noop, false
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return noop, false
		}
		if testHookAfterOpen != nil {
			testHookAfterOpen()
		}
		for {
			err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
				_ = f.Close()
				return noop, false
			}
			if !time.Now().Before(deadline) {
				_ = f.Close()
				return noop, false
			}
			time.Sleep(pollInterval)
		}
		// We hold a lock on the file we opened — but RemoveIfUnlocked may have
		// unlinked that path between our open and our flock, in which case
		// we'd hold a lock on an orphaned inode while another process locks a
		// freshly created file at the same path. Only trust the lock if the
		// path still names our inode; otherwise reopen and retry.
		if samePath(f, lockPath) {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, true
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		if !time.Now().Before(deadline) {
			return noop, false
		}
	}
}

// testHookAfterOpen, when set by tests, runs between opening the lock file and
// flocking it — the window a concurrent RemoveIfUnlocked can hit.
var testHookAfterOpen func()

// samePath reports whether lockPath currently names the inode f has open.
func samePath(f *os.File, lockPath string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	cur, err := os.Stat(lockPath)
	if err != nil {
		return false
	}
	return os.SameFile(held, cur)
}
