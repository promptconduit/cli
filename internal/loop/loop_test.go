package loop

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

func table(t *testing.T) *cost.PriceTable {
	t.Helper()
	tb, err := cost.LoadBundledPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// request prices one call the way the cost enrichment does (1h cache writes).
func request(t *testing.T, tb *cost.PriceTable, id string, ts time.Time, model string, tok [4]int64, tools ...string) Request {
	t.Helper()
	mp, ok := tb.ResolvePrice(model)
	if !ok {
		t.Fatalf("model %s not priced", model)
	}
	c, cw := cost.CostForUsage(cost.Usage{
		InputTokens: tok[0], OutputTokens: tok[1], CacheReadInputTokens: tok[2],
		CacheCreationInput: tok[3], Ephemeral1hTokens: tok[3],
	}, mp)
	r := Request{
		RequestID: id, TS: ts.UTC().Format(time.RFC3339Nano), Model: model,
		Tokens: cost.Tokens{Input: tok[0], Output: tok[1], CacheRead: tok[2], CacheWrite: cw},
		USD:    c,
	}
	for _, n := range tools {
		if r.ToolNames == nil {
			r.ToolNames = map[string]int{}
		}
		r.ToolNames[n]++
	}
	return r
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want ≈ %.4f (±%.3f)", what, got, want, tol)
	}
}

func findAdvice(as []Advice, id string) *Advice {
	for i := range as {
		if as[i].ID == id {
			return &as[i]
		}
	}
	return nil
}

// TestGolden_PublishBlogPost uses a real "publish the blog post" prompt on
// claude-opus-5-5: 8 calls re-reading ~390K tokens each, with a 5m04s wait
// between calls 2 and 3.
func TestGolden_PublishBlogPost(t *testing.T) {
	tb := table(t)
	const model = "claude-opus-5-5"
	tokens := [][4]int64{
		{4, 294, 389517, 8}, {2, 316, 389525, 348}, {2, 364, 389873, 509}, {2, 353, 390382, 1087},
		{2, 245, 391469, 533}, {2, 174, 392002, 305}, {4, 121, 392307, 353}, {2, 464, 392660, 213},
	}
	// The real tool sequence: single-tool calls back to back collapse
	// ([1,2], [4..7]); call 3 ran two tools and call 8 none: 8 -> 4.
	tools := [][]string{{"Bash"}, {"Bash"}, {"TaskStop", "Bash"}, {"Bash"}, {"Bash"}, {"ToolSearch"}, {"mcp__bing-webmaster__submit_url"}, nil}

	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ts := start.Add(2 * time.Second)
	var reqs []Request
	for i, tok := range tokens {
		if i == 2 {
			ts = ts.Add(5*time.Minute + 4*time.Second)
		} else if i > 0 {
			ts = ts.Add(8 * time.Second)
		}
		reqs = append(reqs, request(t, tb, "req-"+string(rune('a'+i)), ts, model, tok, tools[i]...))
	}
	p := &Prompt{PromptID: "p1", TaskKind: "ship", StartedAt: start, EndedAt: ts.Add(3 * time.Second)}
	Attribute([]*Prompt{p}, reqs)

	l, ok := BuildLedger(p, tb)
	if !ok {
		t.Fatal("no ledger")
	}
	if l.RoundTrips != 8 || len(l.Calls) != 8 {
		t.Fatalf("roundTrips = %d, calls = %d, want 8", l.RoundTrips, len(l.Calls))
	}
	near(t, "totals.usd", l.Totals.USD, 0.699, 0.002)
	near(t, "totals.reread", l.Totals.Reread, 0.627, 0.003)
	near(t, "totals.output", l.Totals.Output, 0.0466, 0.001)
	near(t, "totals.newContext", l.Totals.NewContext, 0.0269, 0.001)
	if l.CacheTTL != TTL1h {
		t.Errorf("cacheTtl = %q, want 1h", l.CacheTTL)
	}
	if l.LongestGapAfterCall != 1 || l.LongestGapMs != (5*time.Minute+4*time.Second).Milliseconds() {
		t.Errorf("longest gap = %dms after call %d, want 304000ms after call 1", l.LongestGapMs, l.LongestGapAfterCall)
	}
	if l.Calls[0].ContextTokens != 4+389517+8 {
		t.Errorf("contextTokens[0] = %d", l.Calls[0].ContextTokens)
	}
	if got := CollapsedRoundTrips(l.Calls); got != 4 {
		t.Errorf("collapsed round-trips = %d, want 4", got)
	}

	advice := BuildAdvice(l, tb, &SessionOpening{FirstCallUSD: 0.12, FirstCallContext: 30_000})

	fewer := findAdvice(advice, AdviceFewerRoundTrips)
	if fewer == nil {
		t.Fatal("missing fewer-round-trips")
	}
	near(t, "fewer-round-trips usdIfApplied", fewer.USDIfApplied, 0.35, 0.005)
	near(t, "fewer-round-trips deltaUsd", fewer.DeltaUSD, 0.35-0.699, 0.006)
	if fewer.GuideSlug != "batch-tool-calls" || fewer.Kind != KindOption {
		t.Errorf("fewer = %+v", fewer)
	}

	sw := findAdvice(advice, AdviceAvoidModelSwitch)
	if sw == nil {
		t.Fatal("missing avoid-model-switch")
	}
	near(t, "model switch usdIfApplied", sw.USDIfApplied, 2.14, 0.01)
	if sw.DeltaUSD <= 0 || !strings.HasPrefix(sw.Detail, "Costs more") || !strings.Contains(sw.Title, "claude-sonnet-5") {
		t.Errorf("switching to Sonnet must cost MORE here: %+v", sw)
	}

	exp := findAdvice(advice, AdviceCacheExpiry)
	if exp == nil {
		t.Fatal("missing cache-expiry")
	}
	near(t, "cache-expiry saved by 1h TTL", exp.USDIfApplied, 1.95, 0.01)
	if exp.Kind != KindNote || exp.DeltaUSD != 0 || exp.GuideSlug != "improve-cache-hit-rate" {
		t.Errorf("cache-expiry = %+v", exp)
	}

	fresh := findAdvice(advice, AdviceFreshSession)
	if fresh == nil || fresh.DeltaUSD >= 0 || fresh.GuideSlug != "delegate-with-subagents" {
		t.Errorf("fresh-session should save money from a 30K opening: %+v", fresh)
	}
	if findAdvice(advice, AdviceAfterPromptWake) != nil {
		t.Error("no after-prompt calls, no wake advice")
	}

	// Options cheapest first, notes last.
	sawNote := false
	for i, a := range advice {
		if a.Kind == KindNote {
			sawNote = true
		} else if sawNote {
			t.Errorf("option %s after a note", a.ID)
		} else if i > 0 && advice[i-1].USDIfApplied > a.USDIfApplied {
			t.Errorf("options not sorted cheapest first at %s", a.ID)
		}
	}

	// Wire shape: camelCase field names from the shared contract.
	data, _ := json.Marshal(map[string]interface{}{"ledger": l, "advice": advice})
	for _, k := range []string{`"promptId"`, `"taskKind":"ship"`, `"requestId"`, `"contextTokens"`, `"cache_read"`,
		`"roundTrips"`, `"avgContext"`, `"longestGapMs"`, `"longestGapAfterCall"`, `"cacheTtl":"1h"`, `"newContext"`,
		`"usdIfApplied"`, `"deltaUsd"`, `"guideSlug"`} {
		if !strings.Contains(string(data), k) {
			t.Errorf("JSON missing %s", k)
		}
	}
}

func TestAttribute_ReattributesAcrossPrompts(t *testing.T) {
	tb := table(t)
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	p1 := &Prompt{PromptID: "p1", StartedAt: t0, EndedAt: t0.Add(time.Minute)}
	p2 := &Prompt{PromptID: "p2", StartedAt: t0.Add(10 * time.Minute), EndedAt: t0.Add(11 * time.Minute)}
	tok := [4]int64{1, 10, 1000, 10}
	reqs := []Request{
		request(t, tb, "early", t0.Add(-time.Minute), "claude-opus-5-5", tok),       // before any prompt
		request(t, tb, "a", t0.Add(10*time.Second), "claude-opus-5-5", tok, "Bash"), // p1
		// p2's Stop carried p1's final call (the flush race): ts says p1.
		request(t, tb, "b", t0.Add(50*time.Second), "claude-opus-5-5", tok),
		request(t, tb, "wake", t0.Add(5*time.Minute), "claude-opus-5-5", tok), // after p1 ended
		request(t, tb, "c", t0.Add(10*time.Minute+5*time.Second), "claude-opus-5-5", tok),
		request(t, tb, "a", t0.Add(10*time.Second), "claude-opus-5-5", tok, "Read"), // duplicate sighting
	}
	Attribute([]*Prompt{p1, p2}, reqs)

	ids := func(p *Prompt) []string {
		var out []string
		for _, r := range p.Requests {
			out = append(out, r.RequestID)
		}
		return out
	}
	if got := strings.Join(ids(p1), ","); got != "a,b,wake" {
		t.Fatalf("p1 requests = %s, want a,b,wake", got)
	}
	if got := strings.Join(ids(p2), ","); got != "c" {
		t.Fatalf("p2 requests = %s, want c", got)
	}
	if !p1.AfterPrompt["wake"] || p1.AfterPrompt["b"] {
		t.Errorf("afterPrompt = %v, want only wake", p1.AfterPrompt)
	}
	if n := p1.Requests[0].ToolNames; n["Bash"] != 1 || n["Read"] != 1 {
		t.Errorf("duplicate sighting should merge tools: %v", n)
	}

	l, _ := BuildLedger(p1, tb)
	if l.RoundTrips != 2 || !l.Calls[2].AfterPrompt || l.Calls[2].RequestID != "wake" {
		t.Fatalf("ledger calls = %+v", l.Calls)
	}
	advice := BuildAdvice(l, tb, nil)
	wake := findAdvice(advice, AdviceAfterPromptWake)
	if wake == nil || math.Abs(wake.USDIfApplied-l.Calls[2].USD.Total) > 1e-12 {
		t.Errorf("after-prompt-wake = %+v", wake)
	}
}

func TestAttachTools_UsesResultTiming(t *testing.T) {
	tb := table(t)
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tok := [4]int64{1, 10, 1000, 10}
	p := &Prompt{PromptID: "p", StartedAt: t0}
	Attribute([]*Prompt{p}, []Request{
		request(t, tb, "a", t0.Add(1*time.Second), "claude-opus-5-5", tok, "Bash"),
		request(t, tb, "b", t0.Add(20*time.Second), "claude-opus-5-5", tok, "mcp__github__get_issue"),
		request(t, tb, "c", t0.Add(40*time.Second), "claude-opus-5-5", tok),
	})
	// Bash finished at +10s after 4s: started at +6s, so it belongs to call a.
	p.ToolResults = []ToolResult{{At: t0.Add(10 * time.Second), Name: "Bash", OK: false, DurationMs: 4000}}
	l, _ := BuildLedger(p, tb)
	a := l.Calls[0].Tools
	if len(a) != 1 || a[0].Name != "Bash" || a[0].OK == nil || *a[0].OK || a[0].DurationMs != 4000 {
		t.Errorf("call a tools = %+v", a)
	}
	b := l.Calls[1].Tools
	if len(b) != 1 || b[0].MCPServer != "github" || b[0].OK != nil {
		t.Errorf("call b tools (names-only fallback) = %+v", b)
	}
	if l.Calls[2].Tools == nil || len(l.Calls[2].Tools) != 0 {
		t.Errorf("call c tools = %#v, want empty non-nil", l.Calls[2].Tools)
	}
}

func TestCollapsedRoundTrips(t *testing.T) {
	mk := func(names ...string) Call {
		c := Call{Tools: []Tool{}}
		for _, n := range names {
			c.Tools = append(c.Tools, Tool{Name: n})
		}
		return c
	}
	cases := []struct {
		calls []Call
		want  int
	}{
		{[]Call{mk("Read"), mk("Grep"), mk("Read")}, 1},
		{[]Call{mk("Read"), mk("Edit"), mk("Read")}, 1}, // any single tool, no read-only judgement
		{[]Call{mk("Read", "Edit"), mk("Read")}, 2},
		{[]Call{mk("Read"), mk("Read", "Edit"), mk("Read")}, 3},
		{[]Call{mk(), mk(), mk()}, 3},
		{[]Call{mk("Bash"), mk("ToolSearch"), mk("Write"), mk("Bash"), mk("Bash"), mk()}, 2},
	}
	for i, c := range cases {
		if got := CollapsedRoundTrips(c.calls); got != c.want {
			t.Errorf("case %d: got %d, want %d", i, got, c.want)
		}
	}
}

func TestSiblingModel(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-5-5": "claude-sonnet-5", "claude-fable-5-1": "claude-sonnet-5",
		"claude-sonnet-5-5": "claude-haiku-4-5", "gpt-5": "",
	} {
		if got := SiblingModel(model); got != want {
			t.Errorf("SiblingModel(%s) = %q, want %q", model, got, want)
		}
	}
}
