//go:build !windows

package outbound

import (
	"os"
	"syscall"
)

// inodeOf returns the file's inode number on Unix. Used by Tail to
// detect rotation: when the inode changes under the same path, the
// follower reopens at offset 0.
func inodeOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, nil
	}
	return uint64(st.Ino), nil
}
