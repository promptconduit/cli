package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/promptconduit/cli/internal/eventlog"
)

// A send that exhausts its retries is queued; the next successful send from
// the detached path replays it (same event_id, single attempt).
func TestFailedSendIsQueuedThenReplayed(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(body))
		mu.Unlock()
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	c := retryTestClient(t, srv, 5)
	eventlog.SetEnabled(true)
	t.Cleanup(func() { eventlog.SetEnabled(false) })

	first := `{"schema":2,"event_id":"queued-1","hook_event":"Stop"}`
	if err := c.sendEnvelopeWithRetry([]byte(first)); err == nil {
		t.Fatal("want failure while the server is down")
	}
	if eventlog.OutboxCount() != 1 {
		t.Fatalf("outbox = %d, want the failed envelope queued", eventlog.OutboxCount())
	}

	down.Store(false)
	second := `{"schema":2,"event_id":"live-2","hook_event":"Stop"}`
	if err := c.sendEnvelopeWithRetry([]byte(second)); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if eventlog.OutboxCount() != 0 {
		t.Fatalf("outbox = %d, want it drained after a success", eventlog.OutboxCount())
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last != first {
		t.Fatalf("last request = %s, want the replayed envelope", last)
	}
	if st := eventlog.LoadStatus(); st.Replayed != 1 {
		t.Fatalf("replayed = %d, want 1", st.Replayed)
	}
}

// 4xx is permanent: never queued.
func TestRejectedSendIsNotQueued(t *testing.T) {
	srv, _ := statusSequence(t, 400)
	c := retryTestClient(t, srv, 5)
	eventlog.SetEnabled(true)
	t.Cleanup(func() { eventlog.SetEnabled(false) })
	_ = c.sendEnvelopeWithRetry([]byte(testEnvelope))
	if eventlog.OutboxCount() != 0 {
		t.Fatal("a 4xx must not be queued for replay")
	}
}
