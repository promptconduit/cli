//go:build windows

package outbound

import (
	"os"
	"sync"
)

// On Windows we don't have a portable cross-process advisory lock in the
// stdlib, so we serialize within a single process and accept best-effort
// behavior across processes. In practice the only concurrent writers are
// the foreground CLI process and the `hook --send-event` subprocess it
// spawns, which fire on different cadences, so collisions are very rare.
var inProcessAppendMu sync.Mutex

func appendLine(path string, line []byte) error {
	inProcessAppendMu.Lock()
	defer inProcessAppendMu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, len(line)+1)
	copy(buf, line)
	buf[len(line)] = '\n'
	_, err = f.Write(buf)
	return err
}
