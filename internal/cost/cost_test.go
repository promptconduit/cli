package cost

import (
	"bytes"
	"encoding/json"
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
	// Cursor's own models resolve, and the -fast variant must NOT collapse to
	// the cheaper Standard rate via prefix-trim (the exact key takes priority).
	fast, ok := tbl.ResolvePrice("composer-2.5-fast")
	if !ok {
		t.Fatal("composer-2.5-fast should resolve")
	}
	std, ok := tbl.ResolvePrice("composer-2.5")
	if !ok {
		t.Fatal("composer-2.5 should resolve")
	}
	if fast.Input != 0.000003 || std.Input != 0.0000005 {
		t.Fatalf("composer rates wrong: fast input=%v (want 3e-6), std input=%v (want 5e-7)", fast.Input, std.Input)
	}
	if fast.Input == std.Input {
		t.Fatal("composer-2.5-fast must not resolve to the Standard rate")
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

	// composer-2.5-fast is priced from Cursor's published input/output rates
	// ($3/$15 per M); Cursor publishes no cache rate, so cache_read is priced 0.
	composer := []byte(`{"hook_event_name":"afterAgentResponse","model":"composer-2.5-fast","conversation_id":"conv1","generation_id":"gen1","session_id":"conv1","input_tokens":17072,"output_tokens":92,"cache_read_tokens":6048,"cache_write_tokens":0,"workspace_roots":["/Users/x/tolken"]}`)
	ev, cwd, ok := ParseCursorHookPayload(composer, tbl)
	if !ok {
		t.Fatal("afterAgentResponse with tokens should parse")
	}
	if ev.Tool != ToolCursor || ev.SessionID != "conv1" || ev.RequestID != "gen1" {
		t.Fatalf("unexpected cursor event: %+v", ev)
	}
	if !ev.ModelPriced {
		t.Fatal("composer-2.5-fast should be priced")
	}
	const wantComposer = 17072*3e-6 + 92*15e-6 // cache_read priced at 0 (no Cursor cache rate)
	if math.Abs(ev.Cost.Total-wantComposer) > 1e-9 {
		t.Fatalf("composer cost = %v, want %v", ev.Cost.Total, wantComposer)
	}
	if ev.Tokens.Input != 17072 || ev.Tokens.Output != 92 || ev.Tokens.CacheRead != 6048 {
		t.Fatalf("tokens not read exactly: %+v", ev.Tokens)
	}
	if cwd != "/Users/x/tolken" || ev.CwdBase != "tolken" {
		t.Fatalf("cwd handling wrong: cwd=%q base=%q", cwd, ev.CwdBase)
	}

	// A genuinely unknown model is reported with exact tokens but unpriced
	// (cost 0, flagged) rather than guessed.
	unknown := []byte(`{"hook_event_name":"stop","model":"totally-unknown-model","conversation_id":"c3","generation_id":"g3","input_tokens":10,"output_tokens":5,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`)
	evU, _, ok := ParseCursorHookPayload(unknown, tbl)
	if !ok {
		t.Fatal("unknown-model payload should still parse")
	}
	if evU.ModelPriced || evU.Cost.Total != 0 {
		t.Fatalf("unknown model should be unpriced with cost 0, got priced=%v cost=%v", evU.ModelPriced, evU.Cost.Total)
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

// TestCostEventGroupingKeys is a regression guard for issue #72: the editor
// extension groups cost by "agent tab" using (session_id, conversation_id) and
// dedups turns by request_id, so every emitted CostEvent must carry the keys
// its source provides — and they must survive JSON serialization, since the
// extension reads `cost watch --json` lines, not Go structs.
func TestCostEventGroupingKeys(t *testing.T) {
	tbl := mustTable(t)

	// Cursor sends distinct conversation_id, generation_id, and session_id.
	// All three must land on the event as distinct keys (conversation_id must
	// NOT be conflated into session_id).
	cursorRaw := []byte(`{"hook_event_name":"stop","model":"claude-opus-4-8","conversation_id":"conv_42","generation_id":"gen_99","session_id":"sess_7","input_tokens":100,"output_tokens":50,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`)
	cur, _, ok := ParseCursorHookPayload(cursorRaw, tbl)
	if !ok {
		t.Fatal("cursor payload should parse")
	}
	if cur.SessionID != "sess_7" {
		t.Fatalf("cursor session_id = %q, want sess_7", cur.SessionID)
	}
	if cur.ConversationID != "conv_42" {
		t.Fatalf("cursor conversation_id = %q, want conv_42", cur.ConversationID)
	}
	if cur.RequestID != "gen_99" {
		t.Fatalf("cursor request_id = %q, want gen_99 (generation_id)", cur.RequestID)
	}
	// conversation_id and session_id must be carried separately, not aliased.
	if cur.ConversationID == cur.SessionID {
		t.Fatal("cursor conversation_id and session_id must be distinct keys")
	}

	// The JSON the extension consumes must expose the grouping keys.
	assertGroupingJSON(t, cur, "conv_42", "sess_7", "gen_99")

	// When Cursor omits session_id, fall back to conversation_id so the
	// session grouping key is never empty.
	noSess := []byte(`{"hook_event_name":"stop","model":"claude-opus-4-8","conversation_id":"conv_only","generation_id":"gen_x","input_tokens":10,"output_tokens":5,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`)
	curNS, _, ok := ParseCursorHookPayload(noSess, tbl)
	if !ok {
		t.Fatal("cursor payload without session_id should still parse")
	}
	if curNS.SessionID != "conv_only" || curNS.ConversationID != "conv_only" {
		t.Fatalf("missing session_id should fall back to conversation_id: %+v", curNS)
	}

	// Claude Code carries session_id and request_id; it has no separate
	// conversation id, so conversation_id is empty and omitted from JSON.
	ccRaw := []byte(`{"type":"assistant","requestId":"req_abc","sessionId":"sess_cc","timestamp":"2026-06-13T12:00:00Z","cwd":"/Users/x/tolken","message":{"model":"claude-opus-4-8","usage":{"input_tokens":72,"output_tokens":3674,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)
	cc, _, ok := parseClaudeCodeLine(ccRaw, tbl, "fallback")
	if !ok {
		t.Fatal("claude code line should parse")
	}
	if cc.SessionID != "sess_cc" || cc.RequestID != "req_abc" {
		t.Fatalf("claude code grouping keys wrong: session=%q request=%q", cc.SessionID, cc.RequestID)
	}
	if cc.ConversationID != "" {
		t.Fatalf("claude code has no conversation id; got %q", cc.ConversationID)
	}
	data, err := json.Marshal(cc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "conversation_id") {
		t.Fatalf("empty conversation_id must be omitted from JSON: %s", data)
	}
}

// assertGroupingJSON marshals an event and checks the grouping keys round-trip
// with the exact JSON field names the extension reads.
func assertGroupingJSON(t *testing.T, ev CostEvent, wantConv, wantSess, wantReq string) {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		ConversationID string `json:"conversation_id"`
		SessionID      string `json:"session_id"`
		RequestID      string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ConversationID != wantConv || got.SessionID != wantSess || got.RequestID != wantReq {
		t.Fatalf("grouping keys in JSON = %+v; want conv=%q sess=%q req=%q", got, wantConv, wantSess, wantReq)
	}
}

// TestWatcherCursorAggregationDedup is the end-to-end check for the behavior
// verified by hand against real payloads: a watcher ingesting Cursor cost
// events dedups stop + afterAgentResponse (same generation_id, identical
// tokens) to one billable unit, sums across generations, and prices composer.
// TestLoadPriceTableMerge verifies the refresh-cache layering: curated rates
// win, the cache fills in models the curated table lacks, and free/non-chat
// entries are skipped.
func TestLoadPriceTableMerge(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate StoreDir() from the real config

	cache := `{
      "claude-opus-4-8": {"input_cost_per_token": 0.999, "output_cost_per_token": 0.999},
      "gpt-5.5": {"input_cost_per_token": 0.000002, "output_cost_per_token": 0.000008},
      "text-embedding-x": {"input_cost_per_token": 0, "output_cost_per_token": 0}
    }`
	path := CachedPricingPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}

	tbl, err := LoadPriceTable()
	if err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}

	// Curated entry must win over the cache's bogus override.
	opus, ok := tbl.ResolvePrice("claude-opus-4-8")
	if !ok || opus.Input != 0.000005 {
		t.Fatalf("curated opus rate should win; got %v ok=%v", opus.Input, ok)
	}
	// A model only in the cache is added.
	gpt, ok := tbl.ResolvePrice("gpt-5.5")
	if !ok || gpt.Input != 0.000002 {
		t.Fatalf("cache-only model should resolve; got %v ok=%v", gpt.Input, ok)
	}
	// Free/non-chat entries are skipped.
	if _, ok := tbl.ResolvePrice("text-embedding-x"); ok {
		t.Fatal("zero-cost model should be skipped from the merge")
	}
}

func TestWatcherCursorAggregationDedup(t *testing.T) {
	tbl := mustTable(t)
	var buf bytes.Buffer
	w := NewWatcher(tbl, nil, &buf, true)

	// Two generations; each fires afterAgentResponse + stop with identical
	// tokens and the same generation_id, exactly as real Cursor does.
	payloads := []string{
		`{"hook_event_name":"afterAgentResponse","model":"composer-2.5-fast","conversation_id":"c","generation_id":"g1","input_tokens":100,"output_tokens":10,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`,
		`{"hook_event_name":"stop","model":"composer-2.5-fast","conversation_id":"c","generation_id":"g1","input_tokens":100,"output_tokens":10,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`,
		`{"hook_event_name":"afterAgentResponse","model":"composer-2.5-fast","conversation_id":"c","generation_id":"g2","input_tokens":200,"output_tokens":20,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`,
		`{"hook_event_name":"stop","model":"composer-2.5-fast","conversation_id":"c","generation_id":"g2","input_tokens":200,"output_tokens":20,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`,
	}
	for _, p := range payloads {
		ev, _, ok := ParseCursorHookPayload([]byte(p), tbl)
		if !ok {
			t.Fatal("payload should parse")
		}
		data, _ := json.Marshal(ev)
		w.streamCostEventLine(data)
	}

	// Parse the last session_summary emitted and assert the deduped totals.
	var last *SessionSummary
	for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(line, &probe) == nil && probe.Kind == "session_summary" {
			var s SessionSummary
			if json.Unmarshal(line, &s) == nil {
				last = &s
			}
		}
	}
	if last == nil {
		t.Fatal("expected at least one session_summary")
	}
	// g1 (100/10) + g2 (200/20), each counted once despite the stop duplicate.
	if last.Totals.Input != 300 || last.Totals.Output != 30 {
		t.Fatalf("deduped totals wrong: input=%d output=%d, want 300/30", last.Totals.Input, last.Totals.Output)
	}
	const wantCost = 300*3e-6 + 30*15e-6 // composer-2.5-fast: 0.0009 + 0.00045
	if math.Abs(last.Totals.CostTotal-wantCost) > 1e-9 {
		t.Fatalf("cost = %v, want %v", last.Totals.CostTotal, wantCost)
	}
	if last.Source != SourceExact || last.Tool != ToolCursor {
		t.Fatalf("unexpected source/tool: %s/%s", last.Source, last.Tool)
	}
}

// TestLatestSummary verifies the one-shot `cost` path picks the most-recently
// updated session and reports its total.
func TestLatestSummary(t *testing.T) {
	w := NewWatcher(nil, nil, nil, false) // table/out unused by apply + LatestSummary
	w.apply(CostEvent{Kind: "cost_event", Tool: ToolCursor, SessionID: "old", RequestID: "g1", Timestamp: "2026-06-13T10:00:00Z", Model: "m", ModelPriced: true, Cost: Cost{Total: 1}})
	w.apply(CostEvent{Kind: "cost_event", Tool: ToolCursor, SessionID: "new", RequestID: "g2", Timestamp: "2026-06-13T11:00:00Z", Model: "m", ModelPriced: true, Cost: Cost{Total: 2}})

	s, ok := w.LatestSummary()
	if !ok || s.SessionID != "new" {
		t.Fatalf("want latest session 'new'; got ok=%v id=%q", ok, s.SessionID)
	}
	if s.Totals.CostTotal != 2 || len(s.ByModel) != 1 {
		t.Fatalf("unexpected summary: total=%v models=%d", s.Totals.CostTotal, len(s.ByModel))
	}

	empty := NewWatcher(nil, nil, nil, false)
	if _, ok := empty.LatestSummary(); ok {
		t.Fatal("LatestSummary should report ok=false with no sessions")
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
