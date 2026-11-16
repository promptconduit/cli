package correlation

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/filelock"
)

func TestLoadOrCreateTrace_SkipsRewriteWhileLastSeenIsFresh(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.LoadOrCreateTrace("s1"); err != nil {
		t.Fatal(err)
	}
	path := s.traceFile("s1")
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, old, old)

	if _, err := s.LoadOrCreateTrace("s1"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(old) {
		t.Fatalf("trace file rewritten although last_seen_at was fresh")
	}

	// Make last_seen_at stale: the next event refreshes it.
	rec, _ := readTrace(path)
	rec.LastSeenAt = time.Now().UTC().Add(-2 * lastSeenRefresh)
	if err := writeTraceAtomic(path, rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadOrCreateTrace("s1")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(got.LastSeenAt) > time.Minute {
		t.Fatalf("stale last_seen_at not refreshed: %v", got.LastSeenAt)
	}
	if again, _ := readTrace(path); !again.LastSeenAt.Equal(got.LastSeenAt) {
		t.Fatalf("refresh not persisted")
	}
}

func TestGCKeepsHeldSpansLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	s := NewStore(t.TempDir())
	if err := s.RecordSpan("held", SpanKindToolUse, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSpan("free", SpanKindToolUse, "k", "v"); err != nil {
		t.Fatal(err)
	}
	heldLock := s.spansFile("held") + ".lock"
	freeLock := s.spansFile("free") + ".lock"
	release, ok := filelock.Exclusive(heldLock, time.Second)
	if !ok {
		t.Fatal("lock")
	}
	defer release()
	old := time.Now().Add(-2 * gcMaxAge)
	for _, p := range []string{heldLock, freeLock, s.spansFile("free")} {
		_ = os.Chtimes(p, old, old)
	}
	s.gc()
	if _, err := os.Stat(heldLock); err != nil {
		t.Error("gc removed a spans lock that is held")
	}
	if _, err := os.Stat(freeLock); !os.IsNotExist(err) {
		t.Error("gc should remove a stale, unheld lock")
	}
	if _, err := os.Stat(s.spansFile("free")); !os.IsNotExist(err) {
		t.Error("gc should still remove stale data files")
	}
}

func TestToolUseSpanSurvivesLongCalls(t *testing.T) {
	if toolUseSpanMaxAge < 24*time.Hour {
		t.Fatalf("toolUseSpanMaxAge = %v; long Task/subagent calls need >= 24h", toolUseSpanMaxAge)
	}
	rec := &SpansRecord{
		ToolUses:   map[string]string{"task": "span-task"},
		ToolUsesAt: map[string]time.Time{"task": time.Now().UTC().Add(-3 * time.Hour)},
	}
	pruneToolUses(rec, "new", time.Now().UTC())
	if rec.ToolUses["task"] != "span-task" {
		t.Fatal("a 3h-old in-flight tool call must keep its parent span")
	}
}

func TestToolUseLookupIsIdempotent(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.RecordSpan("s1", SpanKindToolUse, "tu-1", "span-pre"); err != nil {
		t.Fatal(err)
	}
	path := s.spansFile("s1")
	before, _ := os.Stat(path)
	// Duplicate hooks / Failure after Post / replays all resolve the parent.
	for i := 0; i < 3; i++ {
		if got := s.LookupParent("s1", SpanKindToolUse, "tu-1"); got != "span-pre" {
			t.Fatalf("lookup %d = %q", i, got)
		}
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("PostToolUse lookups must not rewrite the spans file")
	}
}

func TestRecordSpan_AgesOutOldToolUses(t *testing.T) {
	s := NewStore(t.TempDir())
	// Seed: one stale entry, one fresh, one legacy entry with no timestamp.
	rec := &SpansRecord{
		ToolUses:   map[string]string{"old": "span-old", "fresh": "span-fresh", "legacy": "span-legacy"},
		ToolUsesAt: map[string]time.Time{"old": time.Now().UTC().Add(-2 * toolUseSpanMaxAge), "fresh": time.Now().UTC().Add(-time.Minute)},
		Subagents:  map[string]string{"agent": "span-agent"},
	}
	if err := s.writeSpans("s1", rec); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSpan("s1", SpanKindToolUse, "new", "span-new"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadSpans("s1")
	if _, ok := got.ToolUses["old"]; ok {
		t.Error("entry older than toolUseSpanMaxAge should be dropped")
	}
	for _, k := range []string{"fresh", "legacy", "new"} {
		if got.ToolUses[k] == "" {
			t.Errorf("%s should be kept", k)
		}
		if _, ok := got.ToolUsesAt[k]; !ok {
			t.Errorf("%s should carry a timestamp", k)
		}
	}
	if _, ok := got.ToolUsesAt["old"]; ok {
		t.Error("timestamp of a dropped entry should be removed too")
	}
	if got.Subagents["agent"] != "span-agent" {
		t.Error("other span kinds must be untouched")
	}
}

// TestSpans_ConcurrentRecordsLoseNothing models parallel tool calls in one
// session: concurrent PreToolUse records on the same spans file must all land.
func TestSpans_ConcurrentRecordsLoseNothing(t *testing.T) {
	// 40 writers queue on one lock; on a slow CI disk the hook-path wait could
	// expire and fall back to unlocked (best-effort) updates. Raise it so the
	// test checks the locking itself, not the fallback.
	prev := spansLockWait
	spansLockWait = 30 * time.Second
	t.Cleanup(func() { spansLockWait = prev })
	s := NewStore(t.TempDir())
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.RecordSpan("s1", SpanKindToolUse, fmt.Sprintf("tu-%d", i), fmt.Sprintf("span-%d", i))
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if got := s.LookupParent("s1", SpanKindToolUse, fmt.Sprintf("tu-%d", i)); got != fmt.Sprintf("span-%d", i) {
			t.Errorf("tu-%d parent = %q (concurrent record lost)", i, got)
		}
	}
}
