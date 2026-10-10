package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/eventlog"
)

func queueForReplay(target string, n int) {
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < n; i++ {
		eventlog.AppendOutbox(target, []byte(fmt.Sprintf(`{"schema":2,"event_id":"q%d","captured_at":"%s"}`, i, now)))
	}
}

// DrainOutbox empties the queue against a live server even inside the
// background cooldown, with no new event being sent.
func TestClientDrainOutboxDeliversEverything(t *testing.T) {
	srv, calls := statusSequence(t, 201)
	c := retryTestClient(t, srv, 5)
	eventlog.SetEnabled(true)
	t.Cleanup(func() { eventlog.SetEnabled(false) })

	queueForReplay(srv.URL, 3)
	// A background pass just ran (stamping the cooldown) with nothing to send.
	eventlog.FlushOutbox(srv.URL, 0, func([]byte) eventlog.ReplayResult { return eventlog.ReplayDelivered })

	res, stopErr := c.DrainOutbox(context.Background(), nil)
	if stopErr != nil || res.Stopped || res.Delivered != 3 || res.Remaining != 0 {
		t.Fatalf("res=%+v stopErr=%v; want 3 delivered, none left", res, stopErr)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
}

// A server-level failure stops the drain, keeps the queue, and reports why.
func TestClientDrainOutboxStopsOnAuthFailure(t *testing.T) {
	srv, calls := statusSequence(t, http.StatusUnauthorized)
	c := retryTestClient(t, srv, 5)
	eventlog.SetEnabled(true)
	t.Cleanup(func() { eventlog.SetEnabled(false) })

	queueForReplay(srv.URL, 3)
	res, stopErr := c.DrainOutbox(context.Background(), nil)
	if !res.Stopped || res.Remaining != 3 || res.Delivered != 0 {
		t.Fatalf("res=%+v; want stopped with all 3 still queued", res)
	}
	if stopErr == nil || !strings.Contains(stopErr.Error(), "401") {
		t.Fatalf("stopErr = %v, want the 401 that stopped the drain", stopErr)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (stop on the first)", got)
	}
}

// A cancelled context (Ctrl-C) stops the drain without sending.
func TestClientDrainOutboxHonorsCancel(t *testing.T) {
	srv, calls := statusSequence(t, 201)
	c := retryTestClient(t, srv, 5)
	eventlog.SetEnabled(true)
	t.Cleanup(func() { eventlog.SetEnabled(false) })

	queueForReplay(srv.URL, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, stopErr := c.DrainOutbox(ctx, nil)
	if !res.Stopped || res.Remaining != 2 || stopErr == nil {
		t.Fatalf("res=%+v stopErr=%v; want stopped with both queued", res, stopErr)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("server saw %d requests after cancel, want 0", got)
	}
}
