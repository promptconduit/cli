package sessions

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

	candidates := make([]ResolveCandidate, 0, len(pids))
	for _, pid := range pids {
		c := resolveClaudePID(ctx, pid)
		if c.SessionID != "" {
			candidates = append(candidates, c)
		}
	}
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
	if sessionID == "" {
		sessionID = transcriptSessionFromLsof(ctx, pid)
	}
	cwd := procCwd(ctx, pid)
	return ResolveCandidate{SessionID: sessionID, PID: pid, Cwd: cwd}
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
