// Package eventlog provides a discoverable, human-facing observability layer
// for the CLI under the user's ~/.promptconduit/ directory — the same folder
// that already holds the lightweight hook-events / app-events status traces.
//
// It writes three files:
//
//	events.ndjson  — one record per outbound event send, embedding the FULL
//	                 envelope payload we POST to /v1/events/raw (untruncated),
//	                 plus the HTTP outcome (status, latency, attempt).
//	errors.log     — human-readable lines for every failure or silent drop,
//	                 so "events aren't reaching the platform" is debuggable.
//	status.json    — rolling counters (sent / failed / dropped) and the last
//	                 success/error, surfaced by `promptconduit status`.
//
// Design notes:
//   - This intentionally does NOT reuse internal/logger (combined error/debug
//     log under ~/.config/…) or internal/outbound (an http.RoundTripper that
//     truncates bodies at 64KB, also under ~/.config/…). Those serve different
//     purposes; this layer is event-keyed, full-fidelity, and lives where the
//     user actually browses.
//   - Every write is best-effort: failures are swallowed so the event log can
//     never block or fail the AI tool's hook. Observability must never break
//     the thing it observes.
//   - Writing is gated behind SetEnabled (default off until the process opts
//     in via config), mirroring how internal/logger gates Debug output.
package eventlog

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventsRotateAt is the size threshold at which events.ndjson is rotated to
// events.ndjson.1. Records themselves are never truncated (full fidelity);
// total disk is bounded by rotation instead. One backup is kept.
const EventsRotateAt int64 = 50 * 1024 * 1024 // 50 MB

// ErrorsRotateAt is the (smaller) rotation threshold for the human-readable
// errors.log — sized like internal/logger so a tail/cat finishes instantly.
const ErrorsRotateAt int64 = 1 << 20 // 1 MiB

var (
	// dirOverride replaces the home-dir lookup. Tests set this so they don't
	// write into the real ~/.promptconduit. Guarded by dirMu.
	dirOverride string
	dirMu       sync.RWMutex

	// writeMu serializes file open/append/rotate so concurrent writers (the
	// hook process and its async send subprocess) don't interleave lines.
	writeMu sync.Mutex

	// enabled gates all writes. Off until SetEnabled(true); keeps a process
	// that never loaded config (e.g. unit tests) from writing to the home dir.
	enabledMu sync.RWMutex
	enabled   bool
)

// SetDirForTest overrides the event-log directory. Test-only. Pass "" to clear.
func SetDirForTest(dir string) {
	dirMu.Lock()
	defer dirMu.Unlock()
	dirOverride = dir
}

// SetEnabled toggles all event-log writes. Call once during startup based on
// the loaded config (cfg.EventLog). Until called, every write is a no-op.
func SetEnabled(on bool) {
	enabledMu.Lock()
	enabled = on
	enabledMu.Unlock()
}

// Enabled reports whether event-log writes are currently active.
func Enabled() bool {
	enabledMu.RLock()
	defer enabledMu.RUnlock()
	return enabled
}

// Dir returns the directory where event-log files are written:
// ~/.promptconduit/ (matching the existing hook-events/app-events traces).
//
// Note this is deliberately the plain home dir, NOT client.ConfigDir(), which
// points at ~/.config/promptconduit/. The two live in different places by
// design — see the package doc.
func Dir() string {
	dirMu.RLock()
	d := dirOverride
	dirMu.RUnlock()
	if d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "promptconduit")
	}
	return filepath.Join(home, ".promptconduit")
}

// EventsPath is the absolute path of the full-payload event log.
func EventsPath() string { return filepath.Join(Dir(), "events.ndjson") }

// EventsJSONLPath is the absolute path of the capture log: one raw envelope per
// line, written at capture time BEFORE any network send. Unlike events.ndjson
// (which is keyed on send outcome), this file is the durable local substrate of
// the events themselves — it is written even when nothing is sent (Free /
// local-only mode), and is the file external local tooling reads.
func EventsJSONLPath() string { return filepath.Join(Dir(), "events.jsonl") }

// ErrorsPath is the absolute path of the human-readable error log.
func ErrorsPath() string { return filepath.Join(Dir(), "errors.log") }

// StatusPath is the absolute path of the rolling counters file.
func StatusPath() string { return filepath.Join(Dir(), "status.json") }

// appendLine appends a single line (caller includes no trailing newline) to
// path, rotating first if it has grown past rotateAt. Best-effort: any error
// drops the line rather than disturbing the caller's hot path.
func appendLine(path string, line []byte, rotateAt int64) {
	writeMu.Lock()
	defer writeMu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	rotateIfNeeded(path, rotateAt)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	_, _ = f.Write(line)
	_, _ = f.Write([]byte{'\n'})
}

// rotateIfNeeded renames path -> path+".1" once it reaches rotateAt bytes. Any
// prior backup is overwritten; only one is kept. Called with writeMu held.
func rotateIfNeeded(path string, rotateAt int64) {
	if rotateAt <= 0 {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Size() < rotateAt {
		return
	}
	backup := path + ".1"
	_ = os.Remove(backup)
	_ = os.Rename(path, backup)
}

// timeLayout is the timestamp format used across all event-log files.
const timeLayout = time.RFC3339

// nowUTC is the single clock used for all timestamps; kept as a var so tests
// could stub it if needed.
var nowUTC = func() time.Time { return time.Now().UTC() }
