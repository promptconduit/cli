package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

// subagentLine builds a SubagentStop envelope with a priced subagent total.
// pricedField is the raw model_priced JSON value ("" omits it, as envelopes
// recorded before the field existed do).
func subagentLine(capturedAt, agentID, model string, requests int, usd float64, pricedField string) string {
	priced := ""
	if pricedField != "" {
		priced = `,"model_priced":` + pricedField
		if pricedField == "false" {
			priced += `,"unpriced_requests":` + itoa(int64(requests)) + `,"unpriced_model":"` + model + `"`
		}
	}
	return `{"schema":2,"captured_at":"` + capturedAt + `","hook_event":"SubagentStop","enrichments":{"subagent":{"agent_id":"` + agentID +
		`","phase":"stop","requests":` + itoa(int64(requests)) + `,"model":"` + model +
		`","tokens":{"input":10,"output":5,"cache_read":100,"cache_write":0},"usd":{"total":` + ftoa(usd) + `}` + priced + `}}}`
}

// TestAggregateCostHistoryCountsSubagents: subagent spend lives in its own
// transcript, so the SubagentStop total must be added to the day (once per
// agent), and unpriced subagent models must be named, not shown as $0.
func TestAggregateCostHistoryCountsSubagents(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 29, 15, 0, 0, 0, loc)
	log := strings.Join([]string{
		costLine("2026-10-29T10:00:00Z", costReq("r1", "2026-10-29T10:00:00Z", true, 1.00, 10, 1, 0)),
		subagentLine("2026-10-29T10:05:00Z", "a1", "claude-opus-5-5", 4, 2.50, "true"),
		// The same agent's Stop replayed: counted once.
		subagentLine("2026-10-29T10:06:00Z", "a1", "claude-opus-5-5", 4, 2.50, "true"),
		// New-style unpriced subagent.
		subagentLine("2026-10-29T10:07:00Z", "a2", "mystery-model", 2, 0, "false"),
		// Recorded before model_priced existed: tokens at $0 means unpriced.
		subagentLine("2026-10-29T10:08:00Z", "a3", "claude-haiku-5-5", 3, 0, ""),
		// A SubagentStart (no totals) adds nothing.
		`{"schema":2,"captured_at":"2026-10-29T10:00:00Z","enrichments":{"subagent":{"agent_id":"a4","phase":"start","concurrent":1}}}`,
	}, "\n")

	got, err := aggregateCostHistory(strings.NewReader(log), now, 1, loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one day, got %+v", got)
	}
	d := got[0]
	if !approx(d.CostTotal, 3.50) || !approx(d.SubagentCost, 2.50) {
		t.Errorf("cost_total=%.4f subagent_cost=%.4f, want 3.50 and 2.50", d.CostTotal, d.SubagentCost)
	}
	if d.Requests != 1+4+2+3 {
		t.Errorf("requests = %d, want 10", d.Requests)
	}
	if d.Input != 10+10*3 {
		t.Errorf("input = %d, want 40 (main + three subagents)", d.Input)
	}
	if d.Unpriced != 5 || d.UnpricedModels["mystery-model"] != 2 || d.UnpricedModels["claude-haiku-5-5"] != 3 {
		t.Errorf("unpriced = %d models=%v", d.Unpriced, d.UnpricedModels)
	}
}

func TestAggregateCostHistoryNamesUnpricedRequestModels(t *testing.T) {
	now := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	req := `{"request_id":"r9","ts":"2026-10-29T10:00:00Z","model":"new-model-x","model_priced":false,"tokens":{"input":5},"usd":{"total":0}}`
	got, err := aggregateCostHistory(strings.NewReader(costLine("2026-10-29T10:00:00Z", req)), now, 1, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UnpricedModels["new-model-x"] != 1 {
		t.Fatalf("want new-model-x named as unpriced, got %+v", got)
	}
	if s := formatModelCounts(map[string]int{"b": 1, "a": 1, "c": 3}); s != "c x3, a x1, b x1" {
		t.Errorf("formatModelCounts = %q", s)
	}
}

func TestWithUnpricedNamesModels(t *testing.T) {
	s := cost.SessionSummary{ByModel: []cost.ModelTotal{
		{Model: "claude-opus-5-5", ModelPriced: true},
		{Model: "new-model-x", ModelPriced: false},
	}}
	out := withUnpriced(map[string]interface{}{}, s)
	models, _ := out["unpriced_models"].([]string)
	if len(models) != 1 || models[0] != "new-model-x" || out["unpriced_note"] == nil {
		t.Fatalf("withUnpriced = %+v", out)
	}
	if out := withUnpriced(map[string]interface{}{}, cost.SessionSummary{}); len(out) != 0 {
		t.Fatalf("all-priced session should add nothing, got %+v", out)
	}
}
