package eventlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"
)

// The outbox holds envelopes whose send finally failed for a reason that may
// clear (no response, 429, 5xx). The detached sender replays it after a later
// send succeeds, so an outage longer than the in-process retry window no
// longer loses events. Each line is the exact envelope that was POSTed;
// replays reuse its event_id, which the server dedupes on.
//
// There is one outbox per API URL (target): events queued against prod are
// only ever replayed to prod, even after `config env use local`.

const (
	// outboxCeiling caps an outbox on disk. Past it new failures are counted
	// as dropped instead of queued (they remain in events.jsonl).
	outboxCeiling int64 = 50 * 1024 * 1024
	// outboxMaxAge: queued envelopes older than this are dropped on flush
	// rather than replayed (matches the default local retention).
	outboxMaxAge = 30 * 24 * time.Hour
	// outboxCooldown: at most one flush pass per this interval, per target.
	outboxCooldown = time.Minute
	// outboxLockStale: a flush lock older than this was left by a killed
	// process and is broken. Live passes keep it fresh.
	outboxLockStale = 5 * time.Minute
)

// outboxKey derives a stable short file key from the target API URL.
func outboxKey(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:])[:12]
}

// OutboxPath is ~/.promptconduit/outbox-<target hash>.jsonl.
func OutboxPath(target string) string {
	return filepath.Join(Dir(), "outbox-"+outboxKey(target)+".jsonl")
}

func outboxLockPath(target string) string {
	return filepath.Join(Dir(), ".outbox-"+outboxKey(target)+".lock")
}

func outboxStampPath(target string) string {
	return filepath.Join(Dir(), ".outbox-"+outboxKey(target)+".stamp")
}

// AppendOutbox queues an envelope for a later replay to target. Best-effort;
// with the outbox full (past outboxCeiling) the event is counted as dropped.
func AppendOutbox(target string, envJSON []byte) {
	if !Enabled() || len(envJSON) == 0 {
		return
	}
	path := OutboxPath(target)
	if info, err := os.Stat(path); err == nil && info.Size() >= outboxCeiling {
		Bump(OutcomeDropped, "outbox full; event not queued for replay")
		return
	}
	appendLine(path, bytes.TrimRight(envJSON, "\n"), 0)
}

// OutboxCount returns how many envelopes are waiting to be replayed to target.
func OutboxCount(target string) int {
	total, _ := countExpired(OutboxPath(target), time.Time{}, envelopeCapturedAt)
	return total
}

// ReplayResult is what a replay callback decided for one queued envelope.
type ReplayResult int

const (
	// ReplayDelivered: the server accepted it (or it's a duplicate); remove it.
	ReplayDelivered ReplayResult = iota
	// ReplayRejected: the server rejected this envelope permanently; remove it.
	ReplayRejected
	// ReplaySkip: this envelope failed but the server is up; keep it and move
	// on (so one bad envelope can't block everything queued behind it).
	ReplaySkip
	// ReplayStop: a server-level failure (no response, rate limited,
	// unavailable) or the pass is out of time; keep it and stop the pass.
	ReplayStop
)

// FlushOutbox replays up to limit envelopes queued for target through send,
// at most once per outboxCooldown per target and never concurrently
// (cross-process lock, kept fresh). Delivered and rejected entries are
// removed; expired ones (older than outboxMaxAge) are dropped without
// sending. Returns how many were delivered. Cheap to call on every successful
// send: it returns immediately when there is nothing to do.
func FlushOutbox(target string, limit int, send func(envJSON []byte) ReplayResult) int {
	if !Enabled() {
		return 0
	}
	path := OutboxPath(target)
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return 0
	}
	if st, err := os.Stat(outboxStampPath(target)); err == nil && time.Since(st.ModTime()) < outboxCooldown {
		return 0
	}
	release, ok := tryLockFile(outboxLockPath(target), outboxLockStale)
	if !ok {
		return 0
	}
	defer release()
	stopFresh := keepLockFresh(outboxLockPath(target), outboxLockStale/5)
	defer stopFresh()
	touchFile(outboxStampPath(target))

	delivered, tried, stopped := 0, 0, false
	cutoff := nowUTC().Add(-outboxMaxAge)
	_, err = rewriteFile(path, func(line []byte) ([]byte, bool) {
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
		case ReplayStop:
			stopped = true
		}
		return nil, false
	})
	if err != nil {
		// Nothing was removed, so the delivered entries will be resent (the
		// server dedupes them); don't count them until they're actually gone.
		Errorf("outbox flush: %v", err)
		return 0
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
