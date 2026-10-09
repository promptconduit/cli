package eventlog

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ErrRewriteBusy means another process holds the prune lock (a prune or
// rewrite is already running). Callers should retry later rather than race it.
var ErrRewriteBusy = errors.New("event log is busy (a prune or rewrite is running); try again shortly")

// LineTransform maps one events.jsonl record (without its trailing newline) to
// its replacement. changed=false keeps the original bytes verbatim.
type LineTransform func(line []byte) (out []byte, changed bool)

// RewriteEvents applies transform to every record in events.jsonl and
// atomically replaces the file, returning how many records changed. It reuses
// the pruner's safety model: the cross-process prune lock (so it never races a
// background prune), a temp file in the same directory, a verbatim copy of any
// lines appended by hooks while it ran, and a rename. Lines have no length cap
// (raw tool payloads can be very large). Nothing is written when no record
// changes.
func RewriteEvents(transform LineTransform) (int, error) {
	release, ok := tryPruneLock()
	if !ok {
		return 0, ErrRewriteBusy
	}
	defer release()
	writeMu.Lock()
	defer writeMu.Unlock()
	return rewriteFile(EventsJSONLPath(), transform)
}

func rewriteFile(path string, transform LineTransform) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	// Only the bytes present now are transformed; anything appended during the
	// pass lands after startSize and is carried over verbatim below.
	startSize := info.Size()

	// ".prune-" prefix: an orphan from a killed run is swept by sweepStaleTemps.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prune-rewrite-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	fail := func(e error) (int, error) {
		cleanupTemp(tmp, tmpName)
		return 0, e
	}

	w := bufio.NewWriterSize(tmp, 1<<20)
	r := bufio.NewReaderSize(io.LimitReader(f, startSize), 1<<20)
	changed := 0
	buf := make([]byte, 0, 4096)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			body := line
			if body[len(body)-1] == '\n' {
				body = body[:len(body)-1]
			}
			out, ch := transform(body)
			if ch {
				changed++
			} else {
				out = body
			}
			// Record + terminator in one write, like every other JSONL writer here.
			buf = append(append(buf[:0], out...), '\n')
			if _, err := w.Write(buf); err != nil {
				return fail(err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fail(readErr)
		}
	}
	if changed == 0 {
		cleanupTemp(tmp, tmpName)
		return 0, nil
	}

	// Carry over lines hooks appended while we ran.
	if _, err := f.Seek(startSize, io.SeekStart); err != nil {
		return fail(err)
	}
	if _, err := io.Copy(w, f); err != nil {
		return fail(err)
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return 0, err
	}
	return changed, nil
}
