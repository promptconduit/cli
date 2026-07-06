package sessions

import (
	"path/filepath"
	"reflect"
	"testing"
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
