package enrich

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
