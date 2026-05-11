//go:build !windows

package outbound

import (
	"os"
	"sync"
	"syscall"
)

// inProcessAppendMu serializes appendLine calls from a single process.
// Across processes we lean on flock for lines >4KB and on POSIX's
// O_APPEND atomicity guarantee for smaller writes.
var inProcessAppendMu sync.Mutex

// appendLine writes line + "\n" to path. POSIX guarantees an O_APPEND
// write of <=PIPE_BUF (typically 4KB) is atomic between processes, so
// small lines never interleave. For larger lines we take an exclusive
// file lock (flock LOCK_EX) for the duration of the write.
//
// Within a single process, inProcessAppendMu prevents tearing if two
// goroutines append concurrently — important because the parent CLI
// can fire several requests in quick succession.
func appendLine(path string, line []byte) error {
	inProcessAppendMu.Lock()
	defer inProcessAppendMu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	needFlock := len(line)+1 > 4096
	if needFlock {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
		// LOCK_UN is implicit on file close, but be explicit for clarity.
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}

	// One Write call so the kernel sees the line+newline as a unit.
	buf := make([]byte, len(line)+1)
	copy(buf, line)
	buf[len(line)] = '\n'
	_, err = f.Write(buf)
	return err
}
