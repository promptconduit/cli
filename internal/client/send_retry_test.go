package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/eventlog"
)

// retryTestClient points a client at srv with tiny retry delays and the event
// log redirected to a temp dir.
func retryTestClient(t *testing.T, srv *httptest.Server, timeoutSeconds int) *Client {
	t.Helper()
	eventlog.SetDirForTest(t.TempDir())
	t.Cleanup(func() { eventlog.SetDirForTest("") })
	old := sendRetryDelays
	sendRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sendRetryDelays = old })
	return NewClient(&Config{APIURL: srv.URL, APIKey: "test", TimeoutSeconds: timeoutSeconds}, "test")
}

// statusSequence serves the given statuses in order (repeating the last).
func statusSequence(t *testing.T, statuses ...int) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&calls, 1)) - 1
		if n >= len(statuses) {
			n = len(statuses) - 1
		}
		w.WriteHeader(statuses[n])
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const testEnvelope = `{"schema":2,"event_id":"e1","hook_event":"Stop"}`

func TestSendWithRetryRecoversFromTransient5xx(t *testing.T) {
	srv, calls := statusSequence(t, 500, 503, 201)
	c := retryTestClient(t, srv, 5)
	if err := c.sendEnvelopeWithRetry([]byte(testEnvelope)); err != nil {
		t.Fatalf("want success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

// Every exhausted failure carries the attempt count.
func TestSendWithRetryErrorNamesAttempts(t *testing.T) {
	srv, _ := statusSequence(t, 503)
	c := retryTestClient(t, srv, 5)
	err := c.sendEnvelopeWithRetry([]byte(testEnvelope))
	if err == nil || !strings.Contains(err.Error(), "(after 3 attempts)") {
		t.Fatalf("err = %v, want it to name 3 attempts", err)
	}
}

func TestSendWithRetryGivesUpAfterLimit(t *testing.T) {
	srv, calls := statusSequence(t, 503)
	c := retryTestClient(t, srv, 5)
	if err := c.sendEnvelopeWithRetry([]byte(testEnvelope)); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if got := atomic.LoadInt32(calls); got != int32(len(sendRetryDelays)+1) {
		t.Fatalf("calls = %d, want %d", got, len(sendRetryDelays)+1)
	}
}

func TestSendWithRetryDoesNotRetryClientErrors(t *testing.T) {
	srv, calls := statusSequence(t, 400)
	c := retryTestClient(t, srv, 5)
	if err := c.sendEnvelopeWithRetry([]byte(testEnvelope)); err == nil {
		t.Fatal("want error for 400")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (4xx is not retryable)", got)
	}
}

func TestSendWithRetryDoesNotRetryTimeouts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c := retryTestClient(t, srv, 1)
	if err := c.sendEnvelopeWithRetry([]byte(testEnvelope)); err == nil {
		t.Fatal("want timeout error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (timeouts are not retried)", got)
	}
}

// The in-process fallback path stays single-attempt so it never blocks a hook.
func TestSendBlockingIsSingleAttempt(t *testing.T) {
	srv, calls := statusSequence(t, 500, 201)
	c := retryTestClient(t, srv, 5)
	if err := c.sendEnvelopeBlocking([]byte(testEnvelope)); err == nil {
		t.Fatal("want the 500 surfaced without retry")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestRetryableSend(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   bool
	}{
		{429, errors.New("x"), true},
		{500, errors.New("x"), true},
		{502, errors.New("x"), true},
		{503, errors.New("x"), true},
		{504, errors.New("x"), false}, // gateway timeout = overload, not retried
		{400, errors.New("x"), false},
		{401, errors.New("x"), false},
		{0, syscall.ECONNREFUSED, true},
		{0, syscall.ECONNRESET, true},
		{0, io.EOF, true},
		{0, context.DeadlineExceeded, false},
		{0, &net.DNSError{Err: "no such host", Name: "api.example", IsNotFound: true}, false},
		{0, errors.New("tls: failed to verify certificate"), false},
	}
	for _, c := range cases {
		if got := retryableSend(c.status, c.err); got != c.want {
			t.Errorf("retryableSend(%d, %v) = %v, want %v", c.status, c.err, got, c.want)
		}
	}
}

// A Retry-After longer than maxRetryAfter means the server is down: give up.
func TestSendWithRetryGivesUpOnLongRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c := retryTestClient(t, srv, 5)
	if err := c.sendEnvelopeWithRetry([]byte(testEnvelope)); err == nil {
		t.Fatal("want error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (Retry-After beyond the cap)", got)
	}
}

// A wait that would push past retryWindow is not taken.
func TestSendWithRetryRespectsWindow(t *testing.T) {
	srv, calls := statusSequence(t, 503)
	c := retryTestClient(t, srv, 5)
	sendRetryDelays = []time.Duration{retryWindow + time.Second}
	if err := c.sendEnvelopeWithRetry([]byte(testEnvelope)); err == nil {
		t.Fatal("want error")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (wait would exceed the retry window)", got)
	}
}

// A connection dropped mid-request (no HTTP response) is retried.
func TestSendWithRetryRecoversFromDroppedConnection(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close() // client sees EOF / connection reset
			}
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	c := retryTestClient(t, srv, 5)
	if err := c.sendEnvelopeWithRetry([]byte(testEnvelope)); err != nil {
		t.Fatalf("want success after a dropped connection, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("2") != 2*time.Second || parseRetryAfter("") != 0 || parseRetryAfter("Wed, 21 Oct 2015") != 0 {
		t.Fatal("parseRetryAfter: want seconds form only")
	}
}
