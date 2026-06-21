package eventlog

import (
	"encoding/json"
	"net/http"
	"os"
)

// SendRecord is what the client hands to RecordSend after attempting (or
// completing) a POST to the events endpoint.
type SendRecord struct {
	Endpoint  string // e.g. "/v1/events/raw"
	Payload   []byte // the exact envelope JSON we sent (full, untruncated)
	Status    int    // HTTP status code; 0 if the request never completed
	LatencyMs int64  // round-trip latency in milliseconds
	Attempt   int    // 1-based attempt number
	Err       error  // non-nil on transport error or non-2xx
}

// eventRecord is the on-disk shape (one NDJSON line in events.ndjson). The
// envelope payload is embedded verbatim (after a secret-scrub) via RawMessage
// so the file shows exactly what we put on the wire.
type eventRecord struct {
	TS        string          `json:"ts"`
	Tool      string          `json:"tool,omitempty"`
	HookEvent string          `json:"hook_event,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	TraceID   string          `json:"trace_id,omitempty"`
	Endpoint  string          `json:"endpoint,omitempty"`
	Outcome   Outcome         `json:"outcome"`
	Status    int             `json:"status"`
	LatencyMs int64           `json:"latency_ms"`
	Attempt   int             `json:"attempt"`
	Error     string          `json:"error,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// RecordSend writes one full-fidelity record to events.ndjson and updates the
// rolling status counters. It is the single entry point the client calls for
// every event send (success or failure). Best-effort and gated on Enabled().
func RecordSend(rec SendRecord) {
	if !Enabled() {
		return
	}

	outcome := OutcomeSent
	if rec.Err != nil || rec.Status < 200 || rec.Status >= 300 {
		outcome = OutcomeFailed
	}

	meta := peekEnvelope(rec.Payload)

	redacted := RedactBody(rec.Payload)
	// Guarantee the payload field is always valid JSON. If the bytes aren't
	// valid (shouldn't happen — it's our own envelope), store them as a JSON
	// string so the NDJSON line stays parseable.
	payload := json.RawMessage(redacted)
	if !json.Valid(redacted) {
		if quoted, err := json.Marshal(string(redacted)); err == nil {
			payload = quoted
		} else {
			payload = json.RawMessage(`null`)
		}
	}

	out := eventRecord{
		TS:        nowUTC().Format(timeLayout),
		Tool:      meta.Tool,
		HookEvent: meta.HookEvent,
		SessionID: meta.SessionID,
		TraceID:   meta.TraceID,
		Endpoint:  rec.Endpoint,
		Outcome:   outcome,
		Status:    rec.Status,
		LatencyMs: rec.LatencyMs,
		Attempt:   rec.Attempt,
		Payload:   payload,
	}
	if rec.Err != nil {
		out.Error = rec.Err.Error()
	}

	line, err := json.Marshal(out)
	if err != nil {
		return
	}
	appendLine(EventsPath(), line, EventsRotateAt)

	if outcome == OutcomeFailed {
		detail := out.Error
		if detail == "" {
			detail = httpDetail(rec.Status)
		}
		Errorf("send failed event=%s status=%d latency=%dms: %s",
			emptyDash(meta.HookEvent), rec.Status, rec.LatencyMs, detail)
		Bump(OutcomeFailed, detail)
	} else {
		Bump(OutcomeSent, "")
	}
}

// envelopeMeta holds the few indexed fields we lift out of the envelope so the
// event-log line is filterable without parsing the whole payload.
type envelopeMeta struct {
	Tool      string
	HookEvent string
	SessionID string
	TraceID   string
}

// peekEnvelope unmarshals only the fields we index. The session ID lives in the
// tool's native_payload under varying keys, so we check the common ones. All
// best-effort: a parse failure just yields empty fields.
func peekEnvelope(payload []byte) envelopeMeta {
	var e struct {
		Tool          string `json:"tool"`
		HookEvent     string `json:"hook_event"`
		NativePayload struct {
			SessionID  string `json:"session_id"`
			SessionID2 string `json:"sessionId"`
		} `json:"native_payload"`
		Enrichment struct {
			Correlation struct {
				TraceID string `json:"trace_id"`
			} `json:"correlation"`
		} `json:"enrichment"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		return envelopeMeta{}
	}
	sid := e.NativePayload.SessionID
	if sid == "" {
		sid = e.NativePayload.SessionID2
	}
	return envelopeMeta{
		Tool:      e.Tool,
		HookEvent: e.HookEvent,
		SessionID: sid,
		TraceID:   e.Enrichment.Correlation.TraceID,
	}
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func httpDetail(status int) string {
	if status == 0 {
		return "no response"
	}
	if txt := http.StatusText(status); txt != "" {
		return txt
	}
	return "unexpected status"
}

// TailEvents returns up to maxLines lines from the end of events.ndjson, or an
// empty string when the file doesn't exist yet.
func TailEvents(maxLines int) (string, error) {
	return tailFile(EventsPath(), maxLines)
}

// tailFile reads up to maxLines lines from the end of path. Returns ("", nil)
// when the file is absent. Reads the whole file; intended for our rotated
// (bounded) logs, not arbitrary large files.
func tailFile(path string, maxLines int) (string, error) {
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
