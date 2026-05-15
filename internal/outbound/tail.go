package outbound

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// TailPollInterval is how often Tail re-checks the file for new bytes
// and possible rotation. Tuned for human eyes — 200ms feels live
// without burning CPU.
const TailPollInterval = 200 * time.Millisecond

// Tail follows path the way `tail -f` does. It first delivers up to
// backfill lines from the end of the file (0 means none — start at the
// current end), then streams new lines as they appear. Detects file
// rotation (inode change on Unix, size shrink everywhere) and reopens
// the file at offset 0 when it happens.
//
// The returned channel is closed when ctx is canceled or when an
// unrecoverable I/O error occurs.
func Tail(ctx context.Context, path string, backfill int) <-chan []byte {
	out := make(chan []byte, 64)
	go func() {
		defer close(out)

		// Wait for the file to appear if it doesn't yet exist. The
		// mirror creates it lazily on first write, so a `watch`
		// invocation before any traffic has flowed is normal.
		f, err := waitForFile(ctx, path)
		if err != nil {
			return
		}
		defer f.Close()

		offset := int64(0)
		if backfill > 0 {
			off, lines, err := readLastLines(f, backfill)
			if err == nil {
				for _, l := range lines {
					if !sendLine(ctx, out, l) {
						return
					}
				}
				offset = off
			}
		} else {
			// Start at end of file.
			end, err := f.Seek(0, io.SeekEnd)
			if err == nil {
				offset = end
			}
		}

		lastInode, _ := inodeOf(path)
		lastSize := offset

		// Polling loop.
		reader := bufio.NewReader(f)
		ticker := time.NewTicker(TailPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			info, err := os.Stat(path)
			if err != nil {
				// File may have been rotated and not yet recreated;
				// keep polling, don't bail.
				continue
			}

			currentInode, _ := inodeOf(path)
			rotated := (lastInode != 0 && currentInode != 0 && currentInode != lastInode) || info.Size() < lastSize
			if rotated {
				// Reopen.
				_ = f.Close()
				f, err = os.Open(path)
				if err != nil {
					return
				}
				reader = bufio.NewReader(f)
				lastInode = currentInode
				lastSize = 0
				offset = 0
			}

			if info.Size() == offset {
				continue
			}

			// Stream any new bytes line by line.
			for {
				line, err := reader.ReadBytes('\n')
				if len(line) > 0 {
					// Drop the trailing newline before sending.
					if line[len(line)-1] == '\n' {
						line = line[:len(line)-1]
					}
					if !sendLine(ctx, out, append([]byte(nil), line...)) {
						return
					}
				}
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return
				}
			}
			offset, _ = f.Seek(0, io.SeekCurrent)
			lastSize = info.Size()
		}
	}()
	return out
}

func sendLine(ctx context.Context, out chan<- []byte, line []byte) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- line:
		return true
	}
}

// waitForFile blocks until path exists and is openable, or ctx ends.
func waitForFile(ctx context.Context, path string) (*os.File, error) {
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(TailPollInterval):
		}
	}
}

// readLastLines reads up to n trailing lines from f and returns them in
// order, along with the file offset they end at (which becomes the
// streaming offset for the polling loop).
func readLastLines(f *os.File, n int) (int64, [][]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil, nil
	}

	const chunkSize = int64(4096)
	var accumulated []byte
	pos := size
	for pos > 0 && bytes.Count(accumulated, []byte{'\n'}) <= n {
		readSize := chunkSize
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return 0, nil, err
		}
		accumulated = append(buf, accumulated...)
	}

	// Split into lines and take the last n.
	all := bytes.Split(accumulated, []byte{'\n'})
	// If the file ends with a newline the last element is empty — drop it.
	if len(all) > 0 && len(all[len(all)-1]) == 0 {
		all = all[:len(all)-1]
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}

	// Position the file at end so the streaming loop picks up from there.
	end, _ := f.Seek(0, io.SeekEnd)

	// Copy to avoid aliasing into accumulated.
	out := make([][]byte, len(all))
	for i, l := range all {
		out[i] = append([]byte(nil), l...)
	}
	return end, out, nil
}
