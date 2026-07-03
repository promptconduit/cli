package envelope

import (
	"encoding/json"
	"testing"
)

func TestNew_V2Shape(t *testing.T) {
	enrichments := map[string]json.RawMessage{
		"vcs": json.RawMessage(`{"type":"github","repo":"promptconduit/cli","branch":"main"}`),
		"env": json.RawMessage(`{"os":"darwin"}`),
	}
	env := New("dev", "claude-code", "SessionStart", "sess-1", "prompt-1", []byte(`{"a":1}`), enrichments)

	if env.Schema != SchemaVersion {
		t.Errorf("Schema = %d, want %d", env.Schema, SchemaVersion)
	}
	if env.EventID == "" {
		t.Error("EventID must be minted at construction")
	}
	if env.SessionID != "sess-1" || env.PromptID != "prompt-1" {
		t.Errorf("ids = %q/%q, want sess-1/prompt-1", env.SessionID, env.PromptID)
	}

	out, err := env.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["schema"] != float64(2) {
		t.Errorf("wire schema = %v, want 2", got["schema"])
	}
	if got["session_id"] != "sess-1" {
		t.Errorf("wire session_id = %v", got["session_id"])
	}
	if got["prompt_id"] != "prompt-1" {
		t.Errorf("wire prompt_id = %v", got["prompt_id"])
	}
	if _, ok := got["raw_event"]; !ok {
		t.Errorf("raw_event missing from wire envelope: %s", out)
	}
	if _, ok := got["native_payload"]; ok {
		t.Errorf("legacy native_payload must not appear in v2: %s", out)
	}
	enr, ok := got["enrichments"].(map[string]interface{})
	if !ok {
		t.Fatalf("enrichments missing or wrong type: %s", out)
	}
	if _, ok := enr["vcs"]; !ok {
		t.Error("enrichments.vcs missing")
	}
	if _, ok := enr["env"]; !ok {
		t.Error("enrichments.env missing")
	}
}

func TestNew_OmitsEmptyOptionals(t *testing.T) {
	env := New("dev", "test", "test", "", "", []byte(`{}`), nil)
	out, err := env.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session_id", "prompt_id", "enrichments", "attachments"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s should be omitted when empty: %s", key, out)
		}
	}
}

func TestNewEventID_UniqueAndOrdered(t *testing.T) {
	a, b := NewEventID(), NewEventID()
	if a == "" || b == "" || a == b {
		t.Fatalf("event ids must be unique and non-empty: %q %q", a, b)
	}
}

func TestExtractIDs(t *testing.T) {
	cases := []struct {
		name        string
		tool        string
		event       map[string]interface{}
		wantSession string
		wantPrompt  string
	}{
		{
			name:        "claude-code",
			tool:        "claude-code",
			event:       map[string]interface{}{"session_id": "s1", "prompt_id": "p1"},
			wantSession: "s1",
			wantPrompt:  "p1",
		},
		{
			name:        "claude-code without prompt id",
			tool:        "claude-code",
			event:       map[string]interface{}{"session_id": "s1"},
			wantSession: "s1",
		},
		{
			name:        "cursor",
			tool:        "cursor",
			event:       map[string]interface{}{"session_id": "cs", "conversation_id": "conv", "generation_id": "gen"},
			wantSession: "cs",
			wantPrompt:  "gen",
		},
		{
			name:        "cursor conversation fallback",
			tool:        "cursor",
			event:       map[string]interface{}{"conversation_id": "conv"},
			wantSession: "conv",
		},
		{
			name:        "camelCase session",
			tool:        "unknown",
			event:       map[string]interface{}{"sessionId": "s2"},
			wantSession: "s2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, p := ExtractIDs(tc.tool, tc.event)
			if s != tc.wantSession || p != tc.wantPrompt {
				t.Errorf("ExtractIDs = %q/%q, want %q/%q", s, p, tc.wantSession, tc.wantPrompt)
			}
		})
	}
}
