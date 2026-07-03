package enrich

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/client"
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

func stateDir() string {
	if stateDirOverride != "" {
		return filepath.Join(stateDirOverride, stateSubdir)
	}
	return filepath.Join(client.ConfigDir(), stateSubdir)
}

func statePath(sessionID string) string {
	return filepath.Join(stateDir(), sessionID+".json")
}

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
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, statePath(sessionID)); err != nil {
		_ = os.Remove(tmpName)
	}
	maybeGCState(dir)
}

// maybeGCState deletes session state files untouched for stateMaxAge, on
// roughly 1 in gcOneInEvery calls (mirrors the correlation store's approach).
func maybeGCState(dir string) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return
	}
	if binary.BigEndian.Uint32(b[:])%gcOneInEvery != 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-stateMaxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
