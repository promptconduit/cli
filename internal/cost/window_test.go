package cost

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// windowLines renders a request as two transcript lines (text, then tool_use)
// sharing requestId and usage, the second stamped one second later.
func windowLines(session, rid string, ts time.Time) string {
	usage := `"usage":{"input_tokens":10,"output_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"sessionId":%q,"timestamp":%q,"message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"x"}],%s}}`+"\n"+
		`{"type":"assistant","requestId":%q,"sessionId":%q,"timestamp":%q,"message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"tu-%s","name":"Read"}],%s}}`+"\n",
		rid, session, ts.Format(time.RFC3339Nano), usage,
		rid, session, ts.Add(time.Second).Format(time.RFC3339Nano), rid, usage)
}

func TestWatcherWindow(t *testing.T) {
	table, err := LoadPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	const session = "win-session"
	path := filepath.Join(t.TempDir(), session+".jsonl")
	base := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i := 0; i < 5; i++ { // requests at 15:00, 15:10, 15:20, 15:30, 15:40
		b.WriteString(windowLines(session, fmt.Sprintf("r%d", i), base.Add(time.Duration(i)*10*time.Minute)))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	price := func(from, to time.Time) SessionSummary {
		w := NewWatcher(table, nil, io.Discard, false)
		w.SetWindow(from, to)
		w.SeedSession(path)
		s, _ := w.SummaryFor(session)
		return s
	}
	all := price(time.Time{}, time.Time{})
	if all.Requests != 5 || all.Tools.Total != 5 {
		t.Fatalf("unbounded: %d requests / %d tools, want 5 / 5", all.Requests, all.Tools.Total)
	}
	per := all.Totals.CostTotal / 5

	// Inclusive bounds on the request's first line. r0's second line
	// (15:00:01) falls inside [15:00:01, ...] but r0 started before it, so
	// it must not count, not even as a fresh request.
	in := price(base.Add(time.Second), base.Add(30*time.Minute))
	if in.Requests != 3 || in.Tools.Total != 3 {
		t.Fatalf("window: %d requests / %d tools, want 3 / 3 (r1..r3)", in.Requests, in.Tools.Total)
	}
	if math.Abs(in.Totals.CostTotal-3*per) > 1e-9 {
		t.Errorf("window cost = %f, want %f", in.Totals.CostTotal, 3*per)
	}
	if from := price(base.Add(40*time.Minute), time.Time{}); from.Requests != 1 {
		t.Errorf("from-only: %d requests, want 1", from.Requests)
	}
	if _, ok := func() (SessionSummary, bool) {
		w := NewWatcher(table, nil, io.Discard, false)
		w.SetWindow(base.Add(2*time.Hour), time.Time{})
		w.SeedSession(path)
		return w.SummaryFor(session)
	}(); ok {
		t.Error("an empty window should report no session")
	}
}

func TestFindTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "-some-other-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "abc-123.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := FindTranscript("abc-123")
	if err != nil || !ok || got != want {
		t.Fatalf("FindTranscript = %q %v %v, want %q", got, ok, err, want)
	}
	for _, bad := range []string{"missing", "", "../abc-123", "x/abc-123"} {
		if _, ok, _ := FindTranscript(bad); ok {
			t.Errorf("FindTranscript(%q) should not match", bad)
		}
	}
}
