package cmd

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHookEventLineIsValidJSONForAnyPath(t *testing.T) {
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, cwd := range []string{
		`C:\Users\dev\My "Project"\src`, // Windows backslashes + quotes
		`/home/dev/plain`,
		"/tmp/tab\tand\nnewline",
		``,
	} {
		line, err := hookEventLine("Stop", cwd, `sess"1`, at)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Event, Cwd, Timestamp string
			SessionID             string `json:"session_id"`
		}
		if err := json.Unmarshal(line, &got); err != nil {
			t.Fatalf("cwd %q produced invalid JSON %s: %v", cwd, line, err)
		}
		if got.Cwd != cwd || got.Event != "Stop" || got.SessionID != `sess"1` || got.Timestamp != "2026-10-10T12:00:00Z" {
			t.Fatalf("round trip mismatch: %+v", got)
		}
	}
	// Field order is unchanged for readers that scan the raw line.
	line, _ := hookEventLine("Stop", "/x", "s", at)
	if want := `{"event":"Stop","cwd":"/x","session_id":"s","timestamp":"2026-10-10T12:00:00Z"}`; string(line) != want {
		t.Fatalf("line = %s, want %s", line, want)
	}
}
