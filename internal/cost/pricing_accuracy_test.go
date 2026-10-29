package cost

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// TestResolvePrice_ClaudeFiveFive pins the 5.5 generation to Anthropic's list
// rates. claude-haiku-5-5 used to miss the table entirely (trim found nothing,
// so subagent spend on it showed $0), and claude-sonnet-5-5 only priced by
// accident through the claude-sonnet-5 trim.
func TestResolvePrice_ClaudeFiveFive(t *testing.T) {
	tbl := mustTable(t)
	cases := []struct {
		model                               string
		in, out, write5m, write1h, cacheRd float64
	}{
		{"claude-sonnet-5-5", 2e-6, 10e-6, 2.5e-6, 4e-6, 0.2e-6},
		{"claude-haiku-5-5", 0.1e-6, 0.5e-6, 0.125e-6, 0.2e-6, 0.01e-6},
		{"claude-opus-5-5", 4e-6, 20e-6, 5e-6, 8e-6, 0.2e-6},
		{"claude-fable-5-1", 10e-6, 50e-6, 12.5e-6, 20e-6, 0.25e-6},
	}
	for _, tc := range cases {
		key, mp, ok := tbl.resolve(tc.model)
		if !ok || key != tc.model {
			t.Errorf("%s: resolved to %q ok=%v, want its own row", tc.model, key, ok)
			continue
		}
		if !near(mp.Input, tc.in) || !near(mp.Output, tc.out) || !near(mp.CacheCreation, tc.write5m) ||
			!near(mp.CacheCreation1h, tc.write1h) || !near(mp.CacheRead, tc.cacheRd) {
			t.Errorf("%s: rates %+v", tc.model, mp)
		}
	}
}

// TestCostForUsage_HaikuLongContext checks Haiku 5.5's two rate cards: a
// prompt (input + cache read + cache write) of 100K tokens or fewer uses the
// standard card; above that the whole request bills at the 5x card.
func TestCostForUsage_HaikuLongContext(t *testing.T) {
	tbl := mustTable(t)
	mp, ok := tbl.ResolvePrice("claude-haiku-5-5")
	if !ok {
		t.Fatal("claude-haiku-5-5 must resolve")
	}

	// Exactly at the threshold: standard card.
	at := Usage{InputTokens: 10_000, CacheReadInputTokens: 80_000, CacheCreationInput: 10_000, OutputTokens: 1_000}
	c, _ := CostForUsage(at, mp)
	want := 10_000*0.1e-6 + 80_000*0.01e-6 + 10_000*0.125e-6 + 1_000*0.5e-6
	if !near(c.Total, want) {
		t.Errorf("at threshold: total %.10f, want %.10f", c.Total, want)
	}

	// One token over, with the 5m/1h split: long-context card for every part.
	over := Usage{InputTokens: 10_001, CacheReadInputTokens: 80_000, OutputTokens: 1_000,
		CacheCreationInput: 10_000, Ephemeral5mTokens: 4_000, Ephemeral1hTokens: 6_000}
	c, _ = CostForUsage(over, mp)
	want = 10_001*0.5e-6 + 80_000*0.05e-6 + 4_000*0.625e-6 + 6_000*1e-6 + 1_000*2.5e-6
	if !near(c.Total, want) {
		t.Errorf("over threshold: total %.10f, want %.10f", c.Total, want)
	}

	// Models without a long-context card are unaffected by prompt size.
	opus, _ := tbl.ResolvePrice("claude-opus-5-5")
	big := Usage{InputTokens: 900_000}
	if c, _ := CostForUsage(big, opus); !near(c.Total, 900_000*4e-6) {
		t.Errorf("opus 5.5 at 900K input: %.6f", c.Total)
	}
}

// TestResolvePriceForSpeed covers Anthropic fast mode: a usage.speed of "fast"
// prices from the model's @fast row, a model without one stays at its
// standard rate, and plain ResolvePrice (and Cursor's "-fast" display ids,
// which prefix-trim) never reach an @fast row.
func TestResolvePriceForSpeed(t *testing.T) {
	tbl := mustTable(t)
	cases := []struct {
		model, speed string
		wantIn       float64
	}{
		{"claude-opus-5-5", "fast", 8e-6},
		{"claude-opus-5-5", "standard", 4e-6},
		{"claude-opus-5-5", "", 4e-6},
		{"claude-opus-5", "fast", 10e-6},
		{"claude-opus-4-8", "fast", 10e-6},
		{"claude-opus-4-8-20260101", "fast", 10e-6}, // dated id trims, then fast row
		{"claude-sonnet-5-5", "fast", 2e-6},         // no fast row: standard rate
	}
	for _, tc := range cases {
		mp, ok := tbl.ResolvePriceForSpeed(tc.model, tc.speed)
		if !ok || !near(mp.Input, tc.wantIn) {
			t.Errorf("ResolvePriceForSpeed(%q, %q) input = %v ok=%v, want %v", tc.model, tc.speed, mp.Input, ok, tc.wantIn)
		}
	}
	if mp, _ := tbl.ResolvePrice("claude-opus-5-5-fast"); !near(mp.Input, 4e-6) {
		t.Errorf("Cursor id claude-opus-5-5-fast = %v, want the standard Opus 5.5 rate", mp.Input)
	}
	if _, ok := tbl.ResolvePriceForSpeed("not-a-model", "fast"); ok {
		t.Error("an unknown model must stay unpriced in fast mode")
	}
}

func TestParseClaudeCodeLine_FastMode(t *testing.T) {
	tbl := mustTable(t)
	line := []byte(`{"type":"assistant","requestId":"req_fast","sessionId":"s","message":{"model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":0,"speed":"fast"}}}`)
	ev, _, ok := parseClaudeCodeLine(line, tbl, "")
	if !ok || !ev.ModelPriced {
		t.Fatalf("fast line should parse and price: ok=%v %+v", ok, ev)
	}
	if !near(ev.Cost.Total, 8) {
		t.Errorf("1M fast-mode input tokens on Opus 5.5 = $%.4f, want $8", ev.Cost.Total)
	}
}

// TestSeedNewestIncludesSubagents: `cost session` must count a session's
// subagent transcripts (<session>/subagents/agent-*.jsonl), which the main
// transcript never repeats.
func TestSeedNewestIncludesSubagents(t *testing.T) {
	tbl := mustTable(t)
	dir := t.TempDir()
	main := filepath.Join(dir, "sess-1.jsonl")
	mainLine := `{"type":"assistant","requestId":"m1","sessionId":"sess-1","timestamp":"2026-10-29T10:00:00Z","message":{"model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(main, []byte(mainLine), 0o644); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(dir, "sess-1", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subLines := `{"type":"assistant","requestId":"s1","sessionId":"sess-1","isSidechain":true,"timestamp":"2026-10-29T10:00:01Z","message":{"model":"claude-haiku-5-5","usage":{"input_tokens":1000000,"output_tokens":0}}}
{"type":"assistant","requestId":"s1","sessionId":"sess-1","isSidechain":true,"timestamp":"2026-10-29T10:00:01Z","message":{"model":"claude-haiku-5-5","usage":{"input_tokens":1000000,"output_tokens":0}}}
`
	if err := os.WriteFile(filepath.Join(subDir, "agent-a.jsonl"), []byte(subLines), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(tbl, nil, nil, false)
	w.SeedNewest([]string{dir})
	s, ok := w.LatestSummary()
	if !ok || s.SessionID != "sess-1" {
		t.Fatalf("summary ok=%v id=%q", ok, s.SessionID)
	}
	// $4 main (Opus 5.5) + $0.50 subagent (Haiku 5.5 above 100K: 5x card),
	// the duplicated subagent line counted once.
	if !near(s.Totals.CostTotal, 4.5) {
		t.Errorf("session total = $%.4f, want $4.50 with subagent spend", s.Totals.CostTotal)
	}
	if len(s.ByModel) != 2 {
		t.Errorf("by_model = %+v, want Opus 5.5 and Haiku 5.5", s.ByModel)
	}
}
