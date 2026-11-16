package enrich

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestGoldenSubagentSlug_CarriesUnpricedFields pins the subagent slug in the
// cross-repo golden envelope to SubagentEnrichment's json tags. The platform
// and the extension mirror the golden byte-for-byte, so a field the CLI emits
// but the golden omits (as model_priced / unpriced_* were) never reaches their
// type mirrors.
func TestGoldenSubagentSlug_CarriesUnpricedFields(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "envelope", "testdata", "envelope.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var env struct {
		Enrichments struct {
			Subagent json.RawMessage `json:"subagent"`
		} `json:"enrichments"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}

	var sub SubagentEnrichment
	if err := json.Unmarshal(env.Enrichments.Subagent, &sub); err != nil {
		t.Fatalf("golden subagent slug: %v", err)
	}
	if sub.ModelPriced == nil || *sub.ModelPriced || sub.UnpricedRequests == 0 || sub.UnpricedModel == "" {
		t.Errorf("golden subagent slug must exercise the unpriced fields: %+v", sub)
	}

	// Every key the struct emits must appear in the golden.
	out, err := json.Marshal(sub)
	if err != nil {
		t.Fatal(err)
	}
	var emitted, golden map[string]json.RawMessage
	_ = json.Unmarshal(out, &emitted)
	_ = json.Unmarshal(env.Enrichments.Subagent, &golden)
	for key := range emitted {
		if _, ok := golden[key]; !ok {
			t.Errorf("SubagentEnrichment emits %q but the golden subagent slug omits it", key)
		}
	}
}
