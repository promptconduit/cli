package enrich

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeEnricher struct {
	slug    string
	applies bool
	payload any
	err     error
	panics  bool
}

func (f fakeEnricher) Slug() string          { return f.slug }
func (f fakeEnricher) Applies(*Context) bool { return f.applies }
func (f fakeEnricher) Enrich(*Context) (any, error) {
	if f.panics {
		panic("boom")
	}
	return f.payload, f.err
}

// withRegistry swaps the global registry for the test's own set.
func withRegistry(t *testing.T, es ...Enricher) {
	t.Helper()
	prev := registry
	registry = es
	t.Cleanup(func() { registry = prev })
}

func TestRun_CollectsApplicableSlugs(t *testing.T) {
	withRegistry(t,
		fakeEnricher{slug: "a", applies: true, payload: map[string]int{"x": 1}},
		fakeEnricher{slug: "b", applies: false, payload: map[string]int{"x": 2}},
	)
	out := Run(&Context{})
	if _, ok := out["a"]; !ok {
		t.Error("applicable slug a missing")
	}
	if _, ok := out["b"]; ok {
		t.Error("non-applicable slug b present")
	}
}

func TestRun_IsolatesFailures(t *testing.T) {
	withRegistry(t,
		fakeEnricher{slug: "bad", applies: true, err: errors.New("nope")},
		fakeEnricher{slug: "panics", applies: true, panics: true},
		fakeEnricher{slug: "good", applies: true, payload: "ok"},
	)
	out := Run(&Context{})
	if len(out) != 1 {
		t.Fatalf("out = %v, want only good", out)
	}
	if string(out["good"]) != `"ok"` {
		t.Errorf("good = %s", out["good"])
	}
}

func TestRun_EmptyIsNil(t *testing.T) {
	withRegistry(t, fakeEnricher{slug: "a", applies: false})
	if out := Run(&Context{}); out != nil {
		t.Errorf("expected nil map, got %v", out)
	}
}

func TestPromptEnricher_AppliesToCursorBeforeSubmit(t *testing.T) {
	e := promptEnricher{}
	if !e.Applies(&Context{HookEvent: "beforeSubmitPrompt"}) {
		t.Fatal("prompt enricher must apply to beforeSubmitPrompt")
	}
}

func TestPromptEnricher_CountsPerSession(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })

	ctx := &Context{
		Tool:      "claude-code",
		HookEvent: "UserPromptSubmit",
		SessionID: "sess-prompt-test",
		RawEvent:  map[string]interface{}{"prompt": "fix the bug in auth"},
	}
	e := promptEnricher{}
	for want := 1; want <= 3; want++ {
		payload, err := e.Enrich(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p := payload.(PromptEnrichment)
		if p.Count != want {
			t.Errorf("count = %d, want %d", p.Count, want)
		}
		if p.Words != 5 || p.Chars != 19 {
			t.Errorf("words/chars = %d/%d, want 5/19", p.Words, p.Chars)
		}
	}
}

func TestCostEnricher_ClaudeCodeOffsets(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })

	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	assistantLine := `{"type":"assistant","requestId":"req-1","sessionId":"s1","timestamp":"2026-07-03T10:00:00Z","cwd":"/tmp","message":{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}}`
	if err := os.WriteFile(transcript, []byte(assistantLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := &Context{
		Tool:           "claude-code",
		HookEvent:      "Stop",
		SessionID:      "s1",
		TranscriptPath: transcript,
		RawEvent:       map[string]interface{}{},
	}
	e := costEnricher{}
	if !e.Applies(ctx) {
		t.Fatal("cost enricher must apply to claude-code Stop with a transcript")
	}

	payload, err := e.Enrich(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ce := payload.(*CostEnrichment)
	if len(ce.Requests) != 1 || ce.Requests[0].RequestID != "req-1" {
		t.Fatalf("requests = %+v, want one req-1", ce.Requests)
	}
	if ce.Requests[0].Tokens.Input != 100 || ce.Requests[0].Tokens.Output != 50 {
		t.Errorf("tokens = %+v", ce.Requests[0].Tokens)
	}
	if ce.Totals.USD <= 0 {
		t.Errorf("total usd = %f, want > 0 for a priced model", ce.Totals.USD)
	}

	// Second Stop with no new lines: offset consumed, no cost slug.
	payload, err = e.Enrich(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		t.Errorf("expected nil on no-new-lines, got %+v", payload)
	}

	// A new turn appends a new request; only it is picked up.
	line2 := `{"type":"assistant","requestId":"req-2","sessionId":"s1","timestamp":"2026-07-03T10:05:00Z","cwd":"/tmp","message":{"model":"claude-opus-4-8","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line2 + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	payload, err = e.Enrich(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ce = payload.(*CostEnrichment)
	if len(ce.Requests) != 1 || ce.Requests[0].RequestID != "req-2" {
		t.Fatalf("second turn requests = %+v, want only req-2", ce.Requests)
	}
}

func TestCostEnricher_Cursor(t *testing.T) {
	raw := []byte(`{"hook_event_name":"stop","cursor_version":"1.0","model":"claude-sonnet-5","conversation_id":"conv-1","generation_id":"gen-1","session_id":"cs-1","input_tokens":500,"output_tokens":100,"cache_read_tokens":2000,"cache_write_tokens":0,"workspace_roots":["/tmp/proj"]}`)
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	ctx := &Context{
		Tool:      "cursor",
		HookEvent: "stop",
		SessionID: "cs-1",
		RawEvent:  parsed,
		RawJSON:   raw,
	}
	e := costEnricher{}
	if !e.Applies(ctx) {
		t.Fatal("cost enricher must apply to cursor stop")
	}
	payload, err := e.Enrich(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ce := payload.(*CostEnrichment)
	if len(ce.Requests) != 1 {
		t.Fatalf("requests = %+v", ce.Requests)
	}
	r := ce.Requests[0]
	if r.RequestID != "gen-1" || r.ConversationID != "conv-1" {
		t.Errorf("ids = %q/%q", r.RequestID, r.ConversationID)
	}
	if r.Tokens.Input != 500 || r.Tokens.CacheRead != 2000 {
		t.Errorf("tokens = %+v", r.Tokens)
	}
}

func TestVCS_NormalizeRemote(t *testing.T) {
	cases := []struct {
		in       string
		wantSlug string
		wantURL  string
	}{
		{"git@github.com:promptconduit/cli.git", "promptconduit/cli", "https://github.com/promptconduit/cli"},
		{"https://github.com/promptconduit/cli", "promptconduit/cli", "https://github.com/promptconduit/cli"},
		{"https://user@gitlab.com/org/sub/repo.git", "org/sub/repo", "https://gitlab.com/org/sub/repo"},
		{"", "", ""},
	}
	for _, tc := range cases {
		slug, url := normalizeRemote(tc.in)
		if slug != tc.wantSlug || url != tc.wantURL {
			t.Errorf("normalizeRemote(%q) = %q,%q want %q,%q", tc.in, slug, url, tc.wantSlug, tc.wantURL)
		}
	}
}

func TestSubagentEnricher_StartStopJoin(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })

	// Fake subagent transcript with two priced requests.
	transcript := filepath.Join(t.TempDir(), "agent.jsonl")
	lines := `{"type":"assistant","requestId":"ar-1","timestamp":"2026-07-03T10:00:00Z","message":{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0}}}
{"type":"assistant","requestId":"ar-2","timestamp":"2026-07-03T10:00:05Z","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
`
	if err := os.WriteFile(transcript, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	e := subagentEnricher{}

	startCtx := &Context{
		Tool: "claude-code", HookEvent: "SubagentStart", SessionID: "sub-sess",
		RawEvent: map[string]interface{}{"agent_id": "a1", "agent_type": "Explore"},
	}
	if !e.Applies(startCtx) {
		t.Fatal("must apply to SubagentStart")
	}
	payload, err := e.Enrich(startCtx)
	if err != nil {
		t.Fatal(err)
	}
	start := payload.(*SubagentEnrichment)
	if start.Phase != "start" || start.AgentType != "Explore" || start.Concurrent != 1 {
		t.Errorf("start = %+v", start)
	}

	// A second concurrent agent bumps parallelism.
	payload, _ = e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "SubagentStart", SessionID: "sub-sess",
		RawEvent: map[string]interface{}{"agent_id": "a2", "agent_type": "Plan"},
	})
	if payload.(*SubagentEnrichment).Concurrent != 2 {
		t.Errorf("second start concurrent = %d, want 2", payload.(*SubagentEnrichment).Concurrent)
	}

	// Stop: real payloads carry an EMPTY agent_type — the state join recovers it.
	payload, err = e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "SubagentStop", SessionID: "sub-sess",
		RawEvent: map[string]interface{}{"agent_id": "a1", "agent_type": "", "agent_transcript_path": transcript},
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := payload.(*SubagentEnrichment)
	if stop.Phase != "stop" || stop.AgentType != "Explore" {
		t.Errorf("stop join failed: %+v", stop)
	}
	if stop.DurationMs < 0 {
		t.Errorf("duration = %d", stop.DurationMs)
	}
	if stop.Requests != 2 || stop.Tokens == nil || stop.Tokens.Input != 110 {
		t.Errorf("transcript sum wrong: requests=%d tokens=%+v", stop.Requests, stop.Tokens)
	}
	if stop.USD == nil || stop.USD.Total <= 0 {
		t.Errorf("usd = %+v, want > 0 for priced models", stop.USD)
	}
	if stop.Model != "claude-opus-4-8" {
		t.Errorf("model = %q, want the costliest", stop.Model)
	}

	// State entry consumed: a repeated Stop degrades gracefully (no join).
	payload, _ = e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "SubagentStop", SessionID: "sub-sess",
		RawEvent: map[string]interface{}{"agent_id": "a1", "agent_type": ""},
	})
	if payload.(*SubagentEnrichment).AgentType != "" {
		t.Error("state entry should have been consumed by the first Stop")
	}
}

func TestToolsEnricher_Shapes(t *testing.T) {
	e := toolsEnricher{}

	// Single PostToolUse.
	payload, err := e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "PostToolUse",
		RawEvent: map[string]interface{}{
			"tool_name": "Bash", "duration_ms": float64(1500),
			"tool_input":    map[string]interface{}{"command": "SECRET must not leak"},
			"tool_response": map[string]interface{}{"stdout": "ok"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	single := payload.(*ToolsEnrichment)
	if single.Total != 1 || single.Failed != 0 || single.Calls[0].Name != "Bash" || single.Calls[0].DurationMs != 1500 {
		t.Errorf("single = %+v", single)
	}

	// Privacy: serialized slug must not contain tool inputs.
	data, _ := json.Marshal(single)
	if strings.Contains(string(data), "SECRET") {
		t.Fatalf("tool_input leaked into tools slug: %s", data)
	}

	// Failure event.
	payload, _ = e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "PostToolUseFailure",
		RawEvent: map[string]interface{}{"tool_name": "Bash", "tool_response": nil},
	})
	if fail := payload.(*ToolsEnrichment); fail.Failed != 1 || fail.Calls[0].OK {
		t.Errorf("failure = %+v", fail)
	}

	// Batch with MCP, Skill, and a subagent call (+ one is_error response).
	payload, _ = e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "PostToolBatch",
		RawEvent: map[string]interface{}{
			"tool_calls": []interface{}{
				map[string]interface{}{"tool_name": "mcp__stripe__create_customer", "tool_response": map[string]interface{}{}},
				map[string]interface{}{"tool_name": "Skill", "tool_input": map[string]interface{}{"skill": "schedule"}},
				map[string]interface{}{"tool_name": "Agent", "tool_response": map[string]interface{}{"agentType": "Explore", "totalDurationMs": float64(41000)}},
				map[string]interface{}{"tool_name": "Read", "tool_response": map[string]interface{}{"is_error": true}},
			},
		},
	})
	batch := payload.(*ToolsEnrichment)
	if batch.Total != 4 || batch.Failed != 1 {
		t.Fatalf("batch = %+v", batch)
	}
	if batch.Calls[0].MCPServer != "stripe" {
		t.Errorf("mcp_server = %q", batch.Calls[0].MCPServer)
	}
	if batch.Calls[1].Skill != "schedule" {
		t.Errorf("skill = %q", batch.Calls[1].Skill)
	}
	if batch.Calls[2].AgentType != "Explore" || batch.Calls[2].DurationMs != 41000 {
		t.Errorf("agent call = %+v", batch.Calls[2])
	}
	if batch.Calls[3].OK {
		t.Error("is_error response must mark the call failed")
	}
}

func TestOSVersionParsers(t *testing.T) {
	plist := []byte(`<dict><key>ProductName</key><string>macOS</string>
<key>ProductVersion</key>
<string>26.1</string></dict>`)
	if got := osVersionFromPlist(plist); got != "26.1" {
		t.Errorf("plist version = %q", got)
	}
	if got := osVersionFromPlist([]byte("<dict></dict>")); got != "" {
		t.Errorf("missing key should yield empty, got %q", got)
	}

	osRelease := []byte("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nVERSION_ID=\"24.04\"\n")
	if got := osVersionFromOSRelease(osRelease); got != "Ubuntu 24.04.1 LTS" {
		t.Errorf("os-release = %q", got)
	}
	if got := osVersionFromOSRelease([]byte("VERSION_ID=\"12\"\n")); got != "12" {
		t.Errorf("version_id fallback = %q", got)
	}
}

func TestTurnAndInterrupt_Lifecycle(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })

	sess := "turn-sess"
	pe := promptEnricher{}
	te := turnEnricher{}
	promptCtx := func() *Context {
		return &Context{
			Tool: "claude-code", HookEvent: "UserPromptSubmit", SessionID: sess,
			RawEvent: map[string]interface{}{"prompt": "do the thing"},
		}
	}
	stopCtx := func() *Context {
		return &Context{
			Tool: "claude-code", HookEvent: "Stop", SessionID: sess, PromptID: "p-9",
			RawEvent: map[string]interface{}{},
		}
	}

	// Prompt 1 opens a turn: not an interrupt.
	payload, err := pe.Enrich(promptCtx())
	if err != nil {
		t.Fatal(err)
	}
	if payload.(PromptEnrichment).IsInterrupt {
		t.Error("first prompt must not be an interrupt")
	}

	// Prompt 2 before any Stop: interrupt.
	payload, _ = pe.Enrich(promptCtx())
	if !payload.(PromptEnrichment).IsInterrupt {
		t.Error("prompt during an open turn must be an interrupt")
	}

	// Stop closes the turn and reports its duration + prompt id.
	if !te.Applies(stopCtx()) {
		t.Fatal("turn enricher must apply to Stop")
	}
	payload, err = te.Enrich(stopCtx())
	if err != nil {
		t.Fatal(err)
	}
	turn := payload.(TurnEnrichment)
	if turn.DurationMs < 0 || turn.PromptID != "p-9" {
		t.Errorf("turn = %+v", turn)
	}

	// Turn consumed: a second Stop emits nothing…
	payload, _ = te.Enrich(stopCtx())
	if payload != nil {
		t.Errorf("closed turn should not re-emit, got %+v", payload)
	}
	// …and the next prompt is clean again.
	payload, _ = pe.Enrich(promptCtx())
	if payload.(PromptEnrichment).IsInterrupt {
		t.Error("prompt after Stop must not be an interrupt")
	}
}

func TestPermissionEnricher(t *testing.T) {
	e := permissionEnricher{}

	payload, err := e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "PermissionRequest",
		RawEvent: map[string]interface{}{
			"tool_name":  "Bash",
			"tool_input": map[string]interface{}{"command": "SECRET"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := payload.(PermissionEnrichment)
	if req.Decision != "requested" || req.ToolName != "Bash" {
		t.Errorf("request = %+v", req)
	}
	data, _ := json.Marshal(req)
	if strings.Contains(string(data), "SECRET") {
		t.Fatalf("tool_input leaked into permission slug: %s", data)
	}

	payload, _ = e.Enrich(&Context{
		Tool: "claude-code", HookEvent: "PermissionDenied",
		RawEvent: map[string]interface{}{
			"tool_name": "mcp__stripe__create_customer",
			"reason":    "policy says no",
		},
	})
	den := payload.(PermissionEnrichment)
	if den.Decision != "denied" || den.MCPServer != "stripe" {
		t.Errorf("denied = %+v", den)
	}
	data, _ = json.Marshal(den)
	if strings.Contains(string(data), "policy says no") {
		t.Fatalf("denial reason leaked into permission slug: %s", data)
	}
}
