package eventlog

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// withTempDir points the event log at a throwaway dir and enables writes for
// the duration of a test, restoring the prior state afterwards.
func withTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetDirForTest(dir)
	SetEnabled(true)
	t.Cleanup(func() {
		SetDirForTest("")
		SetEnabled(false)
	})
	return dir
}

func TestRecordSendWritesFullPayloadAndBumpsSent(t *testing.T) {
	withTempDir(t)

	payload := []byte(`{"tool":"claude-code","hook_event":"Stop","native_payload":{"session_id":"abc"},"enrichment":{"correlation":{"trace_id":"t123"}}}`)
	RecordSend(SendRecord{
		Endpoint:  "/v1/events/raw",
		Payload:   payload,
		Status:    200,
		LatencyMs: 42,
		Attempt:   1,
	})

	data, err := os.ReadFile(EventsPath())
	if err != nil {
		t.Fatalf("read events.ndjson: %v", err)
	}
	line := strings.TrimSpace(string(data))

	var rec eventRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("event line is not valid JSON: %v\n%s", err, line)
	}
	if rec.Outcome != OutcomeSent {
		t.Errorf("outcome = %q, want sent", rec.Outcome)
	}
	if rec.HookEvent != "Stop" || rec.Tool != "claude-code" {
		t.Errorf("indexed fields wrong: tool=%q hook=%q", rec.Tool, rec.HookEvent)
	}
	if rec.SessionID != "abc" || rec.TraceID != "t123" {
		t.Errorf("session/trace wrong: session=%q trace=%q", rec.SessionID, rec.TraceID)
	}
	if rec.Status != 200 || rec.LatencyMs != 42 {
		t.Errorf("status/latency wrong: %d / %d", rec.Status, rec.LatencyMs)
	}
	// The full payload must be embedded as nested JSON, not a string.
	var embedded map[string]any
	if err := json.Unmarshal(rec.Payload, &embedded); err != nil {
		t.Fatalf("payload is not embedded as JSON object: %v", err)
	}

	st := LoadStatus()
	if st.Sent != 1 || st.Failed != 0 || st.Dropped != 0 {
		t.Errorf("counters = %+v, want sent=1", st)
	}
	if st.LastSuccessAt == "" {
		t.Error("last_success_at not set on success")
	}
}

func TestRecordSendFailureWritesErrorAndBumpsFailed(t *testing.T) {
	withTempDir(t)

	RecordSend(SendRecord{
		Endpoint: "/v1/events/raw",
		Payload:  []byte(`{"tool":"cursor","hook_event":"Stop","native_payload":{}}`),
		Status:   401,
		Attempt:  1,
		Err:      errors.New("API error: 401 - unauthorized"),
	})

	st := LoadStatus()
	if st.Failed != 1 || st.Sent != 0 {
		t.Errorf("counters = %+v, want failed=1", st)
	}
	if !strings.Contains(st.LastError, "401") {
		t.Errorf("last_error = %q, want it to mention 401", st.LastError)
	}

	errLog, err := os.ReadFile(ErrorsPath())
	if err != nil {
		t.Fatalf("read errors.log: %v", err)
	}
	if !strings.Contains(string(errLog), "send failed") || !strings.Contains(string(errLog), "401") {
		t.Errorf("errors.log missing failure line:\n%s", errLog)
	}
}

func TestRecordDropLogsAndCounts(t *testing.T) {
	withTempDir(t)

	RecordDrop("not_configured", "no api key")

	st := LoadStatus()
	if st.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", st.Dropped)
	}
	errLog, _ := os.ReadFile(ErrorsPath())
	if !strings.Contains(string(errLog), "not_configured") {
		t.Errorf("errors.log missing drop reason:\n%s", errLog)
	}
}

func TestRedactBodyMasksSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		bad  string
	}{
		{"openai key", `{"prompt":"my key is sk-ABCDEF0123456789abcd"}`, "sk-ABCDEF0123456789abcd"},
		{"bearer", `Authorization: Bearer abcdefghijklmnop1234`, "abcdefghijklmnop1234"},
		{"json token pair", `{"api_key":"supersecretvalue12345"}`, "supersecretvalue12345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(RedactBody([]byte(tc.in)))
			if strings.Contains(out, tc.bad) {
				t.Errorf("secret not redacted: %s -> %s", tc.in, out)
			}
			if !strings.Contains(out, RedactionMask) {
				t.Errorf("mask not present in output: %s", out)
			}
		})
	}
}

func TestRedactBodyEmbeddedPayloadStaysValidJSON(t *testing.T) {
	withTempDir(t)
	payload := []byte(`{"tool":"claude-code","hook_event":"UserPromptSubmit","native_payload":{"prompt":"token sk-ABCDEF0123456789abcd here"}}`)
	RecordSend(SendRecord{Endpoint: "/v1/events/raw", Payload: payload, Status: 200, Attempt: 1})

	data, _ := os.ReadFile(EventsPath())
	var rec eventRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("redacted line not valid JSON: %v", err)
	}
	if strings.Contains(string(rec.Payload), "sk-ABCDEF0123456789abcd") {
		t.Error("secret leaked into embedded payload")
	}
}

func TestEventsRotation(t *testing.T) {
	dir := withTempDir(t)

	// Write a line that exceeds a tiny rotation threshold by appending
	// directly through the shared helper.
	big := strings.Repeat("x", 200)
	appendLine(EventsPath(), []byte(big), 100) // rotateAt=100 forces rotation
	appendLine(EventsPath(), []byte(big), 100)

	if _, err := os.Stat(EventsPath() + ".1"); err != nil {
		t.Errorf("expected rotated backup %s.1 to exist: %v", EventsPath(), err)
	}
	_ = dir
}

func TestDisabledIsNoOp(t *testing.T) {
	dir := t.TempDir()
	SetDirForTest(dir)
	SetEnabled(false)
	t.Cleanup(func() { SetDirForTest("") })

	RecordSend(SendRecord{Endpoint: "/v1/events/raw", Payload: []byte(`{}`), Status: 200, Attempt: 1})
	RecordDrop("parse_error", "x")
	Errorf("should not write")

	if _, err := os.Stat(EventsPath()); !os.IsNotExist(err) {
		t.Error("events.ndjson written while disabled")
	}
	if _, err := os.Stat(ErrorsPath()); !os.IsNotExist(err) {
		t.Error("errors.log written while disabled")
	}
}
