package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/eventlog"
)

// flushTestServer answers every event POST with status and counts requests.
func flushTestServer(t *testing.T, status int) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	eventlog.SetDirForTest(t.TempDir())
	eventlog.SetEnabled(true)
	t.Cleanup(func() {
		eventlog.SetDirForTest("")
		eventlog.SetEnabled(false)
	})
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 3; i++ {
		eventlog.AppendOutbox(srv.URL, []byte(fmt.Sprintf(`{"schema":2,"event_id":"f%d","captured_at":"%s"}`, i, now)))
	}
	return srv, &calls
}

func TestFlushOutboxNowDrains(t *testing.T) {
	srv, calls := flushTestServer(t, http.StatusCreated)
	cfg := &client.Config{APIURL: srv.URL, APIKey: "k", TimeoutSeconds: 5}
	var out bytes.Buffer
	if err := flushOutboxNow(context.Background(), &out, cfg, false, false); err != nil {
		t.Fatalf("flush: %v\n%s", err, out.String())
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
	if !strings.Contains(out.String(), "Flush complete: 3 delivered · 0 rejected · 0 remaining") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	if eventlog.OutboxCount(srv.URL) != 0 {
		t.Fatal("outbox should be empty")
	}
}

func TestFlushOutboxNowDryRunSendsNothing(t *testing.T) {
	srv, calls := flushTestServer(t, http.StatusCreated)
	cfg := &client.Config{APIURL: srv.URL, APIKey: "k", TimeoutSeconds: 5}
	var out bytes.Buffer
	if err := flushOutboxNow(context.Background(), &out, cfg, true, false); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(calls) != 0 || eventlog.OutboxCount(srv.URL) != 3 {
		t.Fatal("dry run must not send or change the queue")
	}
	if !strings.Contains(out.String(), "[dry-run] 3 event(s) queued") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

func TestFlushOutboxNowRespectsLocalOnly(t *testing.T) {
	srv, calls := flushTestServer(t, http.StatusCreated)
	for _, cfg := range []*client.Config{
		{APIURL: srv.URL, APIKey: "k", LocalOnly: true, TimeoutSeconds: 5},
		{APIURL: srv.URL, TimeoutSeconds: 5}, // no API key
	} {
		var out bytes.Buffer
		if err := flushOutboxNow(context.Background(), &out, cfg, false, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Nothing was sent") {
			t.Fatalf("want an explanation, got:\n%s", out.String())
		}
		// quiet (the sync path) prints nothing at all.
		out.Reset()
		_ = flushOutboxNow(context.Background(), &out, cfg, false, true)
		if out.Len() != 0 {
			t.Fatalf("quiet mode printed:\n%s", out.String())
		}
	}
	if atomic.LoadInt32(calls) != 0 || eventlog.OutboxCount(srv.URL) != 3 {
		t.Fatal("local-only / no key must not send")
	}
}

func TestFlushOutboxNowReportsServerFailure(t *testing.T) {
	srv, _ := flushTestServer(t, http.StatusServiceUnavailable)
	cfg := &client.Config{APIURL: srv.URL, APIKey: "k", TimeoutSeconds: 5}
	var out bytes.Buffer
	err := flushOutboxNow(context.Background(), &out, cfg, false, false)
	if err == nil || !strings.Contains(err.Error(), "3 event(s) still queued") {
		t.Fatalf("err = %v, want a stopped-early error", err)
	}
	if eventlog.OutboxCount(srv.URL) != 3 {
		t.Fatal("the queue must be kept when the server is down")
	}
}

// A flush that finds the lock taken sends nothing and fails, so scripts can
// tell nothing was sent.
func TestFlushOutboxNowBusyIsAnError(t *testing.T) {
	srv, calls := flushTestServer(t, http.StatusCreated)
	// The lock sits next to the outbox: outbox-<key>.jsonl -> .outbox-<key>.lock.
	base := filepath.Base(eventlog.OutboxPath(srv.URL))
	lock := filepath.Join(eventlog.Dir(), "."+strings.TrimSuffix(base, ".jsonl")+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &client.Config{APIURL: srv.URL, APIKey: "k", TimeoutSeconds: 5}
	var out bytes.Buffer
	if err := flushOutboxNow(context.Background(), &out, cfg, false, false); !errors.Is(err, errFlushBusy) {
		t.Fatalf("err = %v, want errFlushBusy", err)
	}
	if atomic.LoadInt32(calls) != 0 || eventlog.OutboxCount(srv.URL) != 3 {
		t.Fatal("a busy flush must not send")
	}
}

func TestBlankLineBeforeOnlyWhenWritten(t *testing.T) {
	var buf bytes.Buffer
	w := &blankLineBefore{w: &buf}
	if buf.Len() != 0 {
		t.Fatal("no output expected without writes")
	}
	_, _ = fmt.Fprint(w, "a\n")
	_, _ = fmt.Fprint(w, "b\n")
	if buf.String() != "\na\nb\n" {
		t.Fatalf("got %q", buf.String())
	}
}
