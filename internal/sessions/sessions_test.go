package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// line builds one v2 events.jsonl envelope line for the fields Aggregate reads.
func line(tool, sid, cwd, branch, repo, at, prompt string) string {
	var b strings.Builder
	b.WriteString(`{"schema":2,"tool":"` + tool + `","captured_at":"` + at + `","session_id":"` + sid + `","raw_event":{"cwd":"` + cwd + `"`)
	if prompt != "" {
		b.WriteString(`,"prompt":"` + prompt + `"`)
	}
	b.WriteString(`},"enrichments":{"vcs":{"repo":"` + repo + `","branch":"` + branch + `"}}}`)
	return b.String()
}

func TestAggregate_GroupsAndKeepsLatest(t *testing.T) {
	lines := []string{
		line("claude-code", "s1", "/repo/a", "main", "a", "2026-07-01T10:00:00Z", "first prompt"),
		line("claude-code", "s1", "/repo/a-worktree", "feat/x", "a", "2026-07-01T10:05:00Z", ""),
		line("claude-code", "s2", "/repo/b", "main", "b", "2026-07-01T09:00:00Z", ""),
	}
	got := Aggregate(lines)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	// Newest-active first → s1.
	if got[0].SessionID != "s1" {
		t.Errorf("first session = %s, want s1 (newest active)", got[0].SessionID)
	}
	// Latest event's cwd/branch win (session moved into a worktree).
	if got[0].Cwd != "/repo/a-worktree" || got[0].Branch != "feat/x" {
		t.Errorf("s1 cwd/branch = %s/%s, want /repo/a-worktree/feat/x", got[0].Cwd, got[0].Branch)
	}
	if got[0].EventCount != 2 {
		t.Errorf("s1 event count = %d, want 2", got[0].EventCount)
	}
	// The prompt seen earlier is retained as the last-known prompt.
	if got[0].LastPrompt != "first prompt" {
		t.Errorf("s1 last prompt = %q, want %q", got[0].LastPrompt, "first prompt")
	}
}

func TestAggregate_SkipsUnusableAndNonResumable(t *testing.T) {
	lines := []string{
		`not json`,
		line("cursor", "c1", "/repo/a", "main", "a", "2026-07-01T10:00:00Z", ""),      // non-resumable tool
		line("claude-code", "", "/repo/a", "main", "a", "2026-07-01T10:00:00Z", ""),   // no session id
		line("claude-code", "s3", "", "main", "a", "2026-07-01T10:00:00Z", ""),        // no cwd
		line("claude-code", "s4", "/repo/c", "main", "c", "2026-07-01T10:00:00Z", ""), // keep
	}
	got := Aggregate(lines)
	if len(got) != 1 || got[0].SessionID != "s4" {
		t.Fatalf("got %+v, want only s4", got)
	}
}

func TestFilter_ByCutoff(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	all := []Session{
		{SessionID: "recent", LastActive: now.Add(-30 * time.Minute)},
		{SessionID: "old", LastActive: now.Add(-25 * time.Hour)},
	}
	got := Filter(all, now.Add(-12*time.Hour))
	if len(got) != 1 || got[0].SessionID != "recent" {
		t.Fatalf("got %+v, want only recent", got)
	}
}

func TestReadRecentFrom_TailAndFilter(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	content := strings.Join([]string{
		line("claude-code", "old", "/repo/a", "main", "a", "2026-06-20T10:00:00Z", ""),
		line("claude-code", "fresh", "/repo/b", "main", "b", "2026-07-01T11:30:00Z", ""),
		"", // trailing newline
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readRecentFrom(path, 12*time.Hour, now, defaultMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "fresh" {
		t.Fatalf("got %+v, want only fresh", got)
	}
}

func TestReadRecentFrom_MissingFileIsEmpty(t *testing.T) {
	got, err := readRecentFrom(filepath.Join(t.TempDir(), "nope.jsonl"), time.Hour, time.Now(), defaultMaxBytes)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d, want 0", len(got))
	}
}

func TestTailLines_DropsLeadingPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.jsonl")
	// Three lines; a tiny cap forces a mid-file start so the first (partial)
	// line must be dropped.
	if err := os.WriteFile(path, []byte("AAAA\nBBBB\nCCCC\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(path, 9) // covers "BB\nCCCC\n" region → starts mid-file
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "|")
	if strings.Contains(joined, "A") {
		t.Fatalf("leading partial line not dropped: %v", lines)
	}
	if !strings.Contains(joined, "CCCC") {
		t.Fatalf("expected complete final record CCCC, got %v", lines)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello\nworld", 100); got != "hello world" {
		t.Errorf("newlines not flattened: %q", got)
	}
	long := strings.Repeat("x", 200)
	got := truncate(long, 10)
	if len([]rune(got)) != 10 {
		t.Errorf("truncated len = %d runes, want 10 (%q)", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}
