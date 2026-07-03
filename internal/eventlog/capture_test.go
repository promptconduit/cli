package eventlog

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRecordCaptureWritesBareEnvelope(t *testing.T) {
	withTempDir(t)

	payload := []byte(`{"schema":2,"tool":"claude-code","hook_event":"UserPromptSubmit","session_id":"abc","raw_event":{"session_id":"abc"}}`)
	RecordCapture(payload)

	data, err := os.ReadFile(EventsJSONLPath())
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	line := strings.TrimSpace(string(data))

	// The line must be the bare envelope object, not the send-outcome wrapper
	// that events.ndjson uses — external readers parse a clean envelope stream.
	var env map[string]any
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("captured line is not valid JSON: %v\n%s", err, line)
	}
	if env["tool"] != "claude-code" || env["hook_event"] != "UserPromptSubmit" {
		t.Errorf("unexpected envelope fields: %v", env)
	}
	if _, ok := env["outcome"]; ok {
		t.Error("capture line should not carry a send-outcome wrapper (outcome)")
	}
	if _, ok := env["payload"]; ok {
		t.Error("capture line should be the bare envelope, not a {payload:...} wrapper")
	}

	if n, ok := CountCaptured(); !ok || n != 1 {
		t.Errorf("CountCaptured() = %d,%v want 1,true", n, ok)
	}
}

func TestRecordCaptureRedactsSecrets(t *testing.T) {
	withTempDir(t)

	RecordCapture([]byte(`{"schema":2,"tool":"cursor","raw_event":{"prompt":"my key sk-ABCDEF0123456789abcd"}}`))

	data, err := os.ReadFile(EventsJSONLPath())
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	if strings.Contains(string(data), "sk-ABCDEF0123456789abcd") {
		t.Errorf("secret leaked into events.jsonl:\n%s", data)
	}
}

func TestRecordCaptureDisabledIsNoOp(t *testing.T) {
	dir := t.TempDir()
	SetDirForTest(dir)
	SetEnabled(false)
	t.Cleanup(func() { SetDirForTest("") })

	RecordCapture([]byte(`{"tool":"claude-code"}`))

	if _, err := os.Stat(EventsJSONLPath()); !os.IsNotExist(err) {
		t.Error("events.jsonl written while disabled")
	}
	if n, ok := CountCaptured(); ok || n != 0 {
		t.Errorf("CountCaptured() = %d,%v want 0,false", n, ok)
	}
}

func TestRecordCaptureSkipsInvalidJSON(t *testing.T) {
	withTempDir(t)

	RecordCapture([]byte(`this is not json`))

	if _, err := os.Stat(EventsJSONLPath()); !os.IsNotExist(err) {
		t.Error("invalid JSON should never be appended to events.jsonl")
	}
}

func TestCountCapturedCountsEachLine(t *testing.T) {
	withTempDir(t)

	for i := 0; i < 3; i++ {
		RecordCapture([]byte(`{"schema":2,"tool":"claude-code","hook_event":"Stop"}`))
	}
	if n, ok := CountCaptured(); !ok || n != 3 {
		t.Errorf("CountCaptured() = %d,%v want 3,true", n, ok)
	}
}

func TestMigrateV1FilesMovesLegacyLogAside(t *testing.T) {
	withTempDir(t)

	// A pre-v2 events.jsonl (no "schema" key) plus the retired ndjson files.
	v1Line := `{"envelope_version":"1.2","tool":"claude-code","native_payload":{}}`
	if err := os.WriteFile(EventsJSONLPath(), []byte(v1Line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyNDJSON := EventsJSONLPath()[:len(EventsJSONLPath())-len("events.jsonl")] + "events.ndjson"
	if err := os.WriteFile(legacyNDJSON, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrateV1Files()

	if _, err := os.Stat(EventsJSONLPath() + ".v1.bak"); err != nil {
		t.Errorf("v1 log not moved to .v1.bak: %v", err)
	}
	if _, err := os.Stat(legacyNDJSON); !os.IsNotExist(err) {
		t.Error("events.ndjson not deleted")
	}
	if _, err := os.Stat(EventsJSONLPath()); !os.IsNotExist(err) {
		t.Error("live events.jsonl should be gone until the first v2 capture")
	}
}

func TestMigrateV1FilesLeavesV2LogAlone(t *testing.T) {
	withTempDir(t)

	v2Line := `{"schema":2,"tool":"claude-code","raw_event":{}}`
	if err := os.WriteFile(EventsJSONLPath(), []byte(v2Line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrateV1Files()

	data, err := os.ReadFile(EventsJSONLPath())
	if err != nil || !strings.Contains(string(data), `"schema":2`) {
		t.Errorf("v2 log must be untouched: %v %s", err, data)
	}
}
