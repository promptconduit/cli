package enrich

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCostEnricher_CountsToolsAcrossRequestLines: Claude Code writes one
// transcript line per content block, all sharing the requestId. The cost slug
// keeps one request (tokens once) carrying every one of its tool calls.
func TestCostEnricher_CountsToolsAcrossRequestLines(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })

	usage := `"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":1000}`
	line := func(uuid, content string) string {
		return `{"type":"assistant","requestId":"req_A","uuid":"` + uuid + `","sessionId":"s1","timestamp":"2026-11-27T00:00:00Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[` + content + `],` + usage + `}}`
	}
	edit := line("u3", `{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"/x"}}`)
	lines := []string{
		line("u1", `{"type":"text","text":"on it"}`),
		line("u2", `{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}`),
		edit,
		line("u4", `{"type":"tool_use","id":"toolu_3","name":"Read","input":{"file_path":"/y"}}`),
		edit, // the same line read twice must not double-count
	}
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := &Context{Tool: "claude-code", HookEvent: "Stop", SessionID: "s1", TranscriptPath: transcript, RawEvent: map[string]interface{}{}}
	payload, err := costEnricher{}.Enrich(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ce := payload.(*CostEnrichment)
	if len(ce.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(ce.Requests))
	}
	r := ce.Requests[0]
	if r.Tokens.Input != 10 || r.Tokens.Output != 20 || ce.Totals.Tokens.CacheRead != 1000 {
		t.Fatalf("tokens = %+v, want one request's worth", r.Tokens)
	}
	if r.Tools == nil || r.Tools.Total != 3 || r.Tools.ByName["Bash"] != 1 || r.Tools.ByName["Edit"] != 1 || r.Tools.ByName["Read"] != 1 {
		t.Fatalf("tools = %+v, want Bash, Edit, Read once each", r.Tools)
	}
	if r.Signals.ToolCalls != 3 {
		t.Fatalf("signals.tool_calls = %d, want 3", r.Signals.ToolCalls)
	}
}
