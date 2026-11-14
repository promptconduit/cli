package eventlog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// The upload spool holds envelopes waiting to be sent in batches. A hook
// appends its envelope here (one O_APPEND write, under the shared append lock)
// instead of POSTing it, and a detached flusher ships the spool to the batch
// endpoint in groups. It sits beside the outbox and uses the same building
// blocks (appendLine, the O_EXCL flush lock kept fresh, rewriteFile), but the
// roles differ: the outbox only ever holds sends that already failed, while
// every event passes through the spool on the batched path. A spooled event
// that fails transiently simply stays in the spool for the next flush.
//
// Like the outbox there is one spool per API URL (target), so events captured
// against prod are only ever flushed to prod.
//
// Crash safety: entries are removed only after the server accepted or
// permanently rejected them, by a rewrite that happens after the sends. A
// flusher killed between the send and the rewrite leaves the entries in
// place, and they are sent again — the server dedupes on event_id.

const (
	// spoolCeiling caps a spool on disk. Past it AppendSpool refuses new
	// entries (ErrSpoolFull) and the caller sends directly instead.
	spoolCeiling int64 = 50 * 1024 * 1024
	// spoolMaxAge: spooled envelopes older than this are dropped on flush
	// rather than sent (matches the outbox and the default local retention).
	spoolMaxAge = 30 * 24 * time.Hour
	// SpoolLockStale: a flush lock older than this was left by a killed
	// flusher and is broken. Live flushers keep it fresh.
	SpoolLockStale = 2 * time.Minute
	// spoolDueBytes: a spool this large is due without counting its lines.
	spoolDueBytes int64 = 512 * 1024
)

var (
	// ErrSpoolDisabled: the event log is disabled, so nothing may be written
	// under ~/.promptconduit — the caller must send directly.
	ErrSpoolDisabled = errors.New("eventlog: disabled; upload spool unavailable")
	// ErrSpoolFull: the spool is past its ceiling (a long outage).
	ErrSpoolFull = errors.New("eventlog: upload spool is full")
)

// SpoolPath is ~/.promptconduit/spool-<target hash>.jsonl.
func SpoolPath(target string) string {
	return filepath.Join(Dir(), "spool-"+outboxKey(target)+".jsonl")
}

func spoolLockPath(target string) string {
	return filepath.Join(Dir(), ".spool-"+outboxKey(target)+".lock")
}

func spoolStatePath(target string) string {
	return filepath.Join(Dir(), ".spool-"+outboxKey(target)+".state.json")
}

// AppendSpool queues one envelope for a batched upload to target. Unlike the
// other event-log writers it reports failure, because a spooled event that
// didn't land would otherwise never be sent: the caller falls back to the
// direct single-event send.
func AppendSpool(target string, envJSON []byte) error {
	if !Enabled() {
		return ErrSpoolDisabled
	}
	envJSON = bytes.TrimRight(envJSON, "\n")
	if len(envJSON) == 0 {
		return errors.New("eventlog: empty envelope")
	}
	if info, err := os.Stat(SpoolPath(target)); err == nil && info.Size() >= spoolCeiling {
		return ErrSpoolFull
	}
	return appendLineErr(SpoolPath(target), envJSON, 0)
}

// SpoolCount returns how many envelopes are waiting in target's spool.
func SpoolCount(target string) int {
	total, _ := countExpired(SpoolPath(target), time.Time{}, envelopeCapturedAt)
	return total
}

// SpoolDue reports whether target's spool should be flushed now: it is not
// in a failure backoff, and it holds at least minCount envelopes or its
// oldest envelope is at least maxAge old. Cheap enough for the hook hot path:
// a stat, a tiny state read, and a scan of at most minCount line heads.
func SpoolDue(target string, minCount int, maxAge time.Duration) bool {
	info, err := os.Stat(SpoolPath(target))
	if err != nil || info.Size() == 0 {
		return false
	}
	if st := LoadSpoolState(target); st.NextAttemptAt.After(time.Now()) {
		return false
	}
	if info.Size() >= spoolDueBytes {
		return true
	}
	f, err := os.Open(SpoolPath(target))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	count := 0
	due := false
	_ = scanLines(f, func(head []byte, _ int64, terminated bool) bool {
		if !terminated || len(head) == 0 {
			return true
		}
		if count == 0 {
			if ts, ok := envelopeCapturedAt(head); ok && time.Since(ts) >= maxAge {
				due = true
				return false
			}
		}
		count++
		if count >= minCount {
			due = true
			return false
		}
		return true
	})
	return due
}

// SpoolInBackoff reports whether a recent failed flush asked flushers to wait.
func SpoolInBackoff(target string) bool {
	return LoadSpoolState(target).NextAttemptAt.After(time.Now())
}

// SpoolFlushing reports whether a live flusher currently holds target's lock
// (a lock older than SpoolLockStale is a dead flusher's and doesn't count).
func SpoolFlushing(target string) bool {
	info, err := os.Stat(spoolLockPath(target))
	return err == nil && time.Since(info.ModTime()) < SpoolLockStale
}

// SpoolFlushLimits bounds one flush pass.
type SpoolFlushLimits struct {
	MaxEvents  int           // envelopes per batch
	MaxBytes   int           // summed envelope bytes per batch; a larger lone envelope is sent alone
	MaxBatches int           // batches per pass (0 = no limit)
	Budget     time.Duration // wall-clock budget for the pass (0 = no limit)
}

// SpoolFlushResult summarizes one flush pass.
type SpoolFlushResult struct {
	Busy      bool // another flusher holds the lock; nothing was sent
	Batches   int  // send calls made
	Tried     int  // envelopes handed to send
	Delivered int  // accepted by the server
	Rejected  int  // permanently rejected, removed
	Skipped   int  // failed individually, kept (rotated to the back)
	Expired   int  // older than spoolMaxAge, dropped without sending
	Stopped   bool // a send reported a server-level failure; the pass ended early
	Remaining int  // envelopes still spooled afterwards
	// Err: rewriting the spool failed after the sends. Nothing was removed, so
	// delivered entries will be resent (the server dedupes on event_id).
	Err error
}

// FlushSpool makes one pass over target's spool under its cross-process
// flush lock, handing send batches of up to limits.MaxEvents envelopes /
// limits.MaxBytes bytes. send returns one ReplayResult per envelope, in
// order: Delivered and Rejected entries are removed, Skip entries are kept
// (rotated to the back so they can't occupy the head forever), and any Stop
// ends the pass after that batch with the Stop entries kept.
//
// The pass reads the spool sequentially and rewrites it ONCE at the end, so a
// large backlog costs one rewrite rather than one per batch. Entries appended
// while it runs are carried over untouched. Returns Busy without sending when
// another flusher holds the lock.
func FlushSpool(target string, limits SpoolFlushLimits, send func(batch [][]byte) []ReplayResult) SpoolFlushResult {
	var res SpoolFlushResult
	if limits.MaxEvents <= 0 {
		limits.MaxEvents = 1
	}
	path := SpoolPath(target)
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		return res
	}
	unlock, ok := tryLockFile(spoolLockPath(target), SpoolLockStale)
	if !ok {
		res.Busy = true
		res.Remaining = SpoolCount(target)
		return res
	}
	stopFresh := keepLockFresh(spoolLockPath(target), SpoolLockStale/5)
	defer func() {
		stopFresh()
		unlock()
	}()

	// drop maps a line ordinal to the hash of the line we decided to remove,
	// so the rewrite removes exactly the bytes we sent (or found expired).
	drop := map[int][sha256.Size]byte{}
	var skipped [][]byte
	deadline := time.Time{}
	if limits.Budget > 0 {
		deadline = time.Now().Add(limits.Budget)
	}
	cutoff := nowUTC().Add(-spoolMaxAge)

	f, err := os.Open(path)
	if err != nil {
		res.Err = err
		return res
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		res.Err = err
		return res
	}
	r := bufio.NewReaderSize(io.LimitReader(f, info.Size()), 1<<20)

	var batch [][]byte
	var batchIdx []int
	batchBytes := 0
	flushBatch := func() bool {
		if len(batch) == 0 {
			return true
		}
		res.Batches++
		res.Tried += len(batch)
		results := send(batch)
		for i, line := range batch {
			out := ReplayStop
			if i < len(results) {
				out = results[i]
			}
			switch out {
			case ReplayDelivered:
				res.Delivered++
				drop[batchIdx[i]] = sha256.Sum256(line)
			case ReplayRejected:
				res.Rejected++
				drop[batchIdx[i]] = sha256.Sum256(line)
			case ReplaySkip:
				res.Skipped++
				drop[batchIdx[i]] = sha256.Sum256(line)
				skipped = append(skipped, line)
			default:
				res.Stopped = true
			}
		}
		batch, batchIdx, batchBytes = nil, nil, 0
		if res.Stopped {
			return false
		}
		if limits.MaxBatches > 0 && res.Batches >= limits.MaxBatches {
			return false
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return false
		}
		return true
	}

	ordinal := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) == 0 || line[len(line)-1] != '\n' {
			// EOF, or a record still being written: leave it for next time.
			if readErr != nil && readErr != io.EOF {
				res.Err = readErr
			}
			break
		}
		idx := ordinal
		ordinal++
		body := line[:len(line)-1]
		if len(body) == 0 {
			continue
		}
		if ts, ok := envelopeCapturedAt(body); !ok || ts.Before(cutoff) {
			res.Expired++
			drop[idx] = sha256.Sum256(body)
			continue
		}
		if len(batch) > 0 && (len(batch) >= limits.MaxEvents || batchBytes+len(body) > limits.MaxBytes) {
			if !flushBatch() {
				break
			}
		}
		batch = append(batch, body)
		batchIdx = append(batchIdx, idx)
		batchBytes += len(body)
	}
	if !res.Stopped && res.Err == nil {
		flushBatch()
	}
	_ = f.Close()

	if len(drop) > 0 {
		ordinal := 0
		_, err := rewriteFile(path, func(line []byte) ([]byte, bool) {
			idx := ordinal
			ordinal++
			if want, ok := drop[idx]; ok && sha256.Sum256(line) == want {
				return nil, true
			}
			return nil, false
		})
		if err != nil {
			Errorf("spool flush: %v", err)
			res.Err = err
			res.Remaining = SpoolCount(target)
			return res
		}
		for _, line := range skipped {
			appendLine(path, line, 0)
		}
	}
	if res.Expired > 0 {
		n := int64(res.Expired)
		bumpCounter(func(s *Status) {
			s.Dropped += n
			s.LastErrorAt = nowUTC().Format(timeLayout)
			s.LastError = fmt.Sprintf("%d spooled event(s) expired before they could be sent", n)
		})
	}
	res.Remaining = SpoolCount(target)
	return res
}

// SpoolState is the flusher's memory between runs, persisted next to the
// spool: the last flush's outcome (shown by `promptconduit status`), the
// failure backoff, and whether the server lacks the batch endpoint.
type SpoolState struct {
	LastFlushAt         time.Time `json:"last_flush_at,omitempty"`
	LastResult          string    `json:"last_result,omitempty"` // "ok", "partial" or "error"
	LastError           string    `json:"last_error,omitempty"`
	LastDelivered       int       `json:"last_delivered,omitempty"`
	LastRejected        int       `json:"last_rejected,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
	// NextAttemptAt: automatic flushes wait until then after a failure.
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
	// BatchUnsupportedUntil: the server answered 404/405 for the batch
	// endpoint; use the single-event endpoint until then.
	BatchUnsupportedUntil time.Time `json:"batch_unsupported_until,omitempty"`
}

// LoadSpoolState returns target's flusher state (zero when absent). Safe to
// call regardless of Enabled().
func LoadSpoolState(target string) SpoolState {
	var st SpoolState
	data, err := os.ReadFile(spoolStatePath(target))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

// UpdateSpoolState applies fn to target's flusher state and saves it
// atomically (temp + rename). Callers are flushers, serialized by the flush
// lock; the worst cross-process race is a lost update, never a torn file.
func UpdateSpoolState(target string, fn func(*SpoolState)) {
	if !Enabled() {
		return
	}
	st := LoadSpoolState(target)
	fn(&st)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	path := spoolStatePath(target)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".spool-state-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		cleanupTemp(tmp, tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// BumpSent counts n envelopes accepted by the server in one batch.
func BumpSent(n int64) {
	if n <= 0 {
		return
	}
	bumpCounter(func(st *Status) {
		st.Sent += n
		st.LastSuccessAt = nowUTC().Format(timeLayout)
	})
}

// BumpFailed counts n envelopes that will never be delivered (permanently
// rejected), recording detail as the last error.
func BumpFailed(n int64, detail string) {
	if n <= 0 {
		return
	}
	bumpCounter(func(st *Status) {
		st.Failed += n
		st.LastErrorAt = nowUTC().Format(timeLayout)
		if detail != "" {
			st.LastError = detail
		}
	})
}

// NoteSendError records a transient upload failure as the last error without
// counting any event as failed: the envelopes are still spooled and will be
// retried, so "failed" keeps meaning "never reached the platform".
func NoteSendError(detail string) {
	bumpCounter(func(st *Status) {
		st.LastErrorAt = nowUTC().Format(timeLayout)
		st.LastError = detail
	})
}
