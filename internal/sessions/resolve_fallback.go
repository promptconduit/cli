package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/eventlog"
)

// fallbackRecentWindow bounds which transcripts and SessionStart events count as
// plausibly active when cwd-based resolution can't see an open transcript fd.
const fallbackRecentWindow = 24 * time.Hour

// claudeSessionsDir is where Claude Code stores per-pid session metadata
// (~/.claude/sessions/<pid>.json). Returns "" when home can't be determined.
func claudeSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions")
}

// claudeSessionFile is the subset of ~/.claude/sessions/<pid>.json we read.
type claudeSessionFile struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// encodeProjectPath mirrors Claude Code's project-folder naming: every
// non-alphanumeric character in the absolute cwd becomes a dash.
func encodeProjectPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// projectDirForCwd resolves ~/.claude/projects/<encoded-cwd>, falling back to
// a folder whose newest transcript records the given cwd when the encoded name
// doesn't exist yet.
func projectDirForCwd(projectsRoot, cwd string) string {
	if projectsRoot == "" || cwd == "" {
		return ""
	}
	encoded := filepath.Join(projectsRoot, encodeProjectPath(cwd))
	if fi, err := os.Stat(encoded); err == nil && fi.IsDir() {
		return encoded
	}
	return scanProjectDirForCwd(projectsRoot, cwd)
}

// scanProjectDirForCwd walks project folders and returns the one whose newest
// transcript records the given cwd.
func scanProjectDirForCwd(projectsRoot, cwd string) string {
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		return ""
	}
	cwd = filepath.Clean(cwd)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(projectsRoot, e.Name())
		for _, path := range listRecentTranscripts(dir, time.Time{}) {
			if launchCwdFromTranscript(path) == cwd {
				return dir
			}
		}
	}
	return ""
}

type transcriptFile struct {
	path string
	mod  time.Time
}

// listRecentTranscripts returns .jsonl files in dir with mtime at or after
// cutoff, sorted newest-first. A zero cutoff includes every transcript.
func listRecentTranscripts(dir string, cutoff time.Time) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []transcriptFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mod := info.ModTime()
		if !cutoff.IsZero() && mod.Before(cutoff) {
			continue
		}
		files = append(files, transcriptFile{filepath.Join(dir, e.Name()), mod})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].mod.After(files[j].mod)
	})
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

// sessionIDFromTranscriptPath returns the session id from <session-id>.jsonl.
func sessionIDFromTranscriptPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// recentTranscriptSessionIDs lists session ids for transcripts in the project
// folder for cwd that were modified within the recent window.
func recentTranscriptSessionIDs(projectsRoot, cwd string, now time.Time) []string {
	dir := projectDirForCwd(projectsRoot, cwd)
	if dir == "" {
		return nil
	}
	cutoff := now.Add(-fallbackRecentWindow)
	var ids []string
	seen := map[string]bool{}
	for _, path := range listRecentTranscripts(dir, cutoff) {
		id := sessionIDFromTranscriptPath(path)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// sessionStartEnvelope is the minimal events.jsonl slice for SessionStart lookup.
type sessionStartEnvelope struct {
	Schema     int    `json:"schema"`
	Tool       string `json:"tool"`
	HookEvent  string `json:"hook_event"`
	CapturedAt string `json:"captured_at"`
	SessionID  string `json:"session_id"`
	Raw        struct {
		Cwd string `json:"cwd"`
	} `json:"raw_event"`
}

// recentSessionStartsFromEvents returns session ids from SessionStart events in
// the event log tail whose cwd matches, newest captured_at first.
func recentSessionStartsFromEvents(eventsPath, cwd string, now time.Time) ([]string, error) {
	lines, err := tailLines(eventsPath, defaultMaxBytes)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-fallbackRecentWindow)
	cwd = filepath.Clean(cwd)

	type hit struct {
		id string
		at time.Time
	}
	var hits []hit
	seen := map[string]bool{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e sessionStartEnvelope
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.Schema < 2 || e.Tool != "claude-code" || e.HookEvent != "SessionStart" {
			continue
		}
		if e.SessionID == "" || filepath.Clean(e.Raw.Cwd) != cwd {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.CapturedAt)
		if err != nil || t.Before(cutoff) {
			continue
		}
		if seen[e.SessionID] {
			continue
		}
		seen[e.SessionID] = true
		hits = append(hits, hit{e.SessionID, t})
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].at.After(hits[j].at)
	})
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids, nil
}

// sessionFromPidFile reads ~/.claude/sessions/<pid>.json for a session id.
func sessionFromPidFile(sessionsRoot, pid string) string {
	if sessionsRoot == "" || pid == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(sessionsRoot, pid+".json"))
	if err != nil {
		return ""
	}
	var rec claudeSessionFile
	if err := json.Unmarshal(data, &rec); err != nil || rec.SessionID == "" {
		return ""
	}
	return rec.SessionID
}

// resolveFallbackCandidates applies cwd-based fallbacks when --resume and lsof
// did not resolve a session. Priority: Claude pid file (C), recent transcripts
// in the project folder (A), then SessionStart rows in events.jsonl (B). When
// multiple candidates remain and the pid file can't disambiguate, every option
// is returned so the caller can set Ambiguous.
func resolveFallbackCandidates(pid, cwd string, now time.Time) []ResolveCandidate {
	cwd = filepath.Clean(cwd)
	if cwd == "" {
		return nil
	}

	projectsRoot := claudeProjectsDir()
	sessionsRoot := claudeSessionsDir()
	eventsPath := eventlog.EventsJSONLPath()

	// C: pid-scoped Claude state is the most reliable per-process mapping.
	if sid := sessionFromPidFile(sessionsRoot, pid); sid != "" {
		return []ResolveCandidate{{SessionID: sid, PID: pid, Cwd: cwd}}
	}

	// A: cwd + recently written transcripts in the encoded project folder.
	if ids := recentTranscriptSessionIDs(projectsRoot, cwd, now); len(ids) == 1 {
		return []ResolveCandidate{{SessionID: ids[0], PID: pid, Cwd: cwd}}
	} else if len(ids) > 1 {
		return candidatesFromIDs(ids, pid, cwd)
	}

	// B: SessionStart + cwd in the local event log.
	ids, err := recentSessionStartsFromEvents(eventsPath, cwd, now)
	if err != nil || len(ids) == 0 {
		return nil
	}
	if len(ids) == 1 {
		return []ResolveCandidate{{SessionID: ids[0], PID: pid, Cwd: cwd}}
	}
	return candidatesFromIDs(ids, pid, cwd)
}

func candidatesFromIDs(ids []string, pid, cwd string) []ResolveCandidate {
	out := make([]ResolveCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, ResolveCandidate{SessionID: id, PID: pid, Cwd: cwd})
	}
	return out
}

// dedupeCandidates collapses duplicate session ids, keeping the first pid seen.
func dedupeCandidates(in []ResolveCandidate) []ResolveCandidate {
	seen := map[string]bool{}
	out := make([]ResolveCandidate, 0, len(in))
	for _, c := range in {
		if c.SessionID == "" || seen[c.SessionID] {
			continue
		}
		seen[c.SessionID] = true
		out = append(out, c)
	}
	return out
}
