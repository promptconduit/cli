package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/promptconduit/cli/internal/logger"
)

// Batched event upload.
//
// On the batched path (the default; config `batch_upload`) a hook appends its
// envelope to the local upload spool (eventlog.AppendSpool) and returns. A
// detached `hook --flush-spool` process ships the spool to
// POST /v1/events/raw/batch in groups of up to BatchMaxEvents envelopes /
// BatchMaxBytes bytes, once the spool is due (SpoolDueCount envelopes or an
// envelope SpoolDueAge old) or right away on end-of-turn / end-of-session
// events. Envelopes with binary attachments never go through the spool: they
// keep the multipart single-event path.
//
// Batch response contract: 2xx {"accepted": n, "rejected": [{"index": i,
// "error": "..."}]}. Rejected entries are permanent validation failures —
// removed and logged, never retried. No response, 401/403/408/429 and 5xx
// keep every entry for a later flush (with backoff). 404/405 mean the server
// has no batch endpoint: the flusher uses the single-event endpoint and
// remembers that for batchUnsupportedTTL.

const (
	batchEventsEndpoint = "/v1/events/raw/batch"

	// BatchMaxEvents is the server's per-request event limit.
	BatchMaxEvents = 100
	// BatchMaxBytes caps the summed envelope bytes per request, under the
	// server's ~1 MB body limit. A lone envelope larger than this goes to the
	// single-event endpoint instead.
	BatchMaxBytes = 900 * 1024

	// SpoolDueCount and SpoolDueAge decide when a hook kicks off a flush.
	SpoolDueCount = 20
	SpoolDueAge   = 5 * time.Second

	// batchUnsupportedTTL: after a 404/405 from the batch endpoint, use the
	// single-event endpoint for this long before probing again.
	batchUnsupportedTTL = time.Hour

	// spoolFlushMaxBatches and spoolFlushBudget bound one pass so a large
	// backlog drains over several flushes instead of one long-lived process.
	spoolFlushMaxBatches = 50
	spoolFlushBudget     = 60 * time.Second
	// spoolFlushMaxPasses: a flusher re-runs to pick up envelopes appended
	// while it was sending (they land past the pass's read point).
	spoolFlushMaxPasses = 3

	// Backoff after a failed flush: spoolBackoffBase doubling per
	// consecutive failure, capped at spoolBackoffMax. Only automatic
	// triggers honor it; end-of-turn events and explicit commands don't.
	spoolBackoffBase = 5 * time.Second
	spoolBackoffMax  = 5 * time.Minute
)

// QueueEnvelope spools env for a batched upload and, when the spool is due
// (or flushNow is set, for end-of-turn events), starts a detached flusher.
// When the spool can't take the envelope (event log disabled, spool full, a
// write error) it falls back to the per-event detached send, so an event is
// never silently lost.
func (c *Client) QueueEnvelope(env *envelope.RawEventEnvelope, flushNow bool) error {
	envJSON, err := env.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize envelope: %w", err)
	}
	if err := eventlog.AppendSpool(c.config.APIURL, envJSON); err != nil {
		logger.Debug("spool unavailable (%v); sending this event directly", err)
		return c.startDetachedSender(envJSON)
	}
	c.MaybeTriggerFlush(flushNow)
	return nil
}

// MaybeTriggerFlush starts a detached flusher when the spool needs one: with
// force, whenever it holds anything; otherwise only when it is due and not in
// a failure backoff. Never starts one while a live flusher holds the lock.
// Cheap (a few stats and a small read), so safe on the hook hot path.
// Reports whether a flusher was started.
func (c *Client) MaybeTriggerFlush(force bool) bool {
	target := c.config.APIURL
	if eventlog.SpoolFlushing(target) {
		return false
	}
	if force {
		if info, err := os.Stat(eventlog.SpoolPath(target)); err != nil || info.Size() == 0 {
			return false
		}
	} else if !eventlog.SpoolDue(target, SpoolDueCount, SpoolDueAge) {
		return false
	}
	if err := startSpoolFlusher(); err != nil {
		logger.Debug("spool: could not start flusher: %v", err)
		return false
	}
	return true
}

// startSpoolFlusher spawns the detached `hook --flush-spool` process. A var
// so tests can observe or stub the spawn.
var startSpoolFlusher = func() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "hook", "--flush-spool")
	cmd.Stderr = openLogForStderr()
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

// SpoolFlushReport aggregates the passes of one FlushSpool call.
type SpoolFlushReport struct {
	eventlog.SpoolFlushResult
	// StopErr is the failure that ended the flush early, if any.
	StopErr error
	// SingleEndpoint is true when the flush used /v1/events/raw because the
	// server lacks the batch endpoint (or batching is disabled).
	SingleEndpoint bool
}

// FlushSpool sends everything spooled for this client's API URL, in batches,
// and records the outcome (and any backoff) in the spool state. ctx bounds the
// whole flush. Safe to call from any command: it returns Busy without sending
// when another flusher is running, and immediately when the spool is empty.
func (c *Client) FlushSpool(ctx context.Context) SpoolFlushReport {
	target := c.config.APIURL
	var rep SpoolFlushReport
	limits := eventlog.SpoolFlushLimits{
		MaxEvents:  BatchMaxEvents,
		MaxBytes:   BatchMaxBytes,
		MaxBatches: spoolFlushMaxBatches,
		Budget:     spoolFlushBudget,
	}
	for pass := 0; pass < spoolFlushMaxPasses; pass++ {
		res := eventlog.FlushSpool(target, limits, func(batch [][]byte) []eventlog.ReplayResult {
			return c.sendSpoolBatch(ctx, batch, &rep)
		})
		mergeFlushResult(&rep.SpoolFlushResult, res)
		if res.Busy || res.Stopped || res.Err != nil || res.Tried == 0 || res.Remaining == 0 {
			break
		}
		if res.Delivered+res.Rejected == 0 || ctx.Err() != nil {
			break
		}
	}
	if rep.Tried == 0 && rep.Expired == 0 {
		return rep // nothing happened (empty, or another flusher is busy)
	}
	c.recordFlushState(rep)
	if !rep.Stopped && rep.Delivered > 0 {
		// The server is reachable: replay anything the legacy per-event path
		// queued in the outbox, too.
		c.flushOutbox()
	}
	return rep
}

func mergeFlushResult(dst *eventlog.SpoolFlushResult, src eventlog.SpoolFlushResult) {
	dst.Busy = src.Busy
	dst.Batches += src.Batches
	dst.Tried += src.Tried
	dst.Delivered += src.Delivered
	dst.Rejected += src.Rejected
	dst.Skipped += src.Skipped
	dst.Expired += src.Expired
	dst.Stopped = dst.Stopped || src.Stopped
	dst.Remaining = src.Remaining
	if src.Err != nil {
		dst.Err = src.Err
	}
}

// recordFlushState persists the flush outcome and the failure backoff.
func (c *Client) recordFlushState(rep SpoolFlushReport) {
	failed := rep.Stopped || rep.Err != nil
	eventlog.UpdateSpoolState(c.config.APIURL, func(st *eventlog.SpoolState) {
		now := time.Now().UTC()
		st.LastFlushAt = now
		st.LastDelivered = rep.Delivered
		st.LastRejected = rep.Rejected
		switch {
		case failed:
			st.LastResult = "error"
			st.ConsecutiveFailures++
			backoff := spoolBackoffBase << uint(minInt(st.ConsecutiveFailures-1, 10))
			if backoff > spoolBackoffMax {
				backoff = spoolBackoffMax
			}
			st.NextAttemptAt = now.Add(backoff)
			switch {
			case rep.StopErr != nil:
				st.LastError = rep.StopErr.Error()
			case rep.Err != nil:
				st.LastError = rep.Err.Error()
			}
		default:
			st.LastResult = "ok"
			if rep.Rejected > 0 {
				st.LastResult = "partial"
			}
			st.LastError = ""
			st.ConsecutiveFailures = 0
			st.NextAttemptAt = time.Time{}
		}
	})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// batchResponse is the batch endpoint's 2xx body.
type batchResponse struct {
	Accepted int `json:"accepted"`
	Rejected []struct {
		Index int    `json:"index"`
		Error string `json:"error"`
	} `json:"rejected"`
}

// sendSpoolBatch sends one batch and classifies every envelope in it. It
// takes the single-event endpoint when batching is off, the server lacks the
// batch endpoint, or the batch is one oversized envelope.
func (c *Client) sendSpoolBatch(ctx context.Context, batch [][]byte, rep *SpoolFlushReport) []eventlog.ReplayResult {
	if !c.batchUsable() || (len(batch) == 1 && len(batch[0]) > BatchMaxBytes) {
		rep.SingleEndpoint = true
		return c.sendSingles(ctx, batch, rep)
	}

	start := time.Now()
	status, body, err := c.postBatch(ctx, batch)
	latency := time.Since(start)
	switch {
	case err == nil:
		return c.applyBatchResponse(batch, status, body)
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		logger.Debug("batch endpoint unsupported (HTTP %d); using %s for %s", status, eventsEndpoint, batchUnsupportedTTL)
		eventlog.UpdateSpoolState(c.config.APIURL, func(st *eventlog.SpoolState) {
			st.BatchUnsupportedUntil = time.Now().UTC().Add(batchUnsupportedTTL)
		})
		rep.SingleEndpoint = true
		return c.sendSingles(ctx, batch, rep)
	case status == http.StatusRequestEntityTooLarge && len(batch) > 1:
		// Our byte cap is an estimate of the server's; split and retry.
		mid := len(batch) / 2
		first := c.sendSpoolBatch(ctx, batch[:mid], rep)
		if anyStop(first) {
			return append(first, stopAll(len(batch)-mid)...)
		}
		return append(first, c.sendSpoolBatch(ctx, batch[mid:], rep)...)
	case status == http.StatusRequestEntityTooLarge:
		rep.SingleEndpoint = true
		return c.sendSingles(ctx, batch, rep)
	case batchServerLevel(status):
		detail := fmt.Sprintf("batch upload of %d event(s) failed (status=%d, %dms): %v", len(batch), status, latency.Milliseconds(), err)
		logger.Debug("%s", detail)
		eventlog.NoteSendError(detail)
		rep.StopErr = err
		return stopAll(len(batch))
	default:
		// Any other 4xx rejects the request as a whole (e.g. one envelope
		// broke the body). Fall back to per-event sends so only the bad
		// envelope is rejected.
		logger.Debug("batch rejected as a whole (HTTP %d); isolating per event", status)
		return c.sendSingles(ctx, batch, rep)
	}
}

// batchServerLevel reports whether a failed batch POST is the server's or
// the credentials' problem rather than the batch's: keep everything, retry
// later.
func batchServerLevel(status int) bool {
	switch status {
	case 0, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// applyBatchResponse turns a 2xx batch response into per-envelope results:
// everything is delivered except the indices the server rejected.
func (c *Client) applyBatchResponse(batch [][]byte, status int, body []byte) []eventlog.ReplayResult {
	results := make([]eventlog.ReplayResult, len(batch))
	var resp batchResponse
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &resp); err != nil {
			logger.Debug("batch response (HTTP %d) not parseable, treating all %d as accepted: %v", status, len(batch), err)
		}
	}
	rejected := 0
	lastErr := ""
	for _, r := range resp.Rejected {
		if r.Index < 0 || r.Index >= len(batch) || results[r.Index] == eventlog.ReplayRejected {
			continue
		}
		results[r.Index] = eventlog.ReplayRejected
		rejected++
		lastErr = r.Error
		id, hook := envelopeIDs(batch[r.Index])
		eventlog.Errorf("batch rejected event=%s event_id=%s index=%d: %s", dash(hook), dash(id), r.Index, r.Error)
	}
	eventlog.BumpSent(int64(len(batch) - rejected))
	if rejected > 0 {
		eventlog.BumpFailed(int64(rejected), "rejected by server: "+lastErr)
	}
	return results
}

// sendSingles sends each envelope to the single-event endpoint, one attempt
// each, classified like an outbox replay (see replayOutcome). Once one send
// reports a server-level failure the rest are kept without sending.
func (c *Client) sendSingles(ctx context.Context, batch [][]byte, rep *SpoolFlushReport) []eventlog.ReplayResult {
	results := make([]eventlog.ReplayResult, len(batch))
	stopped := false
	for i, envJSON := range batch {
		if stopped || ctx.Err() != nil {
			results[i] = eventlog.ReplayStop
			if !stopped && ctx.Err() != nil {
				rep.StopErr = ctx.Err()
				stopped = true
			}
			continue
		}
		start := time.Now()
		status, _, err := c.attemptEventSendCtx(ctx, envJSON)
		out := replayOutcome(status, err)
		results[i] = out
		switch out {
		case eventlog.ReplayDelivered, eventlog.ReplayRejected:
			// Terminal either way: count it (and log a rejection).
			recordEventSend(envJSON, status, time.Since(start), 1, err)
		case eventlog.ReplaySkip:
			eventlog.NoteSendError(fmt.Sprintf("send failed (status=%d), kept for retry: %v", status, err))
		default:
			eventlog.NoteSendError(fmt.Sprintf("send failed (status=%d), kept for retry: %v", status, err))
			rep.StopErr = err
			stopped = true
		}
	}
	return results
}

// batchUsable reports whether this flush may use the batch endpoint: batching
// is enabled and the server hasn't recently answered 404/405 for it.
func (c *Client) batchUsable() bool {
	if !c.config.BatchUploadEnabled() {
		return false
	}
	return !eventlog.LoadSpoolState(c.config.APIURL).BatchUnsupportedUntil.After(time.Now())
}

// postBatch makes one POST of the batch. status is 0 when no response was
// received; err is non-nil for any non-2xx.
func (c *Client) postBatch(parent context.Context, batch [][]byte) (status int, body []byte, err error) {
	var buf bytes.Buffer
	buf.WriteString(`{"events":[`)
	for i, env := range batch {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(env)
	}
	buf.WriteString(`]}`)

	ctx, cancel := context.WithTimeout(parent, time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+batchEventsEndpoint, &buf)
	if err != nil {
		return 0, nil, err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, body, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}
	return resp.StatusCode, body, nil
}

func anyStop(rs []eventlog.ReplayResult) bool {
	for _, r := range rs {
		if r == eventlog.ReplayStop {
			return true
		}
	}
	return false
}

func stopAll(n int) []eventlog.ReplayResult {
	rs := make([]eventlog.ReplayResult, n)
	for i := range rs {
		rs[i] = eventlog.ReplayStop
	}
	return rs
}

func envelopeIDs(envJSON []byte) (eventID, hookEvent string) {
	var probe struct {
		EventID   string `json:"event_id"`
		HookEvent string `json:"hook_event"`
	}
	_ = json.Unmarshal(envJSON, &probe)
	return probe.EventID, probe.HookEvent
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
