// Package graph serves the live Session Graph as a local web page. It is a thin
// tail-and-serve shell: it streams the raw v2 envelope lines from
// ~/.promptconduit/events.jsonl to the browser, which builds and renders the
// graph using the SAME portable code the editor extension uses (embedded here as
// a prebuilt bundle in ui/graph.html). The graph LOGIC lives in one place —
// coupled to neither the editor nor to Go.
package graph

import (
	"context"
	"sync"

	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/promptconduit/cli/internal/outbound"
)

// ringCap bounds the in-memory buffer of recent lines. events.jsonl is retention-
// bounded (not size-bounded), so a live viewer keeps only the newest lines; the
// client's graph builder tolerates starting mid-stream, and `backfill` seeds
// enough recent history to reconstruct current sessions.
const ringCap = 100_000

// Hub tails events.jsonl once and buffers the raw lines in memory, so any number
// of browser tabs can poll /api/events without re-reading the file per request.
// Each line gets a monotonic index; clients pass the highest index they've seen
// as ?after= and receive everything newer.
type Hub struct {
	mu    sync.Mutex
	lines [][]byte // ring of the most recent lines
	first int      // global index of lines[0]
	next  int      // global index to assign to the next appended line
}

// NewHub starts tailing path (default eventlog.EventsJSONLPath when empty),
// backfilling the last `backfill` lines, until ctx is cancelled.
func NewHub(ctx context.Context, path string, backfill int) *Hub {
	if path == "" {
		path = eventlog.EventsJSONLPath()
	}
	h := &Hub{}
	go h.run(ctx, path, backfill)
	return h
}

func (h *Hub) run(ctx context.Context, path string, backfill int) {
	for raw := range outbound.Tail(ctx, path, backfill) {
		h.append(raw)
	}
}

func (h *Hub) append(line []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Copy: Tail reuses/owns its slices only until the next send, and we retain.
	h.lines = append(h.lines, append([]byte(nil), line...))
	h.next++
	if len(h.lines) > ringCap {
		drop := len(h.lines) - ringCap
		h.lines = h.lines[drop:]
		h.first += drop
	}
}

// Batch is the JSON shape returned by /api/events.
type Batch struct {
	// Lines are raw v2 envelope JSON strings, oldest first.
	Lines []string `json:"lines"`
	// Cursor is the highest index included; pass it back as ?after= next poll.
	Cursor int `json:"cursor"`
	// More is true when older-than-buffer lines existed or a limit truncated the
	// batch, so the client should poll again immediately to catch up.
	More bool `json:"more"`
}

// Since returns up to `limit` lines with a global index greater than `after`.
// A client starts at after=0 (or -1) to get the full buffer, then advances its
// cursor. Requests for lines already evicted from the ring resume at the ring's
// oldest available line.
func (h *Hub) Since(after, limit int) Batch {
	h.mu.Lock()
	defer h.mu.Unlock()

	start := after + 1
	if start < h.first {
		start = h.first // client fell behind the ring; resume at oldest kept line
	}
	// start..h.next is the outstanding range; clamp to the buffer.
	if start >= h.next {
		return Batch{Lines: []string{}, Cursor: after, More: false}
	}
	end := h.next
	more := false
	if limit > 0 && end-start > limit {
		end = start + limit
		more = true
	}
	out := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, string(h.lines[i-h.first]))
	}
	return Batch{Lines: out, Cursor: end - 1, More: more}
}
