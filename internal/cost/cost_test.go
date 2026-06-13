package cost

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustTable(t *testing.T) *PriceTable {
	t.Helper()
	tbl, err := LoadBundledPriceTable()
	if err != nil {
		t.Fatalf("load bundled table: %v", err)
	}
	return tbl
}

func TestResolvePrice(t *testing.T) {
	tbl := mustTable(t)

	if _, ok := tbl.ResolvePrice("claude-opus-4-8"); !ok {
		t.Fatal("exact key claude-opus-4-8 should resolve")
	}
	// Dated/suffixed variant should collapse to the base model via prefix trim.
	if _, ok := tbl.ResolvePrice("claude-opus-4-8-20260101"); !ok {
		t.Fatal("dated variant should resolve to base model")
	}
	// Alias map.
	if _, ok := tbl.ResolvePrice("claude-3-5-haiku-20241022"); !ok {
		t.Fatal("aliased haiku id should resolve")
	}
	// Unknown model must report not-priced, not panic or guess.
	if _, ok := tbl.ResolvePrice("some-other-llm-9"); ok {
		t.Fatal("unknown model should not resolve")
	}
	if _, ok := tbl.ResolvePrice(""); ok {
		t.Fatal("empty model should not resolve")
	}
}

// TestCostForUsage_RealBlock checks the exact dollar math against a real
// Claude Code usage block captured from a live transcript:
//
//	input 72, output 3674, cache_read 132405, cache_creation 19640 (all 1h),
//	model claude-opus-4-8.
//
// At Opus 4.8 rates (in $5, out $25, cache-read $0.50, cache-write-1h $10 per
// 1M) the total is $0.3548125 — and the 1h tier is what makes it right; pricing
// those 19,640 tokens at the 5m rate ($6.25/1M) would undercount.
func TestCostForUsage_RealBlock(t *testing.T) {
	tbl := mustTable(t)
	mp, ok := tbl.ResolvePrice("claude-opus-4-8")
	if !ok {
		t.Fatal("opus-4-8 must resolve")
	}
	u := Usage{
		InputTokens:          72,
		OutputTokens:         3674,
		CacheReadInputTokens: 132405,
		CacheCreationInput:   19640,
		Ephemeral1hTokens:    19640,
		Ephemeral5mTokens:    0,
	}
	c, cacheWriteTokens := CostForUsage(u, mp)

	if cacheWriteTokens != 19640 {
		t.Fatalf("cache-write tokens = %d, want 19640", cacheWriteTokens)
	}
	const want = 0.00036 + 0.09185 + 0.0662025 + 0.1964 // = 0.3548125
	if math.Abs(c.Total-want) > 1e-9 {
		t.Fatalf("total cost = %.10f, want %.10f", c.Total, want)
	}
	// The 1h cache-write component specifically must use the 2x rate.
	if math.Abs(c.CacheWrite-0.1964) > 1e-9 {
		t.Fatalf("cache-write cost = %.10f, want 0.1964 (1h rate)", c.CacheWrite)
	}
}

func TestCostForUsage_NoSplitFallsBackTo5m(t *testing.T) {
	tbl := mustTable(t)
	mp, _ := tbl.ResolvePrice("claude-opus-4-8")
	// No ephemeral split provided -> lumped total priced at the 5m rate.
	u := Usage{CacheCreationInput: 1000}
	c, tokens := CostForUsage(u, mp)
	if tokens != 1000 {
		t.Fatalf("tokens = %d, want 1000", tokens)
	}
	if math.Abs(c.CacheWrite-0.00625) > 1e-12 { // 1000 * $6.25/1M
		t.Fatalf("cache-write = %.12f, want 0.00625 (5m rate)", c.CacheWrite)
	}
}

func TestParseClaudeCodeLine(t *testing.T) {
	tbl := mustTable(t)
	line := []byte(`{"type":"assistant","requestId":"req_abc","sessionId":"sess_1","timestamp":"2026-06-13T12:00:00Z","cwd":"/Users/x/tolken","message":{"model":"claude-opus-4-8","usage":{"input_tokens":72,"output_tokens":3674,"cache_read_input_tokens":132405,"cache_creation_input_tokens":19640,"cache_creation":{"ephemeral_1h_input_tokens":19640,"ephemeral_5m_input_tokens":0}}}}`)
	ev, key, ok := parseClaudeCodeLine(line, tbl, "fallback")
	if !ok {
		t.Fatal("assistant line with usage should parse")
	}
	if key != "req_abc" {
		t.Fatalf("dedup key = %q, want req_abc", key)
	}
	if ev.SessionID != "sess_1" || ev.Model != "claude-opus-4-8" || !ev.ModelPriced {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.CwdBase != "tolken" {
		t.Fatalf("cwd_base = %q, want tolken (basename only)", ev.CwdBase)
	}
	if ev.Tokens.Output != 3674 {
		t.Fatalf("output tokens = %d, want 3674", ev.Tokens.Output)
	}

	// A user line (no usage) must be skipped.
	if _, _, ok := parseClaudeCodeLine([]byte(`{"type":"user","message":{"content":"hi"}}`), tbl, "fallback"); ok {
		t.Fatal("user line should not parse as a cost event")
	}
}

// TestParseCursorHookPayload uses the real Cursor hook shape captured from the
// M0 probe: top-level token fields, model, conversation/generation ids.
func TestParseCursorHookPayload(t *testing.T) {
	tbl := mustTable(t)

	// composer-* is a Cursor-proprietary model not in the rate table: exact
	// tokens, but unpriced (cost 0, flagged) rather than a guessed rate.
	composer := []byte(`{"hook_event_name":"afterAgentResponse","model":"composer-2.5-fast","conversation_id":"conv1","generation_id":"gen1","session_id":"conv1","input_tokens":17072,"output_tokens":92,"cache_read_tokens":6048,"cache_write_tokens":0,"workspace_roots":["/Users/x/tolken"]}`)
	ev, cwd, ok := ParseCursorHookPayload(composer, tbl)
	if !ok {
		t.Fatal("afterAgentResponse with tokens should parse")
	}
	if ev.Tool != ToolCursor || ev.SessionID != "conv1" || ev.RequestID != "gen1" {
		t.Fatalf("unexpected cursor event: %+v", ev)
	}
	if ev.ModelPriced {
		t.Fatal("composer-2.5-fast should be unpriced")
	}
	if ev.Cost.Total != 0 {
		t.Fatalf("unpriced model cost should be 0, got %v", ev.Cost.Total)
	}
	if ev.Tokens.Input != 17072 || ev.Tokens.Output != 92 || ev.Tokens.CacheRead != 6048 {
		t.Fatalf("tokens not read exactly: %+v", ev.Tokens)
	}
	if cwd != "/Users/x/tolken" || ev.CwdBase != "tolken" {
		t.Fatalf("cwd handling wrong: cwd=%q base=%q", cwd, ev.CwdBase)
	}

	// A known passthrough model prices exactly.
	known := []byte(`{"hook_event_name":"stop","model":"claude-opus-4-8","conversation_id":"c2","generation_id":"g2","input_tokens":1000,"output_tokens":500,"cache_read_tokens":2000,"cache_write_tokens":0,"workspace_roots":["/p"]}`)
	ev2, _, ok := ParseCursorHookPayload(known, tbl)
	if !ok || !ev2.ModelPriced {
		t.Fatal("known model should parse and be priced")
	}
	const want = 1000*5e-6 + 500*25e-6 + 2000*0.5e-6 // 0.0185
	if math.Abs(ev2.Cost.Total-want) > 1e-9 {
		t.Fatalf("priced cursor cost = %v, want %v", ev2.Cost.Total, want)
	}

	// Non-token events and zero-token payloads are skipped.
	if _, _, ok := ParseCursorHookPayload([]byte(`{"hook_event_name":"beforeSubmitPrompt","prompt":"hi"}`), tbl); ok {
		t.Fatal("non-token event should be skipped")
	}
	if _, _, ok := ParseCursorHookPayload([]byte(`{"hook_event_name":"stop","model":"x","input_tokens":0,"output_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0}`), tbl); ok {
		t.Fatal("zero-token payload should be skipped")
	}
}

func TestEncodeProjectPath(t *testing.T) {
	got := encodeProjectPath("/Users/scotthavird/Documents/GitHub/scotthavird/tolken")
	want := "-Users-scotthavird-Documents-GitHub-scotthavird-tolken"
	if got != want {
		t.Fatalf("encodeProjectPath = %q, want %q", got, want)
	}
	// Dots in repo names also become dashes (e.g. havoptic.com).
	if got := encodeProjectPath("/a/havoptic.com"); got != "-a-havoptic-com" {
		t.Fatalf("dot encoding = %q, want -a-havoptic-com", got)
	}
}

// TestPrivacy_NoPlatformSendInCostPackage is the load-bearing privacy
// guarantee: the cost package must never reference the platform send path. If
// any of these symbols appear in a non-test .go file here, the feature could
// exfiltrate data and this test fails.
func TestPrivacy_NoPlatformSendInCostPackage(t *testing.T) {
	forbidden := []string{
		"SendEnvelope",
		"DefaultAPIURL",
		"client.NewClient",
		"apiClient",
		"api_url",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, sym := range forbidden {
			if strings.Contains(string(data), sym) {
				t.Errorf("%s references forbidden platform-send symbol %q — the cost feature must stay local-only", name, sym)
			}
		}
	}
}
