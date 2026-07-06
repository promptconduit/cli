package sessions

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseProcessTree(t *testing.T) {
	out := `
  501 1 /sbin/launchd
89754 35241 zsh
53134 89754 claude
11351 89754 claude
35241 501 Cursor Helper
`
	parents, comms := parseProcessTree(out)
	if parents["53134"] != "89754" {
		t.Fatalf("parent 53134: got %q", parents["53134"])
	}
	if comms["53134"] != "claude" {
		t.Fatalf("comm 53134: got %q", comms["53134"])
	}
}

func TestClaudeDescendants(t *testing.T) {
	out := `
89754 35241 zsh
53134 89754 claude
11351 89754 claude
92000 53134 claude
15823 35241 zsh
77777 15823 claude
`
	parents, comms := parseProcessTree(out)
	got := claudeDescendants("89754", parents, comms)
	want := []string{"53134", "11351", "92000"}
	if !reflect.DeepEqual(sortedStrings(got), sortedStrings(want)) {
		t.Fatalf("claudeDescendants\n got: %v\nwant: %v", got, want)
	}
	got2 := claudeDescendants("15823", parents, comms)
	if !reflect.DeepEqual(got2, []string{"77777"}) {
		t.Fatalf("other shell: got %v", got2)
	}
}

func sortedStrings(ss []string) []string {
	cp := append([]string(nil), ss...)
	for i := 0; i < len(cp); i++ {
		for j := i + 1; j < len(cp); j++ {
			if cp[j] < cp[i] {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	return cp
}

func TestParseResumeSessionID(t *testing.T) {
	cases := []struct {
		args string
		want string
	}{
		{"claude --resume b7c88043-1234-5678-9abc-def012345678", "b7c88043-1234-5678-9abc-def012345678"},
		{"claude", ""},
		{"/usr/local/bin/claude --resume abc --add-dir /tmp/foo", "abc"},
	}
	for _, tc := range cases {
		if got := parseResumeSessionID(tc.args); got != tc.want {
			t.Fatalf("parseResumeSessionID(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestParseTranscriptSessionID(t *testing.T) {
	root := claudeProjectsDir()
	if root == "" {
		t.Skip("no home dir")
	}
	path := filepath.Join(root, "encoded-dir", "session-uuid-1234.jsonl")
	out := "p53134\nn" + path + "\nn/dev/null\n"
	if got := parseTranscriptSessionID(out); got != "session-uuid-1234" {
		t.Fatalf("got %q", got)
	}
}

func TestIsDescendantOf(t *testing.T) {
	parents := map[string]string{
		"53134": "89754",
		"89754": "35241",
		"35241": "1",
	}
	if !isDescendantOf("53134", "89754", parents) {
		t.Fatal("53134 should be under 89754")
	}
	if isDescendantOf("89754", "89754", parents) {
		t.Fatal("process is not its own descendant")
	}
	if isDescendantOf("53134", "15823", parents) {
		t.Fatal("53134 is not under 15823")
	}
}

func TestEncodeProjectPath(t *testing.T) {
	got := encodeProjectPath("/Users/x/GitHub/foo")
	want := "-Users-x-GitHub-foo"
	if got != want {
		t.Fatalf("encodeProjectPath = %q, want %q", got, want)
	}
}

func TestListRecentTranscripts(t *testing.T) {
	dir := t.TempDir()

	mustTouch := func(name string, mod time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	mustTouch("older.jsonl", now.Add(-30*time.Minute))
	mustTouch("newer.jsonl", now.Add(-1*time.Minute))

	got := listRecentTranscripts(dir, now.Add(-time.Hour))
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if sessionIDFromTranscriptPath(got[0]) != "newer" {
		t.Fatalf("newest first: got %q", got[0])
	}
}

func TestResolveFallbackCandidates_transcript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, ".claude", "projects")
	cwd := "/tmp/my-project"
	projDir := filepath.Join(projects, encodeProjectPath(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projDir, "sess-abc.jsonl")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cands := resolveFallbackCandidates("53134", cwd, now)
	if len(cands) != 1 || cands[0].SessionID != "sess-abc" {
		t.Fatalf("got %#v", cands)
	}
}

func TestRecentSessionStartsFromEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	lines := []string{
		`{"schema":2,"tool":"claude-code","hook_event":"SessionStart","session_id":"old","captured_at":"2026-07-01T10:00:00Z","raw_event":{"cwd":"/a"}}`,
		`{"schema":2,"tool":"claude-code","hook_event":"SessionStart","session_id":"new","captured_at":"2026-07-01T12:00:00Z","raw_event":{"cwd":"/a"}}`,
		`{"schema":2,"tool":"cursor","hook_event":"sessionStart","session_id":"c1","captured_at":"2026-07-01T12:00:00Z","raw_event":{"cwd":"/a"}}`,
	}
	if err := os.WriteFile(path, []byte(stringsJoin(lines)), 0o644); err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-07-01T13:00:00Z")
	got, err := recentSessionStartsFromEvents(path, "/a", now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new", "old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestDedupeCandidates(t *testing.T) {
	in := []ResolveCandidate{
		{SessionID: "a", PID: "1"},
		{SessionID: "a", PID: "2"},
		{SessionID: "b", PID: "3"},
	}
	got := dedupeCandidates(in)
	if len(got) != 2 || got[0].SessionID != "a" || got[1].SessionID != "b" {
		t.Fatalf("got %#v", got)
	}
}

func stringsJoin(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out + "\n"
}
