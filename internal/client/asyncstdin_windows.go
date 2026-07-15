//go:build windows

package client

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Win32 constants the standard syscall package doesn't export. Values are from
// the Windows SDK (fileapi.h / winnt.h) and are stable ABI.
const (
	fileFlagDeleteOnClose  = 0x04000000 // FILE_FLAG_DELETE_ON_CLOSE
	fileAttributeTemporary = 0x00000100 // FILE_ATTRIBUTE_TEMPORARY
)

// createEphemeralTemp returns an open temp file that Windows unlinks as soon as
// the last handle to it is closed.
//
// This is the Windows analogue of unlinking an open file on unix, and it needs
// a raw CreateFile to express: os.CreateTemp opens without FILE_SHARE_DELETE,
// which makes os.Remove on the still-open file fail with a sharing violation.
// The inheritable duplicate that os/exec hands the child carries the same
// delete-on-close disposition, so the file disappears once the parent and the
// child have both closed it — crashes included.
//
// FILE_ATTRIBUTE_TEMPORARY additionally hints the cache manager to avoid
// flushing the contents to disk when it can, since this file only ever exists
// to be handed to a child that reads it immediately.
func createEphemeralTemp(prefix string) (*os.File, error) {
	dir := os.TempDir()
	var lastErr error

	// CREATE_NEW fails rather than clobbering an existing file, so retry a few
	// times on the (very unlikely) name collision.
	for i := 0; i < 10; i++ {
		name := filepath.Join(dir, prefix+
			strconv.Itoa(os.Getpid())+"-"+
			strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+
			strconv.Itoa(i))

		namep, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}

		h, err := syscall.CreateFile(
			namep,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
			nil,
			syscall.CREATE_NEW,
			fileAttributeTemporary|fileFlagDeleteOnClose,
			0,
		)
		if err != nil {
			lastErr = err
			continue
		}
		return os.NewFile(uintptr(h), name), nil
	}

	return nil, lastErr
}
