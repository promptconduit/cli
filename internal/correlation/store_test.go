package correlation

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadOrCreateTrace_NewSession(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	rec, err := s.LoadOrCreateTrace("sess-1")
	if err != nil {
		t.Fatalf("LoadOrCreateTrace: %v", err)
	}
	if rec.SessionID != "sess-1" {
		t.Errorf("session id = %q", rec.SessionID)
	}
	if !IsValidTraceID(rec.TraceID) {
		t.Errorf("invalid trace id %q", rec.TraceID)
	}

	// File exists on disk.
	if _, err := os.Stat(filepath.Join(dir, "traces", "sess-1.json")); err != nil {
		t.Errorf("trace file missing: %v", err)
	}
}

func TestLoadOrCreateTrace_Reuses(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	a, err := s.LoadOrCreateTrace("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.LoadOrCreateTrace("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.TraceID != b.TraceID {
		t.Errorf("trace ids differ: %s vs %s", a.TraceID, b.TraceID)
	}
}

func TestLoadOrCreateTrace_Concurrent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	const n = 50
	results := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			rec, err := s.LoadOrCreateTrace("race-session")
			if err != nil {
				t.Errorf("LoadOrCreateTrace: %v", err)
				return
			}
			results[i] = rec.TraceID
		}()
	}
	wg.Wait()

	first := results[0]
	for i, id := range results {
		if id != first {
			t.Errorf("goroutine %d got trace_id %s, want %s", i, id, first)
		}
	}
}

func TestLoadOrCreateTrace_EmptySession(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.LoadOrCreateTrace(""); err == nil {
		t.Fatal("expected error for empty session id")
	}
}

func TestRecordAndLookupSpan(t *testing.T) {
	s := NewStore(t.TempDir())

	if err := s.RecordSpan("s1", SpanKindToolUse, "toolu_abc", "00f067aa0ba902b7"); err != nil {
		t.Fatalf("RecordSpan: %v", err)
	}
	got := s.LookupParent("s1", SpanKindToolUse, "toolu_abc")
	if got != "00f067aa0ba902b7" {
		t.Errorf("LookupParent = %q, want 00f067aa0ba902b7", got)
	}

	// Missing key returns empty.
	if got := s.LookupParent("s1", SpanKindToolUse, "missing"); got != "" {
		t.Errorf("missing key lookup = %q", got)
	}
}

func TestLastPromptSubmit(t *testing.T) {
	s := NewStore(t.TempDir())

	if err := s.RecordLastPromptSubmit("s1", "aabbccddeeff0011"); err != nil {
		t.Fatalf("RecordLastPromptSubmit: %v", err)
	}
	if got := s.LookupLastPromptSubmit("s1"); got != "aabbccddeeff0011" {
		t.Errorf("LookupLastPromptSubmit = %q", got)
	}

	// Overwrite.
	if err := s.RecordLastPromptSubmit("s1", "1122334455667788"); err != nil {
		t.Fatalf("RecordLastPromptSubmit: %v", err)
	}
	if got := s.LookupLastPromptSubmit("s1"); got != "1122334455667788" {
		t.Errorf("after overwrite = %q", got)
	}
}

func TestRootSpan(t *testing.T) {
	s := NewStore(t.TempDir())

	if err := s.RecordRootSpan("s1", "1111111111111111"); err != nil {
		t.Fatalf("RecordRootSpan: %v", err)
	}
	if got := s.LookupRootSpan("s1"); got != "1111111111111111" {
		t.Errorf("LookupRootSpan = %q", got)
	}
}

func TestLoadSpans_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	// Pre-create a corrupt spans file.
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0700); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "traces", "s1.spans.json")
	if err := os.WriteFile(corrupt, []byte("not json {"), 0600); err != nil {
		t.Fatal(err)
	}

	// Should treat as empty, not crash.
	rec, err := s.LoadSpans("s1")
	if err != nil {
		t.Fatalf("LoadSpans: %v", err)
	}
	if rec == nil {
		t.Fatal("nil record")
	}
	if len(rec.ToolUses) != 0 {
		t.Errorf("expected empty ToolUses, got %v", rec.ToolUses)
	}
}

func TestLookupParent_UnknownKind(t *testing.T) {
	s := NewStore(t.TempDir())
	if got := s.LookupParent("s1", SpanKind("nonsense"), "key"); got != "" {
		t.Errorf("unknown kind lookup = %q", got)
	}
}

func TestRecordSpan_UnknownKind(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.RecordSpan("s1", SpanKind("nonsense"), "k", "00f067aa0ba902b7"); err == nil {
		t.Error("expected error for unknown kind")
	}
}
