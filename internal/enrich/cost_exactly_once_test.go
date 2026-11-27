package enrich

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

// ccRequestLines renders one Claude Code request the way the transcript holds
// it: a text line, then a tool_use line repeating the same requestId and usage.
func ccRequestLines(session, rid string, ts time.Time, text string) string {
	usage := `"usage":{"input_tokens":3,"output_tokens":40,"cache_read_input_tokens":2000,"cache_creation_input_tokens":100}`
	l1 := fmt.Sprintf(`{"type":"assistant","requestId":%q,"uuid":"%s-a","sessionId":%q,"timestamp":%q,"message":{"model":"claude-opus-4-8","content":[{"type":"text","text":%q}],%s}}`,
		rid, rid, session, ts.Format(time.RFC3339Nano), text, usage)
	l2 := fmt.Sprintf(`{"type":"assistant","requestId":%q,"uuid":"%s-b","sessionId":%q,"timestamp":%q,"message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"tu-%s","name":"Bash"}],%s}}`,
		rid, rid, session, ts.Add(time.Second).Format(time.RFC3339Nano), rid, usage)
	user := fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":%q,"message":{"content":"ok"}}`, session, ts.Add(2*time.Second).Format(time.RFC3339Nano))
	return l1 + "\n" + l2 + "\n" + user + "\n"
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// TestCostEnricher_EveryRequestPricedOnce drives a multi-turn session through
// its Stop and SessionEnd hooks, reproducing each way requests used to go
// missing, and checks every transcript request lands in exactly one event and
// the events sum to the `cost session` total.
func TestCostEnricher_EveryRequestPricedOnce(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })
	prevWait := finalMessageWait
	finalMessageWait = 3 * time.Second
	t.Cleanup(func() { finalMessageWait = prevWait })

	const session = "11111111-2222-3333-4444-555555555555"
	transcript := filepath.Join(t.TempDir(), session+".jsonl")
	base := time.Date(2026, 11, 26, 15, 16, 0, 0, time.UTC)
	n := 0
	next := func(text string) (string, string) {
		n++
		rid := fmt.Sprintf("req-%03d", n)
		return rid, ccRequestLines(session, rid, base.Add(time.Duration(n)*10*time.Second), text)
	}
	hook := func(event, lastMsg string) *CostEnrichment {
		t.Helper()
		ctx := &Context{
			Tool: "claude-code", HookEvent: event, SessionID: session, TranscriptPath: transcript,
			RawEvent: map[string]interface{}{"last_assistant_message": lastMsg},
		}
		if !(costEnricher{}).Applies(ctx) {
			t.Fatalf("cost must apply to %s", event)
		}
		p, err := costEnricher{}.Enrich(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p == nil {
			return nil
		}
		ce := p.(*CostEnrichment)
		if ce.Error != "" {
			t.Fatalf("%s: unexpected error %q", event, ce.Error)
		}
		return ce
	}
	var events []*CostEnrichment

	// Turn 1: a long autonomous turn of 130 requests (the old cap kept only
	// the newest 100). Its final line is flushed 300ms after Stop fires.
	var lines string
	for i := 0; i < 129; i++ {
		_, l := next("working")
		lines += l
	}
	appendTo(t, transcript, lines)
	_, final1 := next("Turn one is done.")
	go func() {
		time.Sleep(300 * time.Millisecond)
		if f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(final1)
			_ = f.Close()
		}
	}()
	ce := hook("Stop", "Turn one is done.")
	if len(ce.Requests) != 130 {
		t.Fatalf("turn 1 Stop priced %d requests, want all 130", len(ce.Requests))
	}
	events = append(events, ce)

	// Turn 2: Stop fires while the final line is half-written and the payload
	// has no last_assistant_message to wait for. The partial line must not
	// be consumed.
	rid2a, l := next("step")
	appendTo(t, transcript, l)
	rid2b, final2 := next("Turn two is done.")
	appendTo(t, transcript, final2[:40])
	ce = hook("Stop", "")
	if len(ce.Requests) != 1 || ce.Requests[0].RequestID != rid2a {
		t.Fatalf("turn 2 Stop = %+v, want only %s", ce.Requests, rid2a)
	}
	events = append(events, ce)

	// A Stop with nothing new still carries the slug, empty.
	ce = hook("Stop", "")
	if ce == nil || len(ce.Requests) != 0 {
		t.Fatalf("empty Stop = %+v, want an empty slug", ce)
	}

	// The line finishes after the session's last Stop; SessionEnd prices it.
	appendTo(t, transcript, final2[40:])
	ce = hook("SessionEnd", "")
	if ce == nil || len(ce.Requests) != 1 || ce.Requests[0].RequestID != rid2b {
		t.Fatalf("SessionEnd = %+v, want only %s", ce, rid2b)
	}
	events = append(events, ce)
	if again := hook("SessionEnd", ""); again != nil {
		t.Fatalf("SessionEnd with nothing new should omit the slug, got %+v", again)
	}

	// Every request exactly once; the sum matches the whole-session price.
	seen := map[string]int{}
	var sum float64
	for _, e := range events {
		sum += e.Totals.USD
		for _, r := range e.Requests {
			seen[r.RequestID]++
			if r.Tools == nil || r.Tools.Total != 1 {
				t.Errorf("%s tools = %+v, want 1 Bash call", r.RequestID, r.Tools)
			}
		}
	}
	for i := 1; i <= n; i++ {
		rid := fmt.Sprintf("req-%03d", i)
		if seen[rid] != 1 {
			t.Errorf("%s priced %d times, want exactly once", rid, seen[rid])
		}
	}
	table, err := cost.LoadPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	w := cost.NewWatcher(table, nil, io.Discard, false)
	w.SeedSession(transcript)
	summary, ok := w.SummaryFor(session)
	if !ok || summary.Requests != n {
		t.Fatalf("session summary = %+v (ok=%v), want %d requests", summary, ok, n)
	}
	if math.Abs(summary.Totals.CostTotal-sum) > 1e-9 {
		t.Errorf("events sum to $%f, session total is $%f", sum, summary.Totals.CostTotal)
	}
}

func TestCostEnricher_MissingTranscriptIsExplicit(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })
	for _, path := range []string{filepath.Join(t.TempDir(), "never-saved.jsonl"), ""} {
		ctx := &Context{Tool: "claude-code", HookEvent: "Stop", SessionID: "s-missing", TranscriptPath: path, RawEvent: map[string]interface{}{}}
		if !(costEnricher{}).Applies(ctx) {
			t.Fatalf("cost must apply to every claude-code Stop (path %q)", path)
		}
		data := runAndMarshal(costEnricher{}, ctx)
		var ce CostEnrichment
		if err := json.Unmarshal(data, &ce); err != nil {
			t.Fatalf("path %q: slug = %s: %v", path, data, err)
		}
		if ce.Error == "" || ce.Requests == nil || len(ce.Requests) != 0 {
			t.Errorf("path %q: want an explicit error and empty requests, got %s", path, data)
		}
	}
}

func TestCostEnricher_StateBusyIsExplicit(t *testing.T) {
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })
	prev := stateLockWait
	stateLockWait = 10 * time.Millisecond
	t.Cleanup(func() { stateLockWait = prev })

	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	appendTo(t, transcript, ccRequestLines("s-busy", "req-1", time.Now(), "hi"))
	held := make(chan struct{})
	release := make(chan struct{})
	go updateState("s-busy", func(*sessionState) bool {
		close(held)
		<-release
		return false
	})
	<-held
	defer close(release)

	p, err := costEnricher{}.Enrich(&Context{Tool: "claude-code", HookEvent: "Stop", SessionID: "s-busy", TranscriptPath: transcript, RawEvent: map[string]interface{}{}})
	if err != nil {
		t.Fatal(err)
	}
	if ce := p.(*CostEnrichment); ce.Error == "" {
		t.Errorf("busy lock should yield an explicit error, got %+v", ce)
	}
}

// failingCost is the cost enricher with Enrich forced to fail.
type failingCost struct{ costEnricher }

func (failingCost) Enrich(*Context) (any, error) { return nil, errors.New("boom") }

func TestRun_FailureReporterKeepsSlug(t *testing.T) {
	stop := &Context{Tool: "claude-code", HookEvent: "Stop"}
	data := runAndMarshal(failingCost{}, stop)
	var ce CostEnrichment
	if err := json.Unmarshal(data, &ce); err != nil || ce.Error != "boom" {
		t.Fatalf("failed cost on Stop = %s, want error \"boom\"", data)
	}
	// Elsewhere (Cursor) a failure still drops the slug.
	if data := runAndMarshal(failingCost{}, &Context{Tool: "cursor", HookEvent: "stop"}); data != nil {
		t.Errorf("failed cursor cost should drop the slug, got %s", data)
	}
}
