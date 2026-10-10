package eventlog

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ErrRewriteBusy means another process holds the prune lock (a prune or
// rewrite is already running). Callers should retry later rather than race it.
var ErrRewriteBusy = errors.New("event log is busy (a prune or rewrite is running); try again shortly")

// LineTransform maps one events.jsonl record (without its trailing newline) to
// its replacement. changed=false keeps the original bytes verbatim.
type LineTransform func(line []byte) (out []byte, changed bool)

// LockedRewrite applies transform to every record in events.jsonl and
// atomically replaces the file, returning how many records changed. prepare,
// if set, runs first under the same lock with the log path (e.g. a read-only
// pass that writes a backup); a prepare error aborts with the log untouched.
//
// It reuses the pruner's safety model: the cross-process prune lock (kept
// fresh, so a long pass is never declared stale and raced), a temp file in the
// same directory, a verbatim copy of any lines appended by hooks while it ran,
// and a rename. Lines have no length cap (raw tool payloads can be very
// large). Nothing is written when no record changes.
func LockedRewrite(prepare func(path string) error, transform LineTransform) (int, error) {
	release, ok := tryPruneLock()
	if !ok {
		return 0, ErrRewriteBusy
	}
	defer release()
	// A long pass over a large log could outlive pruneLockStale, letting a
	// hook-spawned pruner break the lock and race this rewrite. Keep it fresh.
	stop := keepLockFresh(pruneLockPath(), pruneLockStale/4)
	defer stop()
	writeMu.Lock()
	defer writeMu.Unlock()
	path := EventsJSONLPath()
	if prepare != nil {
		if err := prepare(path); err != nil {
			return 0, err
		}
	}
	return rewriteFile(path, transform)
}

// keepLockFresh bumps the lock file's mtime every interval until stop is called.
func keepLockFresh(path string, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				_ = os.Chtimes(path, now, now)
			}
		}
	}()
	return func() { close(done) }
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
		if len(line) > 0 && line[len(line)-1] != '\n' {
			// A record still being written (or truncated) at startSize: copy it
			// verbatim with no newline; carry() appends the rest after startSize,
			// so it is never split into two lines.
			if _, err := w.Write(line); err != nil {
				return fail(err)
			}
		} else if len(line) > 0 {
			body := line[:len(line)-1]
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

	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return fail(err)
	}
	// Carry over lines hooks appended while we ran and rename into place; the
	// final carry + rename hold the exclusive append lock so no concurrent
	// append can be lost with the old file (see finishRewrite).
	if err := finishRewrite(f, w, tmp, tmpName, path, startSize); err != nil {
		if errors.Is(err, errAppendLockBusy) {
			return 0, ErrRewriteBusy
		}
		return 0, err
	}
	return changed, nil
}
