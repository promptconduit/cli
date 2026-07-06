package sessions

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/eventlog"
)

// ResolveCandidate is one claude process that could own the terminal session.
type ResolveCandidate struct {
	SessionID string `json:"session_id"`
	PID       string `json:"pid,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// ResolveResult maps a terminal shell pid to a Claude Code session when possible.
// When several claude children exist under the shell, Ambiguous is true and
// Candidates carries each option for the caller to disambiguate.
type ResolveResult struct {
	SessionID  string             `json:"session_id,omitempty"`
	Tool       string             `json:"tool,omitempty"`
	Cwd        string             `json:"cwd,omitempty"`
	Ambiguous  bool               `json:"ambiguous,omitempty"`
	Candidates []ResolveCandidate `json:"candidates,omitempty"`
}

// ResolveFromShellPID finds claude processes descended from shellPID and resolves
// each to a session id (--resume in argv, else an open transcript via lsof).
// macOS/Linux only; returns an empty result on other platforms or when nothing
// matches.
func ResolveFromShellPID(shellPID string) ResolveResult {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return ResolveResult{}
	}
	shellPID = strings.TrimSpace(shellPID)
	if shellPID == "" {
		return ResolveResult{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveProbeTimeout)
	defer cancel()

	psOut, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return ResolveResult{}
	}
	parents, comms := parseProcessTree(string(psOut))
	pids := claudeDescendants(shellPID, parents, comms)
	if len(pids) == 0 {
		return ResolveResult{}
	}

	if amb := sameCwdAmbiguousResult(ctx, pids); amb.Ambiguous {
		return amb
	}

	candidates := make([]ResolveCandidate, 0, len(pids))
	for _, pid := range pids {
		c := resolveClaudePID(ctx, pid)
		if c.SessionID != "" {
			candidates = append(candidates, c)
		}
	}
	candidates = dedupeCandidates(candidates)
	if len(candidates) == 0 {
		return ResolveResult{}
	}
	if len(candidates) == 1 {
		c := candidates[0]
		return ResolveResult{
			SessionID: c.SessionID,
			Tool:      "claude-code",
			Cwd:       c.Cwd,
		}
	}
	return ResolveResult{
		Ambiguous:  true,
		Candidates: candidates,
	}
}

// parseProcessTree reads `ps -axo pid=,ppid=,comm=` into parent and comm maps.
func parseProcessTree(psOutput string) (parents map[string]string, comms map[string]string) {
	parents = map[string]string{}
	comms = map[string]string{}
	for _, line := range strings.Split(psOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		ppid, comm, ok := strings.Cut(strings.TrimSpace(rest), " ")
		if !ok {
			continue
		}
		pid = strings.TrimSpace(pid)
		ppid = strings.TrimSpace(ppid)
		comms[pid] = strings.TrimSpace(comm)
		parents[pid] = ppid
	}
	return parents, comms
}

// claudeDescendants returns claude pids whose parent chain includes shellPID.
func claudeDescendants(shellPID string, parents, comms map[string]string) []string {
	var pids []string
	for pid, comm := range comms {
		if filepath.Base(comm) != "claude" {
			continue
		}
		if isDescendantOf(pid, shellPID, parents) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func isDescendantOf(pid, ancestor string, parents map[string]string) bool {
	for {
		ppid, ok := parents[pid]
		if !ok || ppid == "" || ppid == "0" || ppid == "1" {
			return false
		}
		if ppid == ancestor {
			return true
		}
		pid = ppid
	}
}

func resolveClaudePID(ctx context.Context, pid string) ResolveCandidate {
	args := procArgs(ctx, pid)
	sessionID := parseResumeSessionID(args)
	cwd := procCwd(ctx, pid)
	if sessionID == "" {
		sessionID = transcriptSessionFromLsof(ctx, pid)
	}
	if sessionID == "" && cwd != "" {
		sessionID = transcriptSessionFromCwd(cwd)
	}
	if sessionID == "" && cwd != "" {
		sessionID = sessionFromEventLog(cwd, eventlog.EventsJSONLPath())
	}
	return ResolveCandidate{SessionID: sessionID, PID: pid, Cwd: cwd}
}

// dedupeCandidates keeps one entry per session_id (first pid wins).
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

// sameCwdAmbiguousResult fires when multiple claude children share a cwd and
// more than one session could own that terminal — plain `claude` without
// --resume cannot be attributed to a single transcript.
func sameCwdAmbiguousResult(ctx context.Context, pids []string) ResolveResult {
	byCwd := map[string][]string{}
	for _, pid := range pids {
		cwd := procCwd(ctx, pid)
		if cwd == "" {
			continue
		}
		byCwd[cwd] = append(byCwd[cwd], pid)
	}
	for cwd, cpids := range byCwd {
		if len(cpids) < 2 {
			continue
		}
		cands := transcriptCandidatesForCwd(cwd, recentTranscriptWindow)
		if len(cands) == 0 {
			cands = eventLogCandidatesForCwd(cwd, eventlog.EventsJSONLPath(), recentTranscriptWindow)
		}
		for i := range cands {
			cands[i].Cwd = cwd
			if cands[i].PID == "" && len(cpids) > 0 {
				cands[i].PID = cpids[0]
			}
		}
		if len(cands) > 1 {
			return ResolveResult{Ambiguous: true, Candidates: cands}
		}
		if len(cands) == 1 {
			// One transcript but two processes — still ambiguous (cannot pick).
			return ResolveResult{Ambiguous: true, Candidates: cands}
		}
	}
	return ResolveResult{}
}

// recentTranscriptWindow bounds which on-disk transcripts count as live when
// disambiguating same-cwd plain-claude terminals.
const recentTranscriptWindow = 2 * time.Hour

// encodeProjectPath mirrors Claude Code's project-folder naming (see cost package).
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

func projectDirForCwd(cwd string) string {
	root := claudeProjectsDir()
	if root == "" || cwd == "" {
		return ""
	}
	encoded := filepath.Join(root, encodeProjectPath(cwd))
	if fi, err := os.Stat(encoded); err == nil && fi.IsDir() {
		return encoded
	}
	return encoded
}

type transcriptEntry struct {
	sessionID string
	mod       time.Time
}

// transcriptSessionsInDir returns session ids from .jsonl files in dir, newest
// mtime first. Only files modified within within are included.
func transcriptSessionsInDir(dir string, within time.Duration) []transcriptEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-within)
	var out []transcriptEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if id == "" {
			continue
		}
		out = append(out, transcriptEntry{sessionID: id, mod: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].mod.After(out[j].mod)
	})
	return out
}

// transcriptSessionFromCwd picks the newest recently-modified transcript in the
// Claude Code project folder for cwd.
func transcriptSessionFromCwd(cwd string) string {
	dir := projectDirForCwd(cwd)
	if dir == "" {
		return ""
	}
	entries := transcriptSessionsInDir(dir, recentTranscriptWindow)
	if len(entries) == 0 {
		return ""
	}
	return entries[0].sessionID
}

// transcriptCandidatesForCwd lists recent transcript session ids for cwd.
func transcriptCandidatesForCwd(cwd string, within time.Duration) []ResolveCandidate {
	dir := projectDirForCwd(cwd)
	if dir == "" {
		return nil
	}
	entries := transcriptSessionsInDir(dir, within)
	out := make([]ResolveCandidate, 0, len(entries))
	for _, e := range entries {
		out = append(out, ResolveCandidate{SessionID: e.sessionID, Cwd: cwd})
	}
	return out
}

// resolveEnvelope is the minimal events.jsonl line for cwd correlation.
type resolveEnvelope struct {
	Schema    int    `json:"schema"`
	Tool      string `json:"tool"`
	SessionID string `json:"session_id"`
	Raw       struct {
		Cwd string `json:"cwd"`
	} `json:"raw_event"`
}

// sessionFromEventLog returns the most recent claude-code session_id seen at cwd.
func sessionFromEventLog(cwd, path string) string {
	cands := eventLogCandidatesForCwd(cwd, path, recentTranscriptWindow)
	if len(cands) == 0 {
		return ""
	}
	return cands[0].SessionID
}

// eventLogCandidatesForCwd scans the event log tail for distinct session ids at
// cwd, newest activity first.
func eventLogCandidatesForCwd(cwd, path string, within time.Duration) []ResolveCandidate {
	lines, err := tailLines(path, defaultMaxBytes)
	if err != nil || len(lines) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-within)
	seen := map[string]bool{}
	var out []ResolveCandidate
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e resolveEnvelope
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.Schema < 2 || e.Tool != "claude-code" || e.SessionID == "" {
			continue
		}
		if e.Raw.Cwd != cwd {
			continue
		}
		if seen[e.SessionID] {
			continue
		}
		seen[e.SessionID] = true
		_ = cutoff // tail is bounded; recency is implied by reverse scan order
		out = append(out, ResolveCandidate{SessionID: e.SessionID, Cwd: cwd})
	}
	return out
}

func procArgs(ctx context.Context, pid string) string {
	out, err := exec.CommandContext(ctx, "ps", "-p", pid, "-o", "args=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseResumeSessionID extracts the session id from a claude argv string.
func parseResumeSessionID(args string) string {
	fields := strings.Fields(args)
	for i, f := range fields {
		if f == "--resume" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// transcriptSessionFromLsof finds an open Claude Code transcript for pid and
// returns the session id from its filename (<session-id>.jsonl).
func transcriptSessionFromLsof(ctx context.Context, pid string) string {
	out, err := exec.CommandContext(ctx, "lsof", "-Fn", "-p", pid).Output()
	if err != nil {
		return ""
	}
	return parseTranscriptSessionID(string(out))
}

// parseTranscriptSessionID scans lsof -Fn output for a .jsonl path under
// ~/.claude/projects and returns the session id from the basename.
func parseTranscriptSessionID(lsofOutput string) string {
	projects := claudeProjectsDir()
	if projects == "" {
		return ""
	}
	projects = filepath.Clean(projects)
	for _, line := range strings.Split(lsofOutput, "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		p := strings.TrimSpace(line[1:])
		if !strings.HasSuffix(p, ".jsonl") {
			continue
		}
		if !strings.Contains(p, projects) {
			continue
		}
		base := filepath.Base(p)
		id := strings.TrimSuffix(base, ".jsonl")
		if id != "" {
			return id
		}
	}
	return ""
}
