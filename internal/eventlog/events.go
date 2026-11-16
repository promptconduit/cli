package eventlog

import (
	"bytes"
	"io"
	"os"
)

// TailCaptured returns up to maxLines lines from the end of events.jsonl (the
// capture log), or an empty string when the file doesn't exist yet.
func TailCaptured(maxLines int) (string, error) {
	return tailFile(EventsJSONLPath(), maxLines)
}

// tailChunk is how much tailFile reads per step backwards from the end.
const tailChunk = 64 * 1024

// tailFile reads up to maxLines lines from the end of path. Returns ("", nil)
// when the file is absent. It reads backwards from the end in chunks until it
// has enough lines, so tailing a multi-hundred-MB event log touches only its
// last few KB. maxLines <= 0 returns the whole file.
func tailFile(path string, maxLines int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = f.Close() }()

	if maxLines <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	off := size
	var (
		chunks   [][]byte // newest first
		newlines int      // newlines seen, excluding the file's final byte
	)
	for off > 0 && newlines < maxLines {
		n := int64(tailChunk)
		if n > off {
			n = off
		}
		off -= n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, off); err != nil && err != io.EOF {
			return "", err
		}
		counted := chunk
		if off+n == size && n > 0 && chunk[n-1] == '\n' {
			counted = chunk[:n-1] // the terminator of the last line
		}
		newlines += bytes.Count(counted, []byte{'\n'})
		chunks = append(chunks, chunk)
	}
	// lastLines finds the maxLines-th newline from the end within what we
	// read (or returns everything when the file has fewer lines).
	buf := make([]byte, 0, size-off)
	for i := len(chunks) - 1; i >= 0; i-- {
		buf = append(buf, chunks[i]...)
	}
	return lastLines(buf, maxLines), nil
}

func lastLines(data []byte, n int) string {
	if len(data) == 0 || n <= 0 {
		return ""
	}
	count := 0
	i := len(data) - 1
	if data[i] == '\n' {
		i--
	}
	for ; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return string(data[i+1:])
			}
		}
	}
	return string(data)
}
