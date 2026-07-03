package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// RecordCapture appends one v2 event envelope to events.jsonl, exactly as it
// would be POSTed to /v1/events/raw, captured at hook time BEFORE any network
// send is attempted. It runs for every captured event — including Free /
// local-only installs that never send — so the file is a complete,
// send-independent stream that all local tooling reads.
//
// Each line is the bare envelope JSON (no outcome wrapper), secret-scrubbed by
// RedactBody. Best-effort and gated on Enabled(): a write failure drops the
// line rather than disturbing the hook's hot path.
func RecordCapture(payload []byte) {
	if !Enabled() {
		return
	}

	line := RedactBody(payload)
	// The payload is our own envelope, so this should always hold; guard anyway
	// so a malformed line can never corrupt the JSONL stream readers depend on.
	if !json.Valid(line) {
		return
	}

	migrateOnce.Do(migrateV1Files)
	appendLine(EventsJSONLPath(), line, EventsRotateAt)
}

var migrateOnce sync.Once

// migrateV1Files performs the one-time local tidy-up for the v2 redesign:
// an events.jsonl that still starts with a v1 envelope (no `"schema":2`) is
// moved aside to events.jsonl.v1.bak so the live file is pure v2, and the
// retired events.ndjson send log is deleted. Best-effort.
func migrateV1Files() {
	path := EventsJSONLPath()
	if headIsV1(path) {
		_ = os.Remove(path + ".v1.bak")
		_ = os.Rename(path, path+".v1.bak")
		_ = os.Remove(path + ".1") // rotated backup is v1 too
	}
	_ = os.Remove(filepath.Join(Dir(), "events.ndjson"))
	_ = os.Remove(filepath.Join(Dir(), "events.ndjson.1"))
}

// headIsV1 reports whether the first line of path parses as an envelope with
// a schema other than the current one. Absent/empty/corrupt files are not v1.
func headIsV1(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return false
	}
	var probe struct {
		Schema int `json:"schema"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return false
	}
	return probe.Schema < 2
}

// CountCaptured returns the number of events currently in events.jsonl and
// false if the file doesn't exist yet. Best-effort; reads the whole file, whose
// size is bounded by rotation (EventsRotateAt).
func CountCaptured() (int, bool) {
	data, err := os.ReadFile(EventsJSONLPath())
	if err != nil {
		return 0, false
	}
	if len(data) == 0 {
		return 0, true
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	// Count a trailing line that has no terminating newline.
	if data[len(data)-1] != '\n' {
		n++
	}
	return n, true
}
