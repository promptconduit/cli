package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/eventlog"
)

// fakeAPI is an httptest server speaking the batch + single event contract.
type fakeAPI struct {
	srv *httptest.Server

	mu           sync.Mutex
	batches      [][]string // event_ids per batch request
	batchBytes   []int
	singles      []string // event_ids posted to /v1/events/raw
	batchStatus  int      // non-zero: reply this status to batch requests
	singleStatus int      // non-zero: reply this status to single requests
	reject       map[string]string
	maxBody      int // non-zero: 413 for batch bodies larger than this
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{reject: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case batchEventsEndpoint:
		if f.batchStatus != 0 {
			w.WriteHeader(f.batchStatus)
			return
		}
		if f.maxBody > 0 && len(body) > f.maxBody {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		var req struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		type rej struct {
			Index int    `json:"index"`
			Error string `json:"error"`
		}
		resp := struct {
			Accepted int   `json:"accepted"`
			Rejected []rej `json:"rejected"`
		}{Rejected: []rej{}}
		var ids []string
		for i, e := range req.Events {
			id, _ := envelopeIDs(e)
			ids = append(ids, id)
			if msg, bad := f.reject[id]; bad {
				resp.Rejected = append(resp.Rejected, rej{i, msg})
			} else {
				resp.Accepted++
			}
		}
		f.batches = append(f.batches, ids)
		f.batchBytes = append(f.batchBytes, len(body))
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(resp)
	case eventsEndpoint:
		if f.singleStatus != 0 {
			w.WriteHeader(f.singleStatus)
			return
		}
		id, _ := envelopeIDs(body)
		f.singles = append(f.singles, id)
		w.WriteHeader(http.StatusAccepted) // the single endpoint may now answer 202
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAPI) set(fn func(f *fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeAPI) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

// batchTestClient points a client at srv with a hermetic event log and
// config dir, and stubs out the detached flusher spawn.
func batchTestClient(t *testing.T, url string) (*Client, *int32) {
	t.Helper()
	t.Setenv(EnvXDGConfigHome, t.TempDir()) // keep the outbound mirror out of ~/.config
	eventlog.SetDirForTest(t.TempDir())
	eventlog.SetEnabled(true)
	t.Cleanup(func() {
		eventlog.SetDirForTest("")
		eventlog.SetEnabled(false)
	})
	var spawns int32
	old := startSpoolFlusher
	startSpoolFlusher = func() error { atomic.AddInt32(&spawns, 1); return nil }
	t.Cleanup(func() { startSpoolFlusher = old })
	return NewClient(&Config{APIURL: url, APIKey: "test", TimeoutSeconds: 5}, "test"), &spawns
}

func spoolN(t *testing.T, c *Client, n int, pad int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ev-%03d", i)
		env := fmt.Sprintf(`{"schema":2,"event_id":%q,"hook_event":"PostToolUse","captured_at":%q,"pad":%q}`,
			id, time.Now().UTC().Format(time.RFC3339), strings.Repeat("x", pad))
		if err := eventlog.AppendSpool(c.config.APIURL, []byte(env)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestFlushSpoolSendsOneBatch(t *testing.T) {
	api := newFakeAPI(t)
	c, _ := batchTestClient(t, api.srv.URL)
	ids := spoolN(t, c, 5, 0)

	rep := c.FlushSpool(context.Background())
	if rep.Delivered != 5 || rep.Remaining != 0 || rep.Stopped {
		t.Fatalf("rep = %+v", rep)
	}
	if len(api.batches) != 1 || strings.Join(api.batches[0], ",") != strings.Join(ids, ",") {
		t.Fatalf("batches = %v", api.batches)
	}
	if len(api.singles) != 0 {
		t.Fatalf("no single sends expected, got %v", api.singles)
	}
	if st := eventlog.LoadStatus(); st.Sent != 5 {
		t.Fatalf("sent counter = %d, want 5", st.Sent)
	}
	if st := eventlog.LoadSpoolState(c.config.APIURL); st.LastResult != "ok" || st.LastDelivered != 5 {
		t.Fatalf("state = %+v", st)
	}
}

func TestFlushSpoolHonorsCountAndByteCaps(t *testing.T) {
	api := newFakeAPI(t)
	c, _ := batchTestClient(t, api.srv.URL)
	spoolN(t, c, 250, 0)
	rep := c.FlushSpool(context.Background())
	if rep.Delivered != 250 || len(api.batches) != 3 {
		t.Fatalf("rep=%+v batches=%d", rep, len(api.batches))
	}
	for _, b := range api.batches {
		if len(b) > BatchMaxEvents {
			t.Fatalf("batch of %d exceeds %d", len(b), BatchMaxEvents)
		}
	}

	// ~20 KB envelopes: 100 would be ~2 MB, so the byte cap must split them.
	api2 := newFakeAPI(t)
	c2, _ := batchTestClient(t, api2.srv.URL)
	spoolN(t, c2, 100, 20*1024)
	rep = c2.FlushSpool(context.Background())
	if rep.Delivered != 100 || len(api2.batches) < 3 {
		t.Fatalf("rep=%+v batches=%d", rep, len(api2.batches))
	}
	for _, n := range api2.batchBytes {
		if n > BatchMaxBytes+1024 {
			t.Fatalf("batch body %d bytes exceeds cap %d", n, BatchMaxBytes)
		}
	}
}

func TestFlushSpoolPartialRejection(t *testing.T) {
	api := newFakeAPI(t)
	c, _ := batchTestClient(t, api.srv.URL)
	spoolN(t, c, 4, 0)
	api.set(func(f *fakeAPI) { f.reject["ev-001"] = "invalid schema" })

	rep := c.FlushSpool(context.Background())
	if rep.Delivered != 3 || rep.Rejected != 1 || rep.Remaining != 0 {
		t.Fatalf("rep = %+v", rep)
	}
	if api.batchCount() != 1 {
		t.Fatalf("rejected entries must not be retried; batches=%v", api.batches)
	}
	st := eventlog.LoadStatus()
	if st.Sent != 3 || st.Failed != 1 || !strings.Contains(st.LastError, "invalid schema") {
		t.Fatalf("status = %+v", st)
	}
	if got := eventlog.LoadSpoolState(c.config.APIURL).LastResult; got != "partial" {
		t.Fatalf("last result = %q, want partial", got)
	}
}

func TestFlushSpool5xxKeepsEntriesAndBacksOff(t *testing.T) {
	api := newFakeAPI(t)
	c, spawns := batchTestClient(t, api.srv.URL)
	spoolN(t, c, 3, 0)
	api.set(func(f *fakeAPI) { f.batchStatus = http.StatusServiceUnavailable })

	rep := c.FlushSpool(context.Background())
	if !rep.Stopped || rep.Remaining != 3 || rep.Delivered != 0 || rep.StopErr == nil {
		t.Fatalf("rep = %+v", rep)
	}
	if eventlog.SpoolCount(c.config.APIURL) != 3 {
		t.Fatal("a 5xx must keep every entry")
	}
	if st := eventlog.LoadStatus(); st.Failed != 0 || st.LastError == "" {
		t.Fatalf("transient failure should note the error without counting failures: %+v", st)
	}
	st := eventlog.LoadSpoolState(c.config.APIURL)
	if st.LastResult != "error" || st.ConsecutiveFailures != 1 || !st.NextAttemptAt.After(time.Now()) {
		t.Fatalf("state = %+v", st)
	}
	// In backoff an automatic trigger won't spawn a flusher, a forced one will.
	if c.MaybeTriggerFlush(false) || atomic.LoadInt32(spawns) != 0 {
		t.Fatal("automatic trigger must honor the backoff")
	}
	if !c.MaybeTriggerFlush(true) {
		t.Fatal("forced trigger (end of turn) should ignore the backoff")
	}

	// Server recovers: the retry delivers everything and clears the backoff.
	api.set(func(f *fakeAPI) { f.batchStatus = 0 })
	rep = c.FlushSpool(context.Background())
	if rep.Delivered != 3 || rep.Remaining != 0 {
		t.Fatalf("retry rep = %+v", rep)
	}
	if st := eventlog.LoadSpoolState(c.config.APIURL); st.ConsecutiveFailures != 0 || !st.NextAttemptAt.IsZero() {
		t.Fatalf("state after recovery = %+v", st)
	}
}

func TestFlushSpoolAuthFailureKeepsEntries(t *testing.T) {
	api := newFakeAPI(t)
	c, _ := batchTestClient(t, api.srv.URL)
	c.config.APIKey = "wrong"
	spoolN(t, c, 2, 0)
	rep := c.FlushSpool(context.Background())
	if !rep.Stopped || rep.Remaining != 2 {
		t.Fatalf("401 must keep entries for after the key is fixed: rep = %+v", rep)
	}
}

func TestFlushSpoolFallsBackOn404And405(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			api := newFakeAPI(t)
			c, _ := batchTestClient(t, api.srv.URL)
			ids := spoolN(t, c, 3, 0)
			api.set(func(f *fakeAPI) { f.batchStatus = status })

			rep := c.FlushSpool(context.Background())
			if rep.Delivered != 3 || rep.Remaining != 0 || !rep.SingleEndpoint {
				t.Fatalf("rep = %+v", rep)
			}
			if strings.Join(api.singles, ",") != strings.Join(ids, ",") {
				t.Fatalf("singles = %v, want %v (202 counts as success)", api.singles, ids)
			}
			st := eventlog.LoadSpoolState(c.config.APIURL)
			if !st.BatchUnsupportedUntil.After(time.Now().Add(50 * time.Minute)) {
				t.Fatalf("404/405 should be remembered for ~1h: %+v", st)
			}

			// While remembered, the batch endpoint isn't tried at all.
			api.set(func(f *fakeAPI) { f.batchStatus = 0 })
			spoolN(t, c, 2, 0)
			c.FlushSpool(context.Background())
			if api.batchCount() != 0 || len(api.singles) != 5 {
				t.Fatalf("batches=%v singles=%v", api.batches, api.singles)
			}
		})
	}
}

func TestFlushSpoolSplitsOn413(t *testing.T) {
	api := newFakeAPI(t)
	c, _ := batchTestClient(t, api.srv.URL)
	spoolN(t, c, 8, 1000)
	api.set(func(f *fakeAPI) { f.maxBody = 3000 }) // ~2 envelopes per request
	rep := c.FlushSpool(context.Background())
	if rep.Delivered != 8 || rep.Remaining != 0 {
		t.Fatalf("rep = %+v", rep)
	}
	if api.batchCount() < 4 {
		t.Fatalf("expected the batch to be split, got %v", api.batches)
	}
}

func TestFlushSpoolOversizedEnvelopeUsesSingleEndpoint(t *testing.T) {
	api := newFakeAPI(t)
	c, _ := batchTestClient(t, api.srv.URL)
	spoolN(t, c, 1, BatchMaxBytes+10)
	rep := c.FlushSpool(context.Background())
	if rep.Delivered != 1 || len(api.singles) != 1 || api.batchCount() != 0 {
		t.Fatalf("rep=%+v singles=%v batches=%v", rep, api.singles, api.batches)
	}
}

func TestFlushSpoolBatchUploadDisabledUsesSingles(t *testing.T) {
	api := newFakeAPI(t)
	c, _ := batchTestClient(t, api.srv.URL)
	off := false
	c.config.BatchUpload = &off
	spoolN(t, c, 2, 0)
	rep := c.FlushSpool(context.Background())
	if rep.Delivered != 2 || len(api.singles) != 2 || api.batchCount() != 0 {
		t.Fatalf("leftover spool with batching off should drain via singles: rep=%+v", rep)
	}
}

func TestQueueEnvelopeSpoolsAndTriggers(t *testing.T) {
	api := newFakeAPI(t)
	c, spawns := batchTestClient(t, api.srv.URL)
	env := envelope.New("test", "claude-code", "PostToolUse", "s", "p", []byte(`{"a":1}`), nil)
	if err := c.QueueEnvelope(env, false); err != nil {
		t.Fatal(err)
	}
	if eventlog.SpoolCount(api.srv.URL) != 1 {
		t.Fatal("envelope should be spooled")
	}
	if atomic.LoadInt32(spawns) != 0 {
		t.Fatal("one fresh event is not due; no flusher expected")
	}
	if api.batchCount() != 0 || len(api.singles) != 0 {
		t.Fatal("queueing must not send inline")
	}
	stop := envelope.New("test", "claude-code", "Stop", "s", "p", []byte(`{"a":1}`), nil)
	if err := c.QueueEnvelope(stop, true); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(spawns) != 1 {
		t.Fatalf("flush-point event should start a flusher; spawns=%d", *spawns)
	}

	// Due by count.
	spoolN(t, c, SpoolDueCount, 0)
	if !c.MaybeTriggerFlush(false) {
		t.Fatal("spool past SpoolDueCount should be due")
	}
}

func TestMaybeTriggerFlushSkipsWhileFlusherRuns(t *testing.T) {
	api := newFakeAPI(t)
	c, spawns := batchTestClient(t, api.srv.URL)
	spoolN(t, c, SpoolDueCount, 0)
	// Simulate a live flusher holding the lock.
	res := eventlog.FlushSpool(api.srv.URL, eventlog.SpoolFlushLimits{MaxEvents: 1, MaxBytes: 1 << 20, MaxBatches: 1},
		func(batch [][]byte) []eventlog.ReplayResult {
			if c.MaybeTriggerFlush(true) {
				t.Error("must not spawn a second flusher while one holds the lock")
			}
			return make([]eventlog.ReplayResult, len(batch))
		})
	if res.Delivered != 1 || atomic.LoadInt32(spawns) != 0 {
		t.Fatalf("res=%+v spawns=%d", res, *spawns)
	}
}
