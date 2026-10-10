package correlation

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
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

func TestConsumeToolUseParent_ReturnsOnceAndRemoves(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.RecordSpan("s1", SpanKindToolUse, "tu-1", "span-pre"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSpan("s1", SpanKindToolUse, "tu-2", "span-pre-2"); err != nil {
		t.Fatal(err)
	}
	if got := s.ConsumeToolUseParent("s1", "tu-1"); got != "span-pre" {
		t.Fatalf("parent = %q", got)
	}
	rec, _ := s.LoadSpans("s1")
	if _, ok := rec.ToolUses["tu-1"]; ok {
		t.Fatal("consumed entry must be removed")
	}
	if rec.ToolUses["tu-2"] != "span-pre-2" {
		t.Fatal("other in-flight entries must be kept")
	}
	if got := s.ConsumeToolUseParent("s1", "tu-1"); got != "" {
		t.Fatalf("second consume = %q, want empty", got)
	}
	if got := s.ConsumeToolUseParent("s1", "missing"); got != "" {
		t.Fatalf("missing = %q", got)
	}
}

// TestSpans_ConcurrentRecordAndConsumeLoseNothing models parallel tool calls
// in one session: concurrent PreToolUse records racing PostToolUse consumes on
// the same spans file. Every record must be consumable exactly once and the
// file must end empty (bounded).
func TestSpans_ConcurrentRecordAndConsumeLoseNothing(t *testing.T) {
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
	rec, _ := s.LoadSpans("s1")
	if len(rec.ToolUses) != n {
		t.Fatalf("concurrent records lost: have %d of %d", len(rec.ToolUses), n)
	}

	got := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = s.ConsumeToolUseParent("s1", fmt.Sprintf("tu-%d", i))
		}(i)
	}
	wg.Wait()
	for i, g := range got {
		if g != fmt.Sprintf("span-%d", i) {
			t.Errorf("tu-%d parent = %q", i, g)
		}
	}
	rec, _ = s.LoadSpans("s1")
	if len(rec.ToolUses) != 0 {
		t.Fatalf("spans file should be empty after all tools completed, has %d", len(rec.ToolUses))
	}
}
