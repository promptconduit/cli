package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	syntheticUserLine = `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>2026-01-02T03:04:05.000Z</timestamp>\n<user_query>\nPLACEHOLDER\n</user_query>"}]}}`
	syntheticAsstLine = `{"role":"assistant","message":{"content":[{"type":"text","text":"PLACEHOLDER"},{"type":"tool_use","name":"PLACEHOLDER","input":{}}]}}`
	syntheticTurnLine = `{"type":"turn_ended","status":"success"}`
	parentSessionID   = "11111111-1111-1111-1111-111111111111"
	subagentSessionID = "22222222-2222-2222-2222-222222222222"
)

func setupCursorHome(t *testing.T) (home string, parser *CursorParser) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	parser, err := NewCursorParser()
	if err != nil {
		t.Fatalf("NewCursorParser: %v", err)
	}
	return home, parser
}

func writeJSONL(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func cursorTranscriptPath(home, slug, sessionID string) string {
	return filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", sessionID, sessionID+".jsonl")
}

func cursorSubagentPath(home, slug, parentID, subagentID string) string {
	return filepath.Join(home, ".cursor", "projects", slug, "agent-transcripts", parentID, "subagents", subagentID+".jsonl")
}

func TestCursorParser_GetToolName(t *testing.T) {
	_, parser := setupCursorHome(t)
	if got := parser.GetToolName(); got != "cursor" {
		t.Fatalf("GetToolName() = %q, want cursor", got)
	}
}

func TestCursorParser_GetTranscriptPaths(t *testing.T) {
	home, parser := setupCursorHome(t)
	slug := "Users-test-repo"
	parent := cursorTranscriptPath(home, slug, parentSessionID)
	subagent := cursorSubagentPath(home, slug, parentSessionID, subagentSessionID)
	writeJSONL(t, parent, syntheticUserLine)
	writeJSONL(t, subagent, syntheticAsstLine)

	// JSONL outside agent-transcripts must be ignored.
	other := filepath.Join(home, ".cursor", "projects", slug, "other.jsonl")
	writeJSONL(t, other, syntheticTurnLine)

	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(parent, older, older); err != nil {
		t.Fatalf("chtimes parent: %v", err)
	}
	if err := os.Chtimes(subagent, newer, newer); err != nil {
		t.Fatalf("chtimes subagent: %v", err)
	}

	paths, err := parser.GetTranscriptPaths()
	if err != nil {
		t.Fatalf("GetTranscriptPaths: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("GetTranscriptPaths returned %d files, want 2 (got %v)", len(paths), paths)
	}
	if paths[0] != subagent {
		t.Fatalf("newest file first: got %q, want %q", paths[0], subagent)
	}
	if paths[1] != parent {
		t.Fatalf("older file second: got %q, want %q", paths[1], parent)
	}
}

func TestCursorParser_GetTranscriptPaths_MissingDir(t *testing.T) {
	_, parser := setupCursorHome(t)
	paths, err := parser.GetTranscriptPaths()
	if err != nil {
		t.Fatalf("GetTranscriptPaths on empty home: %v", err)
	}
	if paths != nil {
		t.Fatalf("GetTranscriptPaths = %v, want nil", paths)
	}
}

func TestCursorParser_ParseFile_SessionIDAndTool(t *testing.T) {
	home, parser := setupCursorHome(t)
	path := cursorTranscriptPath(home, "slug", parentSessionID)
	writeJSONL(t, path, syntheticUserLine, syntheticAsstLine)

	conv, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if conv.SessionID != parentSessionID {
		t.Fatalf("SessionID = %q, want %q", conv.SessionID, parentSessionID)
	}
	if conv.Tool != "cursor" {
		t.Fatalf("Tool = %q, want cursor", conv.Tool)
	}
	if conv.SourceFileHash == "" {
		t.Fatal("SourceFileHash is empty")
	}
	wantHash, err := CalculateFileHash(path)
	if err != nil {
		t.Fatalf("CalculateFileHash: %v", err)
	}
	if conv.SourceFileHash != wantHash {
		t.Fatalf("SourceFileHash = %q, want %q", conv.SourceFileHash, wantHash)
	}
}

func TestCursorParser_ParseFile_RawJSONPassthrough(t *testing.T) {
	home, parser := setupCursorHome(t)
	path := cursorTranscriptPath(home, "slug", parentSessionID)
	writeJSONL(t, path, syntheticUserLine, syntheticAsstLine, syntheticTurnLine)

	conv, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(conv.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(conv.Messages))
	}
	want := []string{syntheticUserLine, syntheticAsstLine, syntheticTurnLine}
	for i, msg := range conv.Messages {
		if msg.RawJSON != want[i] {
			t.Fatalf("Messages[%d].RawJSON = %q, want %q", i, msg.RawJSON, want[i])
		}
		if msg.SequenceNumber != i {
			t.Fatalf("Messages[%d].SequenceNumber = %d, want %d", i, msg.SequenceNumber, i)
		}
	}
	if conv.Messages[0].Role != "user" {
		t.Fatalf("user role = %q, want user", conv.Messages[0].Role)
	}
	if conv.Messages[2].Type != "turn_ended" {
		t.Fatalf("turn type = %q, want turn_ended", conv.Messages[2].Type)
	}
}

func TestCursorParser_ParseFile_SkipEmpty(t *testing.T) {
	home, parser := setupCursorHome(t)
	path := cursorTranscriptPath(home, "slug", parentSessionID)
	body := syntheticUserLine + "\n\n  \n" + syntheticTurnLine + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	conv, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(conv.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2 (empty lines skipped)", len(conv.Messages))
	}
	if conv.Messages[0].RawJSON != syntheticUserLine {
		t.Fatalf("first RawJSON = %q, want user line", conv.Messages[0].RawJSON)
	}
	if conv.Messages[1].RawJSON != syntheticTurnLine {
		t.Fatalf("second RawJSON = %q, want turn_ended line", conv.Messages[1].RawJSON)
	}
}

func TestCursorParser_ParseFile_TimestampFromUserText(t *testing.T) {
	home, parser := setupCursorHome(t)
	path := cursorTranscriptPath(home, "slug", parentSessionID)
	writeJSONL(t, path, syntheticUserLine, syntheticAsstLine)

	conv, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	const want = "2026-01-02T03:04:05.000Z"
	if conv.Messages[0].Timestamp != want {
		t.Fatalf("user timestamp = %q, want %q", conv.Messages[0].Timestamp, want)
	}
	if conv.StartedAt != want {
		t.Fatalf("StartedAt = %q, want %q", conv.StartedAt, want)
	}
}

func TestCursorParser_ParseFile_TimestampFallbackMtime(t *testing.T) {
	home, parser := setupCursorHome(t)
	path := cursorTranscriptPath(home, "slug", parentSessionID)
	writeJSONL(t, path, syntheticAsstLine, syntheticTurnLine)

	mtime := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	conv, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	want := mtime.UTC().Format(time.RFC3339)
	if conv.Messages[0].Timestamp != want {
		t.Fatalf("fallback timestamp = %q, want %q", conv.Messages[0].Timestamp, want)
	}
	if conv.StartedAt != want || conv.EndedAt != want {
		t.Fatalf("StartedAt/EndedAt = %q/%q, want %q", conv.StartedAt, conv.EndedAt, want)
	}
}

func TestFindTranscriptBySessionID_Cursor(t *testing.T) {
	home, _ := setupCursorHome(t)
	path := cursorTranscriptPath(home, "slug", parentSessionID)
	writeJSONL(t, path, syntheticUserLine)

	got, err := FindTranscriptBySessionID(parentSessionID)
	if err != nil {
		t.Fatalf("FindTranscriptBySessionID: %v", err)
	}
	if got != path {
		t.Fatalf("FindTranscriptBySessionID = %q, want %q", got, path)
	}
}

func TestFindTranscriptBySessionID_PrefersClaude(t *testing.T) {
	home, _ := setupCursorHome(t)
	cursorPath := cursorTranscriptPath(home, "slug", parentSessionID)
	writeJSONL(t, cursorPath, syntheticUserLine)

	claudePath := filepath.Join(home, ".claude", "projects", "proj", parentSessionID+".jsonl")
	writeJSONL(t, claudePath, `{"type":"user","message":{"content":"PLACEHOLDER"}}`)

	got, err := FindTranscriptBySessionID(parentSessionID)
	if err != nil {
		t.Fatalf("FindTranscriptBySessionID: %v", err)
	}
	if got != claudePath {
		t.Fatalf("FindTranscriptBySessionID = %q, want Claude path %q", got, claudePath)
	}
}

func TestNewParserForPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cursorPath := filepath.Join("Users", "me", ".cursor", "projects", "slug", "agent-transcripts", "s", "s.jsonl")
	p, err := NewParserForPath(cursorPath)
	if err != nil {
		t.Fatalf("NewParserForPath cursor: %v", err)
	}
	if _, ok := p.(*CursorParser); !ok {
		t.Fatalf("NewParserForPath(%q) = %T, want *CursorParser", cursorPath, p)
	}

	claudePath := filepath.Join("Users", "me", ".claude", "projects", "proj", "s.jsonl")
	p, err = NewParserForPath(claudePath)
	if err != nil {
		t.Fatalf("NewParserForPath claude: %v", err)
	}
	if _, ok := p.(*ClaudeCodeParser); !ok {
		t.Fatalf("NewParserForPath(%q) = %T, want *ClaudeCodeParser", claudePath, p)
	}
}
