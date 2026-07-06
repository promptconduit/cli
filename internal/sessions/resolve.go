package sessions

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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
// each to a session id (--resume in argv, else an open transcript via lsof, else
// cwd/transcript/event-log fallbacks for plain `claude`). macOS/Linux only;
// returns an empty result on other platforms or when nothing matches.
func ResolveFromShellPID(shellPID string) ResolveResult {
	return resolveFromShellPID(shellPID, resolveProbe{})
}

type resolveProbe struct {
	ps       func(ctx context.Context) (string, error)
	procArgs func(ctx context.Context, pid string) (string, error)
	lsof     func(ctx context.Context, pid string) (string, error)
	procCwd  func(ctx context.Context, pid string) (string, error)
	now      func() time.Time
}

func defaultResolveProbe() resolveProbe {
	return resolveProbe{
		ps: func(ctx context.Context) (string, error) {
			out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,comm=").Output()
			return string(out), err
		},
		procArgs: func(ctx context.Context, pid string) (string, error) {
			out, err := exec.CommandContext(ctx, "ps", "-p", pid, "-o", "args=").Output()
			return strings.TrimSpace(string(out)), err
		},
		lsof: func(ctx context.Context, pid string) (string, error) {
			out, err := exec.CommandContext(ctx, "lsof", "-Fn", "-p", pid).Output()
			return string(out), err
		},
		procCwd: func(ctx context.Context, pid string) (string, error) {
			return procCwd(ctx, pid), nil
		},
		now:     time.Now,
	}
}

func resolveFromShellPID(shellPID string, probe resolveProbe) ResolveResult {
	if probe.ps == nil {
		probe = defaultResolveProbe()
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return ResolveResult{}
	}
	shellPID = strings.TrimSpace(shellPID)
	if shellPID == "" {
		return ResolveResult{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveProbeTimeout)
	defer cancel()

	psOut, err := probe.ps(ctx)
	if err != nil {
		return ResolveResult{}
	}
	parents, comms := parseProcessTree(psOut)
	pids := claudeDescendants(shellPID, parents, comms)
	if len(pids) == 0 {
		return ResolveResult{}
	}

	nowFn := probe.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()

	if amb := sameCwdAmbiguousResult(ctx, probe, pids, now); amb.Ambiguous {
		return amb
	}

	candidates := make([]ResolveCandidate, 0, len(pids))
	for _, pid := range pids {
		candidates = append(candidates, resolveClaudePID(ctx, probe, pid, now)...)
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

// sameCwdAmbiguousResult fires when several claude descendants share a cwd and
// cwd-based fallbacks surface multiple distinct sessions.
func sameCwdAmbiguousResult(ctx context.Context, probe resolveProbe, pids []string, now time.Time) ResolveResult {
	if len(pids) <= 1 {
		return ResolveResult{}
	}
	byCwd := map[string][]string{}
	for _, pid := range pids {
		cwd, _ := probe.procCwd(ctx, pid)
		cwd = filepath.Clean(cwd)
		if cwd == "" {
			continue
		}
		byCwd[cwd] = append(byCwd[cwd], pid)
	}
	projectsRoot := claudeProjectsDir()
	for cwd, group := range byCwd {
		if len(group) <= 1 {
			continue
		}
		if allResolvedViaPrimary(ctx, probe, group) {
			continue
		}
		ids := recentTranscriptSessionIDs(projectsRoot, cwd, now)
		if len(ids) <= 1 {
			continue
		}
		return ResolveResult{
			Ambiguous:  true,
			Candidates: candidatesFromIDs(ids, group[0], cwd),
		}
	}
	return ResolveResult{}
}

func allResolvedViaPrimary(ctx context.Context, probe resolveProbe, pids []string) bool {
	seen := map[string]bool{}
	for _, pid := range pids {
		args, _ := probe.procArgs(ctx, pid)
		if sid := parseResumeSessionID(args); sid != "" {
			seen[sid] = true
			continue
		}
		lsofOut, _ := probe.lsof(ctx, pid)
		if sid := parseTranscriptSessionID(lsofOut); sid != "" {
			seen[sid] = true
		}
	}
	return len(seen) == 1
}

func resolveClaudePID(ctx context.Context, probe resolveProbe, pid string, now time.Time) []ResolveCandidate {
	args, _ := probe.procArgs(ctx, pid)
	sessionID := parseResumeSessionID(args)
	if sessionID == "" {
		lsofOut, _ := probe.lsof(ctx, pid)
		sessionID = parseTranscriptSessionID(lsofOut)
	}
	cwd, _ := probe.procCwd(ctx, pid)
	if sessionID != "" {
		return []ResolveCandidate{{SessionID: sessionID, PID: pid, Cwd: cwd}}
	}
	return resolveFallbackCandidates(pid, cwd, now)
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
