package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/filelock"
)

// Auto-sync scheduling.
//
// The hook triggers a transcript sync on every Stop. With several agents each
// stopping every few seconds that meant a sync subprocess per Stop, and two
// uploads of the same transcript could overlap. PlanAutoSync debounces per
// session with a TRAILING edge, so no content is ever left unsynced:
//
//   - at most one sync per session starts within AutoSyncInterval;
//   - a Stop that arrives while a sync is already scheduled to start later is
//     covered by that sync (it reads the transcript after this Stop) and spawns
//     nothing;
//   - a Stop inside the interval after a sync has started schedules exactly
//     one follow-up sync for when the interval ends.
//
// LockTranscript then guarantees that two sync processes never upload the
// same transcript at once. Neither changes what is uploaded or how.

// AutoSyncInterval is the minimum gap between auto-sync starts per session.
const AutoSyncInterval = 60 * time.Second

// autoSyncFlushDelay is the delay before an immediate sync, letting the tool
// finish flushing the transcript (the previous fixed `--delay 1`).
const autoSyncFlushDelay = 1 * time.Second

const (
	autoSyncSubdir      = "autosync"
	autoSyncPlanWait    = 200 * time.Millisecond
	autoSyncStampMaxAge = 7 * 24 * time.Hour
)

// PlanAutoSync decides whether the hook should start a sync for sessionID at
// now. spawn=false means an already-scheduled sync will cover this event;
// otherwise the caller spawns `sync --file … --delay <delaySeconds>`. State
// lives under baseDir/autosync. Best-effort: on any failure it falls back to
// the old behaviour (spawn now with the flush delay).
func PlanAutoSync(baseDir, sessionID string, now time.Time) (delaySeconds int, spawn bool, undo func()) {
	fallback := int(autoSyncFlushDelay / time.Second)
	undo = func() {}
	if baseDir == "" || sessionID == "" {
		return fallback, true, undo
	}
	dir := filepath.Join(baseDir, autoSyncSubdir)
	stamp := filepath.Join(dir, safeName(sessionID)+".next")

	release, ok := filelock.Exclusive(stamp+".lock", autoSyncPlanWait)
	defer release()
	if !ok {
		return fallback, true, undo
	}

	start := now.Add(autoSyncFlushDelay)
	prev, prevErr := os.ReadFile(stamp)
	if data, err := prev, prevErr; err == nil {
		if last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data))); err == nil {
			switch {
			case coversStop(last, now):
				// A scheduled sync starts at least the flush delay after this
				// event, so it will read this event's flushed transcript.
				return 0, false, undo
			case last.After(now):
				// Scheduled, but too soon for this event's transcript to have
				// flushed: schedule one more, a full flush delay out.
			case now.Before(last.Add(AutoSyncInterval)):
				start = last.Add(AutoSyncInterval) // trailing follow-up
				if earliest := now.Add(autoSyncFlushDelay); start.Before(earliest) {
					start = earliest
				}
			}
		}
	}
	written := []byte(start.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(stamp, written, 0o600); err != nil {
		return fallback, true, undo
	}
	maybeSweepAutoSync(dir, now)
	if prevErr != nil {
		prev = nil
	}
	undo = func() { unplanAutoSync(stamp, written, prev) }
	return int(math.Ceil(start.Sub(now).Seconds())), true, undo
}

// unplanAutoSync rolls back a schedule stamp whose sync never started (the
// caller failed to spawn it), so later Stops aren't suppressed waiting for a
// sync that will never run: the previous stamp is restored (or the stamp
// removed if there was none). Only acts if the stamp is still the one this
// plan wrote; a newer plan from another hook is left alone.
func unplanAutoSync(stamp string, written, prev []byte) {
	release, ok := filelock.Exclusive(stamp+".lock", autoSyncPlanWait)
	defer release()
	if !ok {
		return
	}
	if cur, err := os.ReadFile(stamp); err == nil && string(cur) == string(written) {
		if prev == nil {
			_ = os.Remove(stamp)
		} else {
			_ = os.WriteFile(stamp, prev, 0o600)
		}
	}
}

// AutoSyncPending is a lock-free peek: true when a sync for sessionID is
// already scheduled to start at least the flush delay after now, so the hook can skip even locating the
// transcript. PlanAutoSync makes the authoritative decision.
func AutoSyncPending(baseDir, sessionID string, now time.Time) bool {
	if baseDir == "" || sessionID == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(baseDir, autoSyncSubdir, safeName(sessionID)+".next"))
	if err != nil {
		return false
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	return err == nil && coversStop(last, now)
}

// coversStop reports whether a sync scheduled to start at scheduled will see
// the transcript as of a Stop at now: it must start at least
// autoSyncFlushDelay later, giving the tool time to flush.
func coversStop(scheduled, now time.Time) bool {
	return !scheduled.Before(now.Add(autoSyncFlushDelay))
}

// LockTranscript takes the per-transcript upload lock, waiting up to wait for
// another sync of the same file to finish. release is always safe to call.
func LockTranscript(baseDir, transcriptPath string, wait time.Duration) (release func(), ok bool) {
	sum := sha256.Sum256([]byte(transcriptPath))
	p := filepath.Join(baseDir, autoSyncSubdir, "locks", hex.EncodeToString(sum[:8])+".lock")
	return filelock.Exclusive(p, wait)
}

// safeName maps a session id to a file-name-safe token.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 || strings.Trim(b.String(), ".") == "" {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:8])
	}
	return b.String()
}

// maybeSweepAutoSync drops week-old stamps roughly once a day per directory
// (tracked by the directory's own sweep marker).
func maybeSweepAutoSync(dir string, now time.Time) {
	marker := filepath.Join(dir, ".swept")
	if info, err := os.Stat(marker); err == nil && now.Sub(info.ModTime()) < 24*time.Hour {
		return
	}
	_ = os.WriteFile(marker, nil, 0o600)
	sweepOld(dir, now)
	sweepOld(filepath.Join(dir, "locks"), now)
}

// sweepOld removes files in dir older than autoSyncStampMaxAge. Lock files
// are special: their mtime never changes while in use, so one is removed only
// if it can be locked without waiting (nobody holds it).
func sweepOld(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == ".swept" {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= autoSyncStampMaxAge {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if strings.HasSuffix(p, ".lock") {
			filelock.RemoveIfUnlocked(p)
			continue
		}
		_ = os.Remove(p)
	}
}
