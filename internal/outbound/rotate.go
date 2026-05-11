package outbound

import (
	"errors"
	"os"
)

// DefaultRotateAt is the file-size threshold (in bytes) at which we
// rotate outbound.ndjson to outbound.ndjson.1 and start fresh. One
// backup is kept; older content is overwritten.
const DefaultRotateAt int64 = 50 * 1024 * 1024 // 50 MB

// truncateBody clips b to at most max bytes. Returns the (possibly
// shorter) bytes, a flag indicating whether truncation happened, and
// the original length so the consumer can show "[truncated, was 10MB]".
//
// Pure function, kept out of mirror.go so it is easy to test without
// any disk or HTTP plumbing.
func truncateBody(b []byte, max int) ([]byte, bool, int) {
	if max <= 0 || len(b) <= max {
		return b, false, len(b)
	}
	return b[:max], true, len(b)
}

// rotateIfNeeded renames path -> path+".1" when the file is at or above
// max bytes. Missing files and missing prior backups are not errors.
// Errors stat'ing or renaming are returned to the caller; callers
// typically log-and-continue, because rotation failure should not
// silently drop traffic.
func rotateIfNeeded(path string, max int64) error {
	if max <= 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Size() < max {
		return nil
	}
	backup := path + ".1"
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(path, backup)
}
