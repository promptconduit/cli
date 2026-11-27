package cost

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// multiLineTurn is one Claude Code request written the way Claude Code writes
// it: one transcript line per content block (text, then three tool_use
// blocks), every line sharing the requestId and repeating the usage. A second
// one-line request follows.
var multiLineTurn = []string{
	`{"type":"assistant","requestId":"req_A","uuid":"u1","sessionId":"s","timestamp":"2026-11-27T00:00:00Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"on it"}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":1000}}}`,
	`{"type":"assistant","requestId":"req_A","uuid":"u2","sessionId":"s","timestamp":"2026-11-27T00:00:01Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":1000}}}`,
	`{"type":"assistant","requestId":"req_A","uuid":"u3","sessionId":"s","timestamp":"2026-11-27T00:00:01Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"/x"}}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":1000}}}`,
	`{"type":"assistant","requestId":"req_A","uuid":"u4","sessionId":"s","timestamp":"2026-11-27T00:00:01Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"toolu_3","name":"Read","input":{"file_path":"/y"}}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":1000}}}`,
	`{"type":"user","uuid":"u5","sessionId":"s","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1"}]}}`,
	`{"type":"assistant","requestId":"req_B","uuid":"u6","sessionId":"s","timestamp":"2026-11-27T00:00:05Z","cwd":"/p","message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"toolu_4","name":"Bash","input":{"command":"pwd"}}],"usage":{"input_tokens":5,"output_tokens":7}}}`,
}

// multiLineTurnTranscript returns the fixture as transcript text, with the
// req_A Edit line repeated (a line read twice must not double-count).
func multiLineTurnTranscript() string {
	return strings.Join(multiLineTurn, "\n") + "\n" + multiLineTurn[2] + "\n"
}

// TestSessionCountsEveryToolUseAcrossLines: every tool_use of a request
// counts, though only the request's first line is kept for tokens and cost.
func TestSessionCountsEveryToolUseAcrossLines(t *testing.T) {
	tbl := mustTable(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(multiLineTurnTranscript()), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(tbl, nil, nil, false)
	w.seedFile(path)
	s, ok := w.LatestSummary()
	if !ok {
		t.Fatal("no session summary")
	}
	if s.Tools.Total != 4 {
		t.Fatalf("tools total = %d, want 4 (by name %v)", s.Tools.Total, s.Tools.ByName)
	}
	if s.Tools.ByName["Bash"] != 2 || s.Tools.ByName["Edit"] != 1 || s.Tools.ByName["Read"] != 1 {
		t.Fatalf("tools by name = %v, want Bash 2, Edit 1, Read 1", s.Tools.ByName)
	}
	// Tokens stay deduped per request: req_A once + req_B once.
	if s.Totals.Input != 15 || s.Totals.Output != 27 || s.Totals.CacheRead != 1000 {
		t.Fatalf("totals = %+v, want input 15, output 27, cache_read 1000", s.Totals)
	}
	if s.Signals.ToolCalls != 4 {
		t.Fatalf("signals.tool_calls = %d, want 4", s.Signals.ToolCalls)
	}
}

// TestStreamCountsLaterLinesTools covers the live `cost watch` path, where a
// request's lines arrive one at a time.
func TestStreamCountsLaterLinesTools(t *testing.T) {
	tbl := mustTable(t)
	w := NewWatcher(tbl, nil, io.Discard, false)
	for _, l := range multiLineTurn[:4] {
		w.streamLine([]byte(l), "s")
	}
	w.streamLine([]byte(multiLineTurn[3]), "s") // re-read
	s, _ := w.LatestSummary()
	if s.Tools.Total != 3 || s.Totals.Output != 20 {
		t.Fatalf("tools = %+v, output = %d; want 3 tools, output 20", s.Tools, s.Totals.Output)
	}
}
