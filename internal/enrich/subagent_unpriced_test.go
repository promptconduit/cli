package enrich

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSubagentStop_FlagsUnpricedModels: a subagent on a model missing from the
// rate table must say so (model_priced=false plus a count and model id), not
// report a bare $0 total that reads as free. Haiku 5.5 is priced now, so
// its turns count toward USD.
func TestSubagentStop_FlagsUnpricedModels(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })

	transcript := filepath.Join(t.TempDir(), "agent.jsonl")
	lines := `{"type":"assistant","requestId":"u-1","message":{"model":"claude-haiku-5-5","usage":{"input_tokens":1000,"output_tokens":100}}}
{"type":"assistant","requestId":"u-2","message":{"model":"made-up-model-9","usage":{"input_tokens":10,"output_tokens":5}}}
`
	if err := os.WriteFile(transcript, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, err := subagentEnricher{}.Enrich(&Context{
		Tool: "claude-code", HookEvent: "SubagentStop", SessionID: "s",
		RawEvent: map[string]interface{}{"agent_id": "a9", "agent_transcript_path": transcript},
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := payload.(*SubagentEnrichment)
	if stop.ModelPriced == nil || *stop.ModelPriced {
		t.Fatalf("model_priced = %v, want false", stop.ModelPriced)
	}
	if stop.UnpricedRequests != 1 || stop.UnpricedModel != "made-up-model-9" {
		t.Errorf("unpriced = %d %q", stop.UnpricedRequests, stop.UnpricedModel)
	}
	// Haiku 5.5: 1000 in at $0.10/M + 100 out at $0.50/M.
	if stop.USD == nil || stop.USD.Total < 0.000149 || stop.USD.Total > 0.000151 {
		t.Errorf("usd = %+v, want $0.00015 from the priced Haiku 5.5 turn", stop.USD)
	}
	data, _ := json.Marshal(stop)
	if !strings.Contains(string(data), `"model_priced":false`) {
		t.Errorf("serialized slug lacks model_priced=false: %s", data)
	}

	// SubagentStart carries no totals, so no priced flag either.
	start, _ := subagentEnricher{}.Enrich(&Context{
		Tool: "claude-code", HookEvent: "SubagentStart", SessionID: "s",
		RawEvent: map[string]interface{}{"agent_id": "a10"},
	})
	if data, _ := json.Marshal(start); strings.Contains(string(data), "model_priced") {
		t.Errorf("start phase should omit model_priced: %s", data)
	}
}
