package eventlog

import (
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

func TestRecordSendOutcomeBumpsSent(t *testing.T) {
	withTempDir(t)

	RecordSendOutcome("evt-1", "Stop", 200, 42, nil)

	st := LoadStatus()
	if st.Sent != 1 || st.Failed != 0 || st.Dropped != 0 {
		t.Errorf("counters = %+v, want sent=1", st)
	}
	if st.LastSuccessAt == "" {
		t.Error("last_success_at not set on success")
	}
}

func TestRecordSendOutcomeFailureWritesErrorAndBumpsFailed(t *testing.T) {
	withTempDir(t)

	RecordSendOutcome("evt-2", "Stop", 401, 10, errors.New("API error: 401 - unauthorized"))

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
	// The event_id is what makes the line actionable: it's the key that finds
	// the offending envelope in events.jsonl.
	if !strings.Contains(string(errLog), "event_id=evt-2") {
		t.Errorf("errors.log missing event_id:\n%s", errLog)
	}
}

func TestRecordSendOutcomeUnknownIdentifiersRenderAsDash(t *testing.T) {
	withTempDir(t)

	RecordSendOutcome("", "", 400, 5, errors.New("API error: 400 - Invalid JSON in request body"))

	errLog, err := os.ReadFile(ErrorsPath())
	if err != nil {
		t.Fatalf("read errors.log: %v", err)
	}
	if !strings.Contains(string(errLog), "event=- event_id=-") {
		t.Errorf("errors.log should render unknown identifiers as dashes:\n%s", errLog)
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

func TestEventsRotation(t *testing.T) {
	withTempDir(t)

	// Write a line that exceeds a tiny rotation threshold by appending
	// directly through the shared helper.
	big := strings.Repeat("x", 200)
	appendLine(EventsJSONLPath(), []byte(big), 100) // rotateAt=100 forces rotation
	appendLine(EventsJSONLPath(), []byte(big), 100)

	if _, err := os.Stat(EventsJSONLPath() + ".1"); err != nil {
		t.Errorf("expected rotated backup %s.1 to exist: %v", EventsJSONLPath(), err)
	}
}

func TestDisabledIsNoOp(t *testing.T) {
	dir := t.TempDir()
	SetDirForTest(dir)
	SetEnabled(false)
	t.Cleanup(func() { SetDirForTest("") })

	RecordCapture([]byte(`{"schema":2}`))
	RecordSendOutcome("evt-3", "Stop", 200, 1, nil)
	RecordDrop("parse_error", "x")
	Errorf("should not write")

	if _, err := os.Stat(EventsJSONLPath()); !os.IsNotExist(err) {
		t.Error("events.jsonl written while disabled")
	}
	if _, err := os.Stat(ErrorsPath()); !os.IsNotExist(err) {
		t.Error("errors.log written while disabled")
	}
	if _, err := os.Stat(StatusPath()); !os.IsNotExist(err) {
		t.Error("status.json written while disabled")
	}
}
