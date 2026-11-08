package enrich

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassifyTaskKind(t *testing.T) {
	cases := []struct {
		prompt string
		want   string
	}{
		{"publish the blog post", TaskKindShip},
		{"merge it", TaskKindShip},
		{"cut v0.36.0 and add the changelog", TaskKindShip},
		{"open a PR for this", TaskKindShip},
		{"why is CI failing", TaskKindDebug},
		{"fix the login redirect", TaskKindDebug},
		{"add coverage for the parser", TaskKindBuild}, // "add" starts first
		{"write tests for the parser", TaskKindTest},
		{"refactor the auth module", TaskKindRefactor},
		{"write the blog post about caching", TaskKindDocs},
		{"update the README", TaskKindDocs},
		{"review this diff", TaskKindReview},
		{"can you ensure all cicd ran succesfully", TaskKindReview},
		{"how does the offset state work", TaskKindExplore},
		{"implement the export endpoint", TaskKindBuild},
		{"bump golangci-lint", TaskKindChore},
		{"yes please", TaskKindOther},
		{"", TaskKindOther},
	}
	for _, c := range cases {
		if got := ClassifyTaskKind(c.prompt); got != c.want {
			t.Errorf("ClassifyTaskKind(%q) = %q, want %q", c.prompt, got, c.want)
		}
	}
}

func TestTaskKindFor_ShortFollowUpsInherit(t *testing.T) {
	cases := []struct {
		prompt, previous, want string
	}{
		{"yes please", TaskKindShip, TaskKindShip},
		{"do it", TaskKindDebug, TaskKindDebug},
		{"yes please", "", TaskKindOther},
		{"merge it", TaskKindDebug, TaskKindShip},                             // short but classified
		{"sounds good, carry on with that then", TaskKindShip, TaskKindOther}, // long: no inheritance
		{"", TaskKindDocs, TaskKindDocs},                                      // empty inherits
	}
	for _, c := range cases {
		if got := taskKindFor(c.prompt, c.previous); got != c.want {
			t.Errorf("taskKindFor(%q, %q) = %q, want %q", c.prompt, c.previous, got, c.want)
		}
	}
}

func TestPromptEnricher_TaskKindInheritsAcrossPrompts(t *testing.T) {
	withStateDir(t)
	e := promptEnricher{}
	run := func(prompt string) PromptEnrichment {
		t.Helper()
		p, err := e.Enrich(&Context{
			Tool: "claude-code", HookEvent: "UserPromptSubmit", SessionID: "tk-sess",
			RawEvent: map[string]interface{}{"prompt": prompt},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p.(PromptEnrichment)
	}
	if got := run("publish the blog post").TaskKind; got != TaskKindShip {
		t.Fatalf("first prompt kind = %q, want ship", got)
	}
	if got := run("yes please").TaskKind; got != TaskKindShip {
		t.Fatalf("follow-up kind = %q, want inherited ship", got)
	}
	if got := run("why is CI failing").TaskKind; got != TaskKindDebug {
		t.Fatalf("third prompt kind = %q, want debug", got)
	}
}

func assistantLine(reqID, ts, stopReason string, tools ...string) string {
	content := `[{"type":"text"}]`
	if len(tools) > 0 {
		content = "["
		for i, n := range tools {
			if i > 0 {
				content += ","
			}
			content += fmt.Sprintf(`{"type":"tool_use","name":%q}`, n)
		}
		content += "]"
	}
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"sessionId":"s1","timestamp":%q,"message":{"model":"claude-opus-5-5","stop_reason":%q,"content":%s,"usage":{"input_tokens":2,"output_tokens":100,"cache_read_input_tokens":1000,"cache_creation_input_tokens":10}}}`,
		reqID, ts, stopReason, content)
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCostEnricher_WaitsForFinalMessage reproduces the Stop flush race: when
// Stop fires, the transcript only holds the turn's tool_use call; the final
// end_turn message lands a moment later. The cost slug must include it.
func TestCostEnricher_WaitsForFinalMessage(t *testing.T) {
	withStateDir(t)
	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	now := time.Now().UTC()
	// The previous turn's last call, then this turn's tool call.
	appendLines(t, transcript,
		assistantLine("req-old", now.Add(-time.Minute).Format(time.RFC3339Nano), "end_turn"),
	)
	updateState("s1", func(st *sessionState) bool {
		st.TurnStartedAt = now.Add(-10 * time.Second).Format(time.RFC3339Nano)
		return true
	})
	appendLines(t, transcript,
		assistantLine("req-1", now.Add(-5*time.Second).Format(time.RFC3339Nano), "tool_use", "Bash"),
	)

	go func() {
		time.Sleep(300 * time.Millisecond)
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		_, _ = f.WriteString(assistantLine("req-2", time.Now().UTC().Format(time.RFC3339Nano), "end_turn") + "\n")
		_ = f.Close()
	}()

	start := time.Now()
	payload, err := costEnricher{}.Enrich(&Context{
		Tool: "claude-code", HookEvent: "Stop", SessionID: "s1", TranscriptPath: transcript,
		RawEvent: map[string]interface{}{},
	})
	if err != nil {
		t.Fatal(err)
	}
	waited := time.Since(start)
	ce := payload.(*CostEnrichment)
	var ids []string
	for _, r := range ce.Requests {
		ids = append(ids, r.RequestID)
	}
	if len(ids) != 3 || ids[2] != "req-2" {
		t.Fatalf("requests = %v, want req-old, req-1, req-2 (final message included)", ids)
	}
	if waited < 250*time.Millisecond || waited > 1400*time.Millisecond {
		t.Errorf("waited %s; want to return soon after the final line lands", waited)
	}
}

func TestCostEnricher_FlushWaitIsCapped(t *testing.T) {
	withStateDir(t)
	prevMax := flushWaitMax
	flushWaitMax = 200 * time.Millisecond
	t.Cleanup(func() { flushWaitMax = prevMax })

	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	now := time.Now().UTC()
	updateState("s1", func(st *sessionState) bool {
		st.TurnStartedAt = now.Add(-10 * time.Second).Format(time.RFC3339Nano)
		return true
	})
	appendLines(t, transcript, assistantLine("req-1", now.Add(-5*time.Second).Format(time.RFC3339Nano), "tool_use", "Read"))

	start := time.Now()
	payload, _ := costEnricher{}.Enrich(&Context{
		Tool: "claude-code", HookEvent: "Stop", SessionID: "s1", TranscriptPath: transcript,
		RawEvent: map[string]interface{}{},
	})
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("waited %s past the cap", waited)
	}
	if ce := payload.(*CostEnrichment); len(ce.Requests) != 1 {
		t.Fatalf("on timeout, price what exists: got %d requests", len(ce.Requests))
	}
}

func TestTurnFlushed(t *testing.T) {
	start := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	before, after := start.Add(-time.Second), start.Add(time.Second)
	cases := []struct {
		name string
		last lastAssistant
		turn time.Time
		want bool
	}{
		{"nothing new, no open turn", lastAssistant{}, time.Time{}, true},
		{"nothing new, open turn", lastAssistant{}, start, false},
		{"previous turn's line", lastAssistant{seen: true, ts: before, stopReason: "end_turn"}, start, false},
		{"tool_use pending", lastAssistant{seen: true, ts: after, stopReason: "tool_use"}, start, false},
		{"final message", lastAssistant{seen: true, ts: after, stopReason: "end_turn"}, start, true},
		{"null stop reason counts", lastAssistant{seen: true, ts: after}, start, true},
	}
	for _, c := range cases {
		if got := turnFlushed(c.last, c.turn); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCostEnricher_MergesToolsAcrossContentBlockLines: Claude Code writes one
// line per content block; the tool_use blocks after the first line belong to
// the same request.
func TestCostEnricher_MergesToolsAcrossContentBlockLines(t *testing.T) {
	withStateDir(t)
	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	appendLines(t, transcript,
		assistantLine("req-1", ts, "tool_use"), // thinking/text block
		assistantLine("req-1", ts, "tool_use", "Bash"),
		assistantLine("req-1", ts, "tool_use", "Read", "Bash"),
		assistantLine("req-2", ts, "end_turn"),
	)
	payload, err := costEnricher{}.Enrich(&Context{
		Tool: "claude-code", HookEvent: "Stop", SessionID: "s1", TranscriptPath: transcript,
		RawEvent: map[string]interface{}{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ce := payload.(*CostEnrichment)
	if len(ce.Requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(ce.Requests))
	}
	tools := ce.Requests[0].Tools
	if tools == nil || tools.Total != 3 || tools.ByName["Bash"] != 2 || tools.ByName["Read"] != 1 {
		t.Fatalf("req-1 tools = %+v, want Bash×2 Read×1", tools)
	}
	if ce.Requests[0].Signals.ToolCalls != 3 {
		t.Errorf("signals.tool_calls = %d, want 3", ce.Requests[0].Signals.ToolCalls)
	}
}

func usageLine(reqID string, output int64, tool string) string {
	content := `[{"type":"thinking"}]`
	if tool != "" {
		content = fmt.Sprintf(`[{"type":"tool_use","name":%q}]`, tool)
	}
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"sessionId":"s1","timestamp":"2026-11-08T10:00:00Z","message":{"model":"claude-opus-5-5","stop_reason":"end_turn","content":%s,"usage":{"input_tokens":2,"output_tokens":%d,"cache_read_input_tokens":1000,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_1h_input_tokens":10}}}}`,
		reqID, content, output)
}

// TestCostEnricher_PricesMergedUsage: a request's first transcript line holds
// a partial output count (242); a later line holds the full one (893). The
// request is priced at 893, and never below the largest count seen.
func TestCostEnricher_PricesMergedUsage(t *testing.T) {
	withStateDir(t)
	transcript := filepath.Join(t.TempDir(), "s1.jsonl")
	appendLines(t, transcript,
		usageLine("req-1", 242, ""),
		usageLine("req-1", 893, "Bash"),
		usageLine("req-2", 422, ""),
		usageLine("req-2", 301, "Read"), // later line with a smaller count: keep the max
	)
	payload, err := costEnricher{}.Enrich(&Context{
		Tool: "claude-code", HookEvent: "Stop", SessionID: "s1", TranscriptPath: transcript,
		RawEvent: map[string]interface{}{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ce := payload.(*CostEnrichment)
	if len(ce.Requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(ce.Requests))
	}
	const outRate = 20.0 / 1_000_000 // claude-opus-5-5 output
	for i, want := range []int64{893, 422} {
		r := ce.Requests[i]
		if r.Tokens.Output != want {
			t.Errorf("%s output tokens = %d, want %d", r.RequestID, r.Tokens.Output, want)
		}
		if got, exp := r.USD.Output, float64(want)*outRate; math.Abs(got-exp) > 1e-9 {
			t.Errorf("%s usd.output = %g, want %g", r.RequestID, got, exp)
		}
		if sum := r.USD.Input + r.USD.Output + r.USD.CacheRead + r.USD.CacheWrite; math.Abs(sum-r.USD.Total) > 1e-12 {
			t.Errorf("%s total %g != parts %g", r.RequestID, r.USD.Total, sum)
		}
		if r.Tools == nil || r.Tools.Total != 1 || r.Signals.ToolCalls != 1 {
			t.Errorf("%s tools = %+v signals.tool_calls = %d", r.RequestID, r.Tools, r.Signals.ToolCalls)
		}
	}
	if ce.Totals.Tokens.Output != 893+422 {
		t.Errorf("totals output = %d", ce.Totals.Tokens.Output)
	}
}
