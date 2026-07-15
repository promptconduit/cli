package cost

import (
	"bytes"
	"encoding/json"
	"io"
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

// TestResolvePrice_CurrentClaudeFamily pins every model in the current Claude
// lineup to its list input rate. ResolvePrice's fallbacks can't rescue a model
// that's simply absent from the table — "claude-sonnet-5" trims to
// "claude-sonnet" then "claude", neither of which is a key — so a new model that
// misses the table resolves ok=false and is silently charged $0 with
// ModelPriced=false (cli#123). This table is the guard: adding a model to the
// lineup without adding its rates fails here.
func TestResolvePrice_CurrentClaudeFamily(t *testing.T) {
	tbl := mustTable(t)

	cases := []struct {
		model     string
		wantInput float64 // USD per input token
	}{
		{"claude-sonnet-5", 0.000003},
		{"claude-fable-5", 0.00001},
		{"claude-mythos-5", 0.00001},
		{"claude-opus-4-8", 0.000005},
		{"claude-sonnet-4-6", 0.000003},
		{"claude-haiku-4-5", 0.000001},
	}
	for _, tc := range cases {
		mp, ok := tbl.ResolvePrice(tc.model)
		if !ok {
			t.Errorf("ResolvePrice(%q) = not priced; every current Claude model must resolve", tc.model)
			continue
		}
		if mp.Input != tc.wantInput {
			t.Errorf("ResolvePrice(%q).Input = %v, want %v", tc.model, mp.Input, tc.wantInput)
		}
		// A dated variant from a real hook payload must reach the same rates.
		dated := tc.model + "-20260101"
		datedMP, ok := tbl.ResolvePrice(dated)
		if !ok {
			t.Errorf("ResolvePrice(%q) = not priced; dated variant should trim to the base model", dated)
			continue
		}
		if datedMP.Input != tc.wantInput {
			t.Errorf("ResolvePrice(%q).Input = %v, want %v", dated, datedMP.Input, tc.wantInput)
		}
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

// TestSummarizeToolNames covers the names-only roll-up helper: counts, the
// per-name map, empty-name skipping, and the empty input case.
func TestSummarizeToolNames(t *testing.T) {
	s := summarizeToolNames([]string{"Read", "Bash", "Read", ""})
	if s.Total != 3 {
		t.Fatalf("total = %d, want 3 (empty name skipped)", s.Total)
	}
	if s.ByName["Read"] != 2 || s.ByName["Bash"] != 1 {
		t.Fatalf("by_name = %v, want Read:2 Bash:1", s.ByName)
	}
	if _, ok := s.ByName[""]; ok {
		t.Fatal("empty tool name must never be recorded")
	}

	empty := summarizeToolNames(nil)
	if empty.Total != 0 || empty.ByName != nil {
		t.Fatalf("no tools should yield zero summary, got %+v", empty)
	}
}

// TestParseClaudeCodeLine_ToolSummary verifies the per-request tool-call
// summary is derived from the assistant turn's tool_use blocks (names only),
// from the same transcript line the cost event already parses.
func TestParseClaudeCodeLine_ToolSummary(t *testing.T) {
	tbl := mustTable(t)

	// Two tool_use blocks (Read twice) plus a text block, all in one turn.
	line := []byte(`{"type":"assistant","requestId":"req_tools","sessionId":"s","timestamp":"2026-06-24T00:00:00Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"on it"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/etc/passwd"}},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/secret"}}],"usage":{"input_tokens":10,"output_tokens":20}}}`)
	ev, _, ok := parseClaudeCodeLine(line, tbl, "fallback")
	if !ok {
		t.Fatal("assistant line with usage should parse")
	}
	if ev.Tools.Total != 2 {
		t.Fatalf("tools total = %d, want 2", ev.Tools.Total)
	}
	if ev.Tools.ByName["Read"] != 2 {
		t.Fatalf("Read count = %d, want 2", ev.Tools.ByName["Read"])
	}

	// Privacy invariant: serialized event must not leak any tool input. The
	// token-count fields legitimately use the key "input", so we assert on the
	// tool-input KEY ("file_path") and its VALUES (the actual paths) instead.
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"file_path", "/etc/passwd", "/secret"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Fatalf("serialized cost event leaked tool content %q: %s", leak, data)
		}
	}

	// A turn with no tool_use blocks yields an empty (omitted) summary.
	noTools := []byte(`{"type":"assistant","requestId":"req_none","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	evN, _, ok := parseClaudeCodeLine(noTools, tbl, "fallback")
	if !ok {
		t.Fatal("text-only assistant line should still parse for cost")
	}
	if evN.Tools.Total != 0 || evN.Tools.ByName != nil {
		t.Fatalf("no-tool turn should have empty summary, got %+v", evN.Tools)
	}
}

// TestSessionSummaryToolAggregation verifies per-request tool summaries roll
// up into the session-level SessionSummary.Tools the watcher emits.
func TestSessionSummaryToolAggregation(t *testing.T) {
	w := NewWatcher(nil, nil, nil, false)
	w.apply(CostEvent{
		Kind: "cost_event", Tool: ToolClaudeCode, SessionID: "s", RequestID: "r1",
		Timestamp: "2026-06-24T00:00:00Z", Model: "m",
		Tools: summarizeToolNames([]string{"Read", "Bash"}),
	})
	w.apply(CostEvent{
		Kind: "cost_event", Tool: ToolClaudeCode, SessionID: "s", RequestID: "r2",
		Timestamp: "2026-06-24T00:00:01Z", Model: "m",
		Tools: summarizeToolNames([]string{"Read"}),
	})

	s, ok := w.LatestSummary()
	if !ok {
		t.Fatal("expected a session summary")
	}
	if s.Tools.Total != 3 {
		t.Fatalf("session tool total = %d, want 3", s.Tools.Total)
	}
	if s.Tools.ByName["Read"] != 2 || s.Tools.ByName["Bash"] != 1 {
		t.Fatalf("aggregated by_name = %v, want Read:2 Bash:1", s.Tools.ByName)
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

// TestWatchStreamCarriesGroupingKeys is the end-to-end guard for issue #72's
// acceptance criterion: the extension does not read CostEvent structs, it reads
// `cost watch --json` lines off the watcher's stream. So drive a Cursor event
// through the same emit path `cost watch` uses (streamCostEventLine -> emitJSON)
// and assert the emitted cost_event line carries conversation_id, session_id,
// and request_id with the exact JSON field names.
func TestWatchStreamCarriesGroupingKeys(t *testing.T) {
	tbl := mustTable(t)
	var buf bytes.Buffer
	w := NewWatcher(tbl, nil, &buf, true) // emit per-turn cost events, as `cost watch` does

	raw := []byte(`{"hook_event_name":"stop","model":"claude-opus-4-8","conversation_id":"conv_42","generation_id":"gen_99","session_id":"sess_7","input_tokens":100,"output_tokens":50,"cache_read_tokens":0,"cache_write_tokens":0,"workspace_roots":["/p"]}`)
	ev, _, ok := ParseCursorHookPayload(raw, tbl)
	if !ok {
		t.Fatal("cursor payload should parse")
	}
	data, _ := json.Marshal(ev)
	w.streamCostEventLine(data) // exactly what the Cursor feed tail does

	// Find the emitted cost_event line and assert its grouping keys, parsing it
	// the way the extension would (raw JSON, exact field names).
	var found bool
	for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Kind           string `json:"kind"`
			ConversationID string `json:"conversation_id"`
			SessionID      string `json:"session_id"`
			RequestID      string `json:"request_id"`
		}
		if json.Unmarshal(line, &probe) != nil || probe.Kind != "cost_event" {
			continue
		}
		found = true
		if probe.ConversationID != "conv_42" || probe.SessionID != "sess_7" || probe.RequestID != "gen_99" {
			t.Fatalf("emitted cost_event line missing grouping keys: %s", line)
		}
	}
	if !found {
		t.Fatalf("no cost_event line emitted on the watch stream; got:\n%s", buf.Bytes())
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

// approx is a small float tolerance helper for the signal-ratio assertions.
func approx(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// TestSignalFormulas pins the three derived ratios against hand-computed values
// and exercises the divide-by-zero guards. The cache-hit formula is the one
// called out in #71: cache_read / (cache_read + cache_creation + input).
func TestSignalFormulas(t *testing.T) {
	// input 100, cache_read 700, cache_creation 200 -> denom 1000.
	if got := cacheHitRate(100, 700, 200); !approx(got, 0.7) {
		t.Fatalf("cacheHitRate = %v, want 0.7", got)
	}
	if got := inputTokenShare(100, 700, 200); !approx(got, 0.1) {
		t.Fatalf("inputTokenShare = %v, want 0.1", got)
	}
	// No input-side tokens at all -> 0, not NaN/panic.
	if got := cacheHitRate(0, 0, 0); got != 0 {
		t.Fatalf("cacheHitRate(0,0,0) = %v, want 0", got)
	}
	if got := inputTokenShare(0, 0, 0); got != 0 {
		t.Fatalf("inputTokenShare(0,0,0) = %v, want 0", got)
	}

	// cache-miss cost share = (input + cache_write) / total.
	c := Cost{Input: 2, Output: 1, CacheRead: 1, CacheWrite: 3, Total: 7}
	if got := cacheMissCostShare(c); !approx(got, 5.0/7.0) {
		t.Fatalf("cacheMissCostShare = %v, want %v", got, 5.0/7.0)
	}
	if got := cacheMissCostShare(Cost{Total: 0}); got != 0 {
		t.Fatalf("cacheMissCostShare with zero total = %v, want 0", got)
	}
}

// TestModelTier covers the coarse, names-only tier buckets, including the
// unpriced override (an unpriced model is always TierUnknown).
func TestModelTier(t *testing.T) {
	cases := []struct {
		model  string
		priced bool
		want   string
	}{
		{"claude-opus-4-8", true, TierPremium},
		{"gpt-5.5", true, TierPremium},
		{"claude-sonnet-4-5", true, TierStandard},
		{"composer-2.5-fast", true, TierStandard},
		{"claude-3-5-haiku", true, TierEconomy},
		{"gemini-2.0-flash", true, TierEconomy},
		{"gpt-4o-mini", true, TierEconomy},      // economy markers win over premium gpt-4
		{"claude-opus-4-8", false, TierUnknown}, // unpriced overrides everything
		{"", true, TierUnknown},
	}
	for _, tc := range cases {
		if got := modelTier(tc.model, tc.priced); got != tc.want {
			t.Errorf("modelTier(%q, priced=%v) = %q, want %q", tc.model, tc.priced, got, tc.want)
		}
	}
}

// TestParseClaudeCodeLine_Signals checks the per-request signals are wired onto
// the CostEvent from the same transcript line, with the exact ratios for a real
// usage block (input 72, cache_read 132405, cache_creation 19640).
func TestParseClaudeCodeLine_Signals(t *testing.T) {
	tbl := mustTable(t)
	line := []byte(`{"type":"assistant","requestId":"req_sig","sessionId":"s","timestamp":"2026-06-24T00:00:00Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"t1","name":"Bash"}],"usage":{"input_tokens":72,"output_tokens":3674,"cache_read_input_tokens":132405,"cache_creation_input_tokens":19640,"cache_creation":{"ephemeral_1h_input_tokens":19640,"ephemeral_5m_input_tokens":0}}}}`)
	ev, _, ok := parseClaudeCodeLine(line, tbl, "fallback")
	if !ok {
		t.Fatal("assistant line should parse")
	}
	denom := float64(72 + 132405 + 19640)
	wantHit := 132405.0 / denom
	wantInShare := 72.0 / denom
	if !approx(ev.Signals.CacheHitRate, wantHit) {
		t.Fatalf("cache_hit_rate = %v, want %v", ev.Signals.CacheHitRate, wantHit)
	}
	if !approx(ev.Signals.InputTokenShare, wantInShare) {
		t.Fatalf("input_token_share = %v, want %v", ev.Signals.InputTokenShare, wantInShare)
	}
	if ev.Signals.Tier != TierPremium || !ev.Signals.ModelPriced {
		t.Fatalf("tier/priced wrong: %q priced=%v", ev.Signals.Tier, ev.Signals.ModelPriced)
	}
	if ev.Signals.ToolCalls != 1 {
		t.Fatalf("tool_calls signal = %d, want 1 (mirrors Tools.Total)", ev.Signals.ToolCalls)
	}
	// cache-miss cost share must be in (0,1) for this priced, cache-heavy turn.
	if ev.Signals.CacheMissCostShare <= 0 || ev.Signals.CacheMissCostShare >= 1 {
		t.Fatalf("cache_miss_cost_share = %v, want in (0,1)", ev.Signals.CacheMissCostShare)
	}
}

// TestParseCursorHookPayload_Signals confirms Cursor events also carry signals
// (derived from the exact usage block) even though their tool summary is empty.
func TestParseCursorHookPayload_Signals(t *testing.T) {
	tbl := mustTable(t)
	payload := []byte(`{"hook_event_name":"stop","model":"composer-2.5-fast","conversation_id":"c","generation_id":"g","input_tokens":300,"output_tokens":50,"cache_read_tokens":700,"cache_write_tokens":0,"workspace_roots":["/p"]}`)
	ev, _, ok := ParseCursorHookPayload(payload, tbl)
	if !ok {
		t.Fatal("cursor payload should parse")
	}
	// denom = 300 + 700 + 0 = 1000 -> hit 0.7, input share 0.3.
	if !approx(ev.Signals.CacheHitRate, 0.7) {
		t.Fatalf("cursor cache_hit_rate = %v, want 0.7", ev.Signals.CacheHitRate)
	}
	if !approx(ev.Signals.InputTokenShare, 0.3) {
		t.Fatalf("cursor input_token_share = %v, want 0.3", ev.Signals.InputTokenShare)
	}
	if ev.Signals.Tier != TierStandard {
		t.Fatalf("composer tier = %q, want standard", ev.Signals.Tier)
	}
	if ev.Signals.ToolCalls != 0 {
		t.Fatalf("cursor tool_calls = %d, want 0 (no tool summary)", ev.Signals.ToolCalls)
	}
}

// TestSessionSignalsFromTotals verifies the session-level Signals are recomputed
// from the accumulated totals (not averaged per turn). Two turns with very
// different cache profiles must yield a signal derived from their SUM.
func TestSessionSignalsFromTotals(t *testing.T) {
	tbl := mustTable(t)
	w := NewWatcher(tbl, nil, io.Discard, false)

	// Turn 1: cache-cold (all input). Turn 2: cache-hot (all cache_read).
	cold, _, ok := parseClaudeCodeLine([]byte(`{"type":"assistant","requestId":"c1","sessionId":"s","timestamp":"2026-06-24T00:00:00Z","cwd":"/p","message":{"model":"claude-opus-4-8","usage":{"input_tokens":1000,"output_tokens":10}}}`), tbl, "s")
	if !ok {
		t.Fatal("cold turn should parse")
	}
	hot, _, ok := parseClaudeCodeLine([]byte(`{"type":"assistant","requestId":"c2","sessionId":"s","timestamp":"2026-06-24T00:00:01Z","cwd":"/p","message":{"model":"claude-opus-4-8","usage":{"input_tokens":0,"output_tokens":10,"cache_read_input_tokens":3000}}}`), tbl, "s")
	if !ok {
		t.Fatal("hot turn should parse")
	}
	w.apply(cold)
	w.apply(hot)

	s, ok := w.LatestSummary()
	if !ok {
		t.Fatal("expected a session summary")
	}
	// Session cache_read 3000, input 1000, cache_creation 0 -> denom 4000.
	wantHit := 3000.0 / 4000.0
	if !approx(s.Signals.CacheHitRate, wantHit) {
		t.Fatalf("session cache_hit_rate = %v, want %v (from summed totals)", s.Signals.CacheHitRate, wantHit)
	}
	if !approx(s.Signals.InputTokenShare, 0.25) {
		t.Fatalf("session input_token_share = %v, want 0.25", s.Signals.InputTokenShare)
	}
	if s.Signals.Tier != TierPremium {
		t.Fatalf("session tier = %q, want premium (dominant model)", s.Signals.Tier)
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
