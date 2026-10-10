package enrich

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/filelock"
)

// sessionState is the tiny per-session scratchpad some enrichers need across
// hook fires: the running prompt count, how far into the transcript the cost
// enricher has already priced, and the currently-open subagents (so
// SubagentStop can recover the type and duration that only SubagentStart
// carries). One JSON file per session under
// ~/.config/promptconduit/enrich/sessions/, GC'd by mtime like the
// correlation store.
type sessionState struct {
	PromptCount      int                     `json:"prompt_count,omitempty"`
	TranscriptOffset int64                   `json:"transcript_offset,omitempty"`
	Subagents        map[string]subagentInfo `json:"subagents,omitempty"`
	// TurnStartedAt is the RFC3339 time of the last UserPromptSubmit; non-empty
	// means a turn is OPEN (no Stop yet). The prompt enricher sets it (and reads
	// it first for is_interrupt); the turn enricher consumes it at Stop.
	TurnStartedAt string `json:"turn_started_at,omitempty"`
}

// subagentInfo is what SubagentStart records for the matching SubagentStop.
type subagentInfo struct {
	Type      string `json:"type,omitempty"`
	StartedAt string `json:"started_at"`
}

const (
	stateSubdir  = "enrich/sessions"
	stateMaxAge  = 30 * 24 * time.Hour
	gcOneInEvery = 100
)

// stateDirOverride lets tests redirect state writes. "" = real config dir.
var stateDirOverride string

// SetStateDirForTest overrides the enrich state directory. Test-only.
func SetStateDirForTest(dir string) { stateDirOverride = dir }

// enrichBaseDir is the root every enrich state/cache path hangs off: the
// config dir, or the test override.
func enrichBaseDir() string {
	if stateDirOverride != "" {
		return stateDirOverride
	}
	return client.ConfigDir()
}

func stateDir() string {
	return filepath.Join(enrichBaseDir(), stateSubdir)
}

func statePath(sessionID string) string {
	return filepath.Join(stateDir(), sessionID+".json")
}

// stateLockWait bounds how long a hook waits for another process (or an
// abandoned timed-out enricher goroutine) to finish its state update. On
// timeout the update is SKIPPED, never written unlocked. A var so tests can
// shorten it.
var stateLockWait = 1 * time.Second

func stateLockPath(sessionID string) string { return statePath(sessionID) + ".lock" }

// updateState is the only way enrichers modify per-session state: it takes an
// exclusive cross-process lock on the session, loads the state, runs fn, and
// (when fn returns true) saves atomically before unlocking. Concurrent hook
// processes for the same session, and enricher goroutines Run abandoned after
// a timeout, therefore can't clobber each other's changes. Returns false
// without running fn when sessionID is empty or the lock wasn't acquired
// within stateLockWait; callers then degrade (read-only, or omit the slug).
func updateState(sessionID string, fn func(st *sessionState) (save bool)) bool {
	if sessionID == "" {
		return false
	}
	release, ok := filelock.Exclusive(stateLockPath(sessionID), stateLockWait)
	defer release()
	if !ok {
		return false
	}
	st := loadState(sessionID)
	if fn(&st) {
		saveState(sessionID, st)
	}
	return true
}

// sessionStateUser marks an enricher that read-modify-writes the per-session
// state file (via updateState). Correctness comes from updateState's lock;
// Run additionally executes these enrichers sequentially, in registration
// order, on one goroutine so they don't contend for that lock within one hook.
// Every other enricher runs concurrently.
//
// Any new enricher that touches session state MUST use updateState and
// implement this.
type sessionStateUser interface {
	usesSessionState()
}

func (promptEnricher) usesSessionState()   {}
func (costEnricher) usesSessionState()     {}
func (subagentEnricher) usesSessionState() {}
func (turnEnricher) usesSessionState()     {}

// loadState returns the session's state (zero value when absent/corrupt).
func loadState(sessionID string) sessionState {
	var st sessionState
	if sessionID == "" {
		return st
	}
	data, err := os.ReadFile(statePath(sessionID))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

// saveState persists the session's state atomically (temp file + rename).
// Best-effort; occasionally runs a probabilistic GC of stale session files.
func saveState(sessionID string, st sessionState) {
	if sessionID == "" {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	dir := stateDir()
	if writeFileAtomic(dir, statePath(sessionID), data) {
		maybeGC(dir, stateMaxAge)
	}
}

// writeFileAtomic writes data to path (inside dir, created if needed) via a
// temp file + rename, so concurrent hook processes never see a torn file.
// Reports whether the write landed.
func writeFileAtomic(dir, path string, data []byte) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return false
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return false
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return false
	}
	return true
}

// maybeGC deletes files in dir untouched for maxAge, on roughly 1 in
// gcOneInEvery calls (mirrors the correlation store's approach).
func maybeGC(dir string, maxAge time.Duration) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return
	}
	if binary.BigEndian.Uint32(b[:])%gcOneInEvery != 0 {
		return
	}
	gcDir(dir, maxAge)
}

// gcDir deletes files in dir untouched for maxAge (held lock files excepted).
func gcDir(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			p := filepath.Join(dir, e.Name())
			if strings.HasSuffix(p, ".lock") {
				// A lock file's mtime never changes while it's in use; only
				// delete it if nobody holds it right now.
				filelock.RemoveIfUnlocked(p)
				continue
			}
			_ = os.Remove(p)
		}
	}
}
