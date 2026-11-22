package enrich

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptEnricher(t *testing.T) {
	e := transcriptEnricher{}
	stop := &Context{Tool: "claude-code", HookEvent: "Stop", SessionID: "s1"}
	if !e.Applies(stop) {
		t.Error("should apply to claude-code Stop")
	}
	for _, c := range []*Context{
		{Tool: "claude-code", HookEvent: "PostToolUse", SessionID: "s1"},
		{Tool: "cursor", HookEvent: "stop", SessionID: "s1"},
		{Tool: "claude-code", HookEvent: "Stop"},
	} {
		if e.Applies(c) {
			t.Errorf("should not apply to %+v", c)
		}
	}

	present := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(present, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop.TranscriptPath = present
	if p, err := e.Enrich(stop); p != nil || err != nil {
		t.Errorf("existing transcript: payload=%v err=%v, want slug omitted", p, err)
	}

	stop.TranscriptPath = present + ".gone"
	p, err := e.Enrich(stop)
	if err != nil {
		t.Fatal(err)
	}
	got := p.(TranscriptEnrichment)
	if got.Saved || got.Path != stop.TranscriptPath || !strings.Contains(got.Warning, "s1") ||
		!strings.Contains(got.Warning, "CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1") {
		t.Errorf("missing transcript payload = %+v", got)
	}
	data, _ := json.Marshal(got)
	if !strings.Contains(string(data), `"saved":false`) {
		t.Errorf("saved:false must be serialized, got %s", data)
	}
}
