package eventlog

import (
	"encoding/json"
	"os"
)

// RecordCapture appends one raw event envelope to events.jsonl, exactly as it
// would be POSTed to /v1/events/raw, captured at hook time BEFORE any network
// send is attempted.
//
// This is deliberately distinct from RecordSend (events.ndjson): RecordSend is
// keyed on a send outcome and only fires when we actually try to send, whereas
// RecordCapture is the unconditional local record of the event. It runs for
// every captured event — including Free / local-only installs that never send —
// so the file is a complete, send-independent stream that local tooling (e.g. a
// cost estimator) can read.
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

	appendLine(EventsJSONLPath(), line, EventsRotateAt)
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
