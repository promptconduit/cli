package client

import (
	"errors"
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
	if eventlog.OutboxCount(srv.URL) != 1 {
		t.Fatalf("outbox = %d, want the failed envelope queued", eventlog.OutboxCount(srv.URL))
	}

	down.Store(false)
	second := `{"schema":2,"event_id":"live-2","hook_event":"Stop"}`
	if err := c.sendEnvelopeWithRetry([]byte(second)); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if eventlog.OutboxCount(srv.URL) != 0 {
		t.Fatalf("outbox = %d, want it drained after a success", eventlog.OutboxCount(srv.URL))
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
	if eventlog.OutboxCount(srv.URL) != 0 {
		t.Fatal("a 4xx must not be queued for replay")
	}
}

func TestReplayOutcome(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   eventlog.ReplayResult
	}{
		{201, nil, eventlog.ReplayDelivered},
		{400, errors.New("x"), eventlog.ReplayRejected},
		{422, errors.New("x"), eventlog.ReplayRejected},
		{401, errors.New("x"), eventlog.ReplayStop}, // auth may clear: keep it
		{403, errors.New("x"), eventlog.ReplayStop},
		{408, errors.New("x"), eventlog.ReplayStop},
		{500, errors.New("x"), eventlog.ReplaySkip}, // this envelope only
		{502, errors.New("x"), eventlog.ReplaySkip},
		{503, errors.New("x"), eventlog.ReplayStop}, // server-level
		{429, errors.New("x"), eventlog.ReplayStop},
		{0, errors.New("x"), eventlog.ReplayStop},
	}
	for _, c := range cases {
		if got := replayOutcome(c.status, c.err); got != c.want {
			t.Errorf("replayOutcome(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}
