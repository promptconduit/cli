package eventlog

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"time"
)

// The outbox holds envelopes whose send finally failed for a reason that may
// clear (5xx, 429, connection errors, timeouts). The detached sender replays
// it after a later send succeeds, so an outage longer than the in-process
// retry window no longer loses events. Each line is the exact envelope that
// was POSTed; replays reuse its event_id, which the server dedupes on.

const (
	// outboxCeiling caps the outbox on disk. Past it new failures aren't
	// queued (they remain in events.jsonl; `promptconduit sync` style
	// backfill is the escape hatch for a very long outage).
	outboxCeiling int64 = 50 * 1024 * 1024
	// outboxMaxAge: queued envelopes older than this are dropped on flush
	// rather than replayed (matches the default local retention).
	outboxMaxAge = 30 * 24 * time.Hour
	// outboxCooldown: at most one flush pass per this interval.
	outboxCooldown = time.Minute
	// outboxLockStale: a flush lock older than this was left by a killed
	// process and is broken.
	outboxLockStale = 5 * time.Minute
)

// OutboxPath is ~/.promptconduit/outbox.jsonl.
func OutboxPath() string { return filepath.Join(Dir(), "outbox.jsonl") }

func outboxLockPath() string  { return filepath.Join(Dir(), ".outbox.lock") }
func outboxStampPath() string { return filepath.Join(Dir(), ".outbox.stamp") }

// AppendOutbox queues an envelope for a later replay. Best-effort; a full
// outbox (past outboxCeiling) or a disabled event log queues nothing.
func AppendOutbox(envJSON []byte) {
	if !Enabled() || len(envJSON) == 0 {
		return
	}
	if info, err := os.Stat(OutboxPath()); err == nil && info.Size() >= outboxCeiling {
		Errorf("outbox full (%d bytes); not queueing event for replay", info.Size())
		return
	}
	appendLine(OutboxPath(), bytes.TrimRight(envJSON, "\n"), 0)
}

// OutboxCount returns how many envelopes are waiting to be replayed.
func OutboxCount() int {
	f, err := os.Open(OutboxPath())
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	n := 0
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
		if err != nil {
			return n
		}
	}
}

// ReplayResult is what a replay callback decided for one queued envelope.
type ReplayResult int

const (
	// ReplayDelivered: the server accepted it (or it's a duplicate); remove it.
	ReplayDelivered ReplayResult = iota
	// ReplayRejected: a permanent failure (4xx); remove it, it will never succeed.
	ReplayRejected
	// ReplayStop: a transient failure; keep it and stop this pass (the server
	// is struggling again — no point hammering it with the rest).
	ReplayStop
)

// FlushOutbox replays up to limit queued envelopes through send, at most once
// per outboxCooldown and never concurrently (cross-process lock). Delivered
// and rejected entries are removed; expired ones (older than outboxMaxAge) are
// dropped without sending. Returns how many were delivered. Safe to call on
// every successful send: it returns immediately when there is nothing to do.
func FlushOutbox(limit int, send func(envJSON []byte) ReplayResult) int {
	if !Enabled() {
		return 0
	}
	info, err := os.Stat(OutboxPath())
	if err != nil || info.Size() == 0 {
		return 0
	}
	if st, err := os.Stat(outboxStampPath()); err == nil && time.Since(st.ModTime()) < outboxCooldown {
		return 0
	}
	release, ok := tryLockFile(outboxLockPath(), outboxLockStale)
	if !ok {
		return 0
	}
	defer release()
	touchFile(outboxStampPath())

	delivered, tried, stopped := 0, 0, false
	cutoff := nowUTC().Add(-outboxMaxAge)
	_, err = rewriteFile(OutboxPath(), func(line []byte) ([]byte, bool) {
		if ts, ok := envelopeCapturedAt(line); ok && ts.Before(cutoff) {
			return nil, true // expired: drop without sending
		}
		if stopped || tried >= limit {
			return nil, false
		}
		tried++
		switch send(line) {
		case ReplayDelivered:
			delivered++
			return nil, true
		case ReplayRejected:
			return nil, true
		default:
			stopped = true
			return nil, false
		}
	})
	if err != nil {
		Errorf("outbox flush: %v", err)
	}
	if delivered > 0 {
		BumpReplayed(int64(delivered))
	}
	return delivered
}

// tryLockFile takes an O_EXCL lock file without blocking, breaking a lock
// older than stale (left by a killed process) once.
func tryLockFile(p string, stale time.Duration) (release func(), ok bool) {
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(p) }, true
		}
		if !os.IsExist(err) {
			return nil, false
		}
		info, err := os.Stat(p)
		if err != nil || time.Since(info.ModTime()) < stale {
			return nil, false
		}
		_ = os.Remove(p)
	}
	return nil, false
}

func touchFile(p string) {
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_ = f.Close()
		}
	}
}
