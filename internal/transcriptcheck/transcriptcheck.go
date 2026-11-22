// Package transcriptcheck notices Claude Code sessions that run without a
// transcript file.
//
// Claude Code writes no transcript .jsonl when a session inherits
// CLAUDE_CODE_CHILD_SESSION (its status bar says "Transcript saving is off").
// Hooks still fire, but everything that reads the transcript (session cost,
// cost history, transcript sync, the cost enrichment) finds nothing, so that
// session's spend goes missing without any error. This package records such
// sessions so `promptconduit status` and the setup_status MCP tool can say so
// and explain the fix, instead of leaving the gap silent.
//
// State is one small JSON file per session under
// <configDir>/transcript-missing/, so concurrent hooks never contend on a
// shared file: Observe writes or removes only its own session's file.
package transcriptcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Fix is the remedy shown to the user. Claude Code honors this variable even
// when the session would otherwise inherit transcript-less child mode.
const Fix = "restart the session with CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1"

// MaxAge is how long a record is kept. Older records are pruned on List: the
// warning is about recent sessions, not a permanent history.
const MaxAge = 7 * 24 * time.Hour

const subdir = "transcript-missing"

// Entry is one session seen running without its transcript.
type Entry struct {
	SessionID string `json:"session_id"`
	// TranscriptPath is the path the hook reported ("" when it reported none).
	TranscriptPath string    `json:"transcript_path,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	FirstSeen      time.Time `json:"first_seen"`
}

// Message is the human-readable warning for this session.
func (e Entry) Message() string {
	return Message(e.SessionID)
}

// Message is the warning text for a session with no transcript.
func Message(sessionID string) string {
	return "transcript saving is off for session " + sessionID +
		", so its cost is not counted; " + Fix
}

// Missing reports whether the transcript at path is absent. An empty path
// counts as missing: Claude Code always reports one when it saves a transcript.
// Other stat errors (permissions, I/O) are not treated as missing, so a
// transient failure never raises a false warning.
func Missing(path string) bool {
	if path == "" {
		return true
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// Observe checks a Claude Code Stop event's transcript and records or clears
// the session accordingly. missing reports whether the transcript is absent;
// first is true only on the Stop that first recorded the session, so callers
// can log once rather than on every turn. Best-effort: state write failures
// are ignored (the hook must never fail).
func Observe(baseDir, sessionID, transcriptPath, cwd string, now time.Time) (missing, first bool) {
	if sessionID == "" {
		return false, false
	}
	missing = Missing(transcriptPath)
	if baseDir == "" {
		return missing, false
	}
	p := entryPath(baseDir, sessionID)
	if !missing {
		_ = os.Remove(p) // transcript showed up (or was always there)
		return false, false
	}
	if _, err := os.Stat(p); err == nil {
		return true, false // already recorded; keep the original FirstSeen
	}
	data, err := json.Marshal(Entry{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		Cwd:            cwd,
		FirstSeen:      now.UTC(),
	})
	if err != nil {
		return true, false
	}
	return true, writeAtomic(p, data) == nil
}

// List returns the recorded sessions, newest first. Records older than MaxAge,
// and records whose transcript has since appeared, are removed and skipped.
func List(baseDir string, now time.Time) []Entry {
	if baseDir == "" {
		return nil
	}
	dir := filepath.Join(baseDir, subdir)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, f.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(data, &e) != nil || e.SessionID == "" {
			_ = os.Remove(p)
			continue
		}
		if now.Sub(e.FirstSeen) > MaxAge || (e.TranscriptPath != "" && !Missing(e.TranscriptPath)) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.After(out[j].FirstSeen) })
	return out
}

// entryPath maps a session id to its record file. Session ids are UUIDs in
// practice; anything that isn't a safe file name is hashed so a hostile id can
// never escape the directory.
func entryPath(baseDir, sessionID string) string {
	name := sessionID
	if !safeName(name) {
		sum := sha256.Sum256([]byte(sessionID))
		name = hex.EncodeToString(sum[:16])
	}
	return filepath.Join(baseDir, subdir, name+".json")
}

func safeName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		ok := r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
