//go:build !windows

package client

import "os"

// createEphemeralTemp returns an open temp file that has already been removed
// from the filesystem.
//
// On unix an inode outlives its directory entry for as long as any descriptor
// stays open, so unlinking immediately is safe: the parent still has the file,
// the child inherits a descriptor to it across exec, and the kernel reclaims
// the space once both are done. There is deliberately no cleanup path and
// nothing to leak — not even if either process crashes mid-send.
func createEphemeralTemp(prefix string) (*os.File, error) {
	f, err := os.CreateTemp("", prefix)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
