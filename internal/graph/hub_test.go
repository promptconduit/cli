package graph

import "testing"

func newHubWith(lines ...string) *Hub {
	h := &Hub{}
	for _, l := range lines {
		h.append([]byte(l))
	}
	return h
}

func TestSinceReturnsAllFromStart(t *testing.T) {
	h := newHubWith("a", "b", "c")
	b := h.Since(-1, 5000)
	if got := b.Lines; len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("lines = %v, want [a b c]", got)
	}
	if b.Cursor != 2 {
		t.Fatalf("cursor = %d, want 2", b.Cursor)
	}
	if b.More {
		t.Fatalf("more = true, want false")
	}
}

func TestSinceAdvancesCursor(t *testing.T) {
	h := newHubWith("a", "b")
	first := h.Since(-1, 5000)
	// Nothing new yet.
	if b := h.Since(first.Cursor, 5000); len(b.Lines) != 0 || b.Cursor != 1 {
		t.Fatalf("empty poll = %+v, want no lines cursor 1", b)
	}
	// New line arrives.
	h.append([]byte("c"))
	b := h.Since(first.Cursor, 5000)
	if len(b.Lines) != 1 || b.Lines[0] != "c" || b.Cursor != 2 {
		t.Fatalf("incremental poll = %+v, want [c] cursor 2", b)
	}
}

func TestSinceLimitSetsMore(t *testing.T) {
	h := newHubWith("a", "b", "c", "d")
	b := h.Since(-1, 2)
	if len(b.Lines) != 2 || b.Lines[0] != "a" || b.Lines[1] != "b" {
		t.Fatalf("lines = %v, want [a b]", b.Lines)
	}
	if b.Cursor != 1 || !b.More {
		t.Fatalf("got cursor=%d more=%v, want cursor=1 more=true", b.Cursor, b.More)
	}
	rest := h.Since(b.Cursor, 2)
	if len(rest.Lines) != 2 || rest.Lines[0] != "c" || rest.More {
		t.Fatalf("rest = %+v, want [c d] more=false", rest)
	}
}

func TestSinceResumesAfterEviction(t *testing.T) {
	h := &Hub{}
	// Force the ring past capacity so the oldest lines evict.
	total := ringCap + 10
	for i := 0; i < total; i++ {
		h.append([]byte("x"))
	}
	// A client stuck at an evicted cursor resumes at the oldest kept line, not
	// before it, and never re-reads out of bounds.
	b := h.Since(-1, ringCap*2)
	if len(b.Lines) != ringCap {
		t.Fatalf("kept %d lines, want ring cap %d", len(b.Lines), ringCap)
	}
	if b.Cursor != total-1 {
		t.Fatalf("cursor = %d, want %d", b.Cursor, total-1)
	}
}
