package eventlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
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
		// The send was already counted as failed; just say why it won't be
		// replayed, at most once an hour so a long outage can't flood the log.
		stamp := filepath.Join(Dir(), ".outbox-"+outboxKey(target)+".full")
		if st, err := os.Stat(stamp); err != nil || time.Since(st.ModTime()) > time.Hour {
			touchFile(stamp)
			Errorf("outbox full (%d bytes); failed events are not being queued for replay", info.Size())
		}
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
	if !Enabled() || outboxEmpty(target) {
		return 0
	}
	if st, err := os.Stat(outboxStampPath(target)); err == nil && time.Since(st.ModTime()) < outboxCooldown {
		return 0
	}
	release, ok := lockOutbox(target)
	if !ok {
		return 0
	}
	defer release()
	touchFile(outboxStampPath(target))
	st := flushPass(target, limit, send, nil)
	if st.Err != nil {
		return 0 // nothing was removed; the deliveries will be resent
	}
	return st.Delivered
}

// FlushStats summarizes one replay pass over an outbox.
type FlushStats struct {
	Tried     int  // envelopes handed to send
	Delivered int  // accepted by the server
	Rejected  int  // permanently rejected
	Skipped   int  // failed but kept, rotated to the back
	Expired   int  // older than the max age, dropped without sending
	Stopped   bool // a send returned ReplayStop; the pass ended early
	// Err: rewriting the outbox failed after the sends. The counters still say
	// what happened on the wire, but nothing was removed from the outbox, so
	// delivered entries will be resent (the server dedupes on event_id).
	Err error
}

// DrainResult summarizes a DrainOutbox call.
type DrainResult struct {
	FlushStats
	Busy      bool // another process holds the flush lock; nothing was sent
	Remaining int  // envelopes still queued afterwards
}

// DrainOutbox replays everything queued for target now, for an explicit
// "push it all" request (`promptconduit events flush`, the end of `sync`).
// Unlike FlushOutbox it ignores the per-target cooldown and has no batch
// limit: it makes ONE pass over the whole queue, so every envelope is tried
// at most once, failing ones (ReplaySkip) rotate to the back once, and the
// file is rewritten once. The pass ends early on ReplayStop. Entries appended
// while it runs are left for the next flush.
//
// It holds the same cross-process lock as FlushOutbox, so it never runs
// alongside a background flush; when that lock is taken it returns Busy
// without sending. It refreshes the cooldown stamp so background senders
// don't immediately re-walk what was just drained. onSend, if set, sees the
// running stats after each send.
func DrainOutbox(target string, send func(envJSON []byte) ReplayResult, onSend func(st FlushStats)) DrainResult {
	var res DrainResult
	if !Enabled() || outboxEmpty(target) {
		res.Remaining = OutboxCount(target)
		return res
	}
	release, ok := lockOutbox(target)
	if !ok {
		res.Busy = true
		res.Remaining = OutboxCount(target)
		return res
	}
	defer release()
	touchFile(outboxStampPath(target))

	res.FlushStats = flushPass(target, math.MaxInt, send, onSend)
	res.Remaining = OutboxCount(target)
	return res
}

func outboxEmpty(target string) bool {
	info, err := os.Stat(OutboxPath(target))
	return err != nil || info.Size() == 0
}

// lockOutbox takes target's flush lock and keeps it fresh until released.
func lockOutbox(target string) (release func(), ok bool) {
	unlock, ok := tryLockFile(outboxLockPath(target), outboxLockStale)
	if !ok {
		return nil, false
	}
	stopFresh := keepLockFresh(outboxLockPath(target), outboxLockStale/5)
	return func() {
		stopFresh()
		unlock()
	}, true
}

// flushPass makes one replay pass of up to limit envelopes. The caller holds
// the outbox lock. onSend, if set, sees the running stats after each send.
func flushPass(target string, limit int, send func(envJSON []byte) ReplayResult, onSend func(FlushStats)) FlushStats {
	path := OutboxPath(target)
	var st FlushStats
	var skipped [][]byte
	cutoff := nowUTC().Add(-outboxMaxAge)
	_, err := rewriteFile(path, func(line []byte) ([]byte, bool) {
		if ts, ok := envelopeCapturedAt(line); !ok || ts.Before(cutoff) {
			st.Expired++ // too old (or unreadable) to replay: drop, counted below
			return nil, true
		}
		if st.Stopped || st.Tried >= limit {
			return nil, false
		}
		st.Tried++
		var drop bool
		switch send(line) {
		case ReplayDelivered:
			st.Delivered++
			drop = true
		case ReplayRejected:
			st.Rejected++
			drop = true
		case ReplaySkip:
			// Rotate to the back so a run of failing envelopes can't keep
			// occupying the head of every pass.
			st.Skipped++
			skipped = append(skipped, append([]byte(nil), line...))
			drop = true
		default: // ReplayStop
			st.Stopped = true
		}
		if onSend != nil {
			onSend(st)
		}
		return nil, drop
	})
	if err != nil {
		// Nothing was removed (skipped entries included), so delivered entries
		// will be resent and the server dedupes them. Report what was sent but
		// don't bump replayed/dropped counters until the entries are gone.
		Errorf("outbox flush: %v", err)
		st.Err = err
		return st
	}
	for _, line := range skipped {
		appendLine(path, line, 0)
	}
	if st.Expired > 0 {
		n := int64(st.Expired)
		bumpCounter(func(s *Status) {
			s.Dropped += n
			s.LastErrorAt = nowUTC().Format(timeLayout)
			s.LastError = fmt.Sprintf("%d queued event(s) expired before they could be replayed", n)
		})
	}
	if st.Delivered > 0 {
		BumpReplayed(int64(st.Delivered))
	}
	return st
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
