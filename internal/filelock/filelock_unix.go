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
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return noop, false
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, true
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
}
