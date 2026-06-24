// Package logger provides a small, dependency-free logger for the CLI.
//
// Two levels:
//
//	Error — always written. Use for any failure path (HTTP error, config
//	        missing, subprocess crash, etc.). Errors are the most important
//	        signal when debugging "events aren't reaching the platform."
//	Debug — written only when the active config has Debug=true. Use for
//	        trace-level breadcrumbs.
//
// Logs live under the user's config dir at
//
//	~/.config/promptconduit/logs/promptconduit.log
//
// (or the equivalent XDG path). The file is rotated when it exceeds
// MaxBytes — the current file is renamed to `.1` and a fresh one is
// started. Only one backup is kept; older data is discarded. This keeps
// total disk footprint bounded at ~2*MaxBytes per machine.
package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxBytes is the soft size cap at which the active log file is rotated.
// Tuned for "one or two days of activity for a typical user": large enough
// to keep useful history, small enough that a tail/cat finishes instantly.
const MaxBytes int64 = 1 << 20 // 1 MiB

// dirOverride, when set, replaces the user-config-dir lookup. Used by tests
// so we don't write into the real home directory.
//
// Two mutexes:
//   - dirMu guards dirOverride (cheap RWMutex; Dir() is called from inside write())
//   - writeMu serializes file open/append so concurrent writers don't interleave
var (
	dirOverride string
	dirMu       sync.RWMutex
	writeMu     sync.Mutex
)

// SetDirForTest overrides the log directory. Test-only. Pass "" to clear.
func SetDirForTest(dir string) {
	dirMu.Lock()
	defer dirMu.Unlock()
	dirOverride = dir
}

// Dir returns the directory where logs are written.
func Dir() string {
	dirMu.RLock()
	d := dirOverride
	dirMu.RUnlock()
	if d != "" {
		return d
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "promptconduit", "logs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "promptconduit-logs")
	}
	return filepath.Join(home, ".config", "promptconduit", "logs")
}

// Path returns the absolute path of the active log file.
func Path() string {
	return filepath.Join(Dir(), "promptconduit.log")
}

// BackupPath returns the path of the rotated backup file.
func BackupPath() string {
	return filepath.Join(Dir(), "promptconduit.log.1")
}

// Error writes an error-level line. Always recorded.
func Error(format string, args ...interface{}) {
	write("ERROR", format, args...)
}

// Debug writes a debug-level line. Recorded only when the caller has
// signaled debug mode via SetDebug — typically the CLI sets this at startup
// based on the loaded config. We accept the bool as a function rather than
// reading config every call to avoid a circular dependency with the client
// package and to keep the hot path cheap.
func Debug(format string, args ...interface{}) {
	if !debugEnabled() {
		return
	}
	write("DEBUG", format, args...)
}

var (
	debugMu  sync.RWMutex
	debugOn  bool
	debugSet bool
)

// SetDebug toggles debug-level output. Call once during startup. Until it
// is called, Debug() is a no-op so we never accidentally spam the log from
// a process that hasn't loaded config (e.g. unit tests).
func SetDebug(on bool) {
	debugMu.Lock()
	debugOn = on
	debugSet = true
	debugMu.Unlock()
}

func debugEnabled() bool {
	debugMu.RLock()
	defer debugMu.RUnlock()
	return debugSet && debugOn
}

func write(level, format string, args ...interface{}) {
	writeMu.Lock()
	defer writeMu.Unlock()

	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// Best-effort: if we can't even make the directory, drop the line
		// rather than crash the host process (this is called from a hook
		// that must never block or fail the AI tool).
		return
	}

	path := Path()
	rotateIfNeeded(path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	msg := fmt.Sprintf(format, args...)
	pid := os.Getpid()
	line := fmt.Sprintf("%s %s pid=%d %s\n",
		time.Now().UTC().Format(time.RFC3339Nano),
		level,
		pid,
		msg,
	)
	_, _ = f.WriteString(line)
}

// rotateIfNeeded renames the active log to `.1` once it crosses MaxBytes.
// Any prior `.1` is overwritten. Called with `writeMu` held.
func rotateIfNeeded(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Size() < MaxBytes {
		return
	}
	_ = os.Remove(BackupPath())
	_ = os.Rename(path, BackupPath())
}

// Tail returns up to maxLines lines from the end of the log (current file
// only — backup is not read). Returns ("", nil) when the file doesn't
// exist yet. Reads the whole file; not intended for unbounded logs (we
// rotate at 1 MiB).
func Tail(maxLines int) (string, error) {
	path := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if maxLines <= 0 {
		return string(data), nil
	}
	return lastLines(data, maxLines), nil
}

// CopyTo streams the active log file to w. Used by `promptconduit logs
// --follow` and for piping into other tools.
func CopyTo(w io.Writer) error {
	path := Path()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

func lastLines(data []byte, n int) string {
	if len(data) == 0 || n <= 0 {
		return ""
	}
	count := 0
	// Walk backwards until we've seen n newlines (or hit the start).
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
