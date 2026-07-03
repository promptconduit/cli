package sessions

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// liveProbeTimeout bounds the process inspection so `sessions` never hangs on a
// slow lsof.
const liveProbeTimeout = 3 * time.Second

// LiveClaudeCwds returns the set of working directories that currently have a
// running `claude` process. Used to mark sessions Alive so restore never opens
// a second terminal on top of a session that's still going. Best-effort: on any
// error (no pgrep/lsof, permission, non-Unix) it returns an empty set, so the
// caller degrades to "assume nothing is alive" rather than failing.
func LiveClaudeCwds() map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), liveProbeTimeout)
	defer cancel()

	cwds := map[string]bool{}
	for _, pid := range claudePIDs(ctx) {
		if cwd := procCwd(ctx, pid); cwd != "" {
			cwds[cwd] = true
		}
	}
	return cwds
}

// MarkAlive sets Alive on each session whose Cwd has a live claude process.
func MarkAlive(list []Session, liveCwds map[string]bool) {
	for i := range list {
		if liveCwds[list[i].Cwd] {
			list[i].Alive = true
		}
	}
}

// claudePIDs lists the pids of running Claude Code processes. We enumerate with
// `ps` rather than `pgrep` deliberately: pgrep does not report the probe's own
// ancestor process, so when `sessions` runs from *inside* a Claude Code terminal
// (the extension's normal path is safe, but `promptconduit sessions` by hand is
// not) pgrep silently drops that very session and reports it as interrupted.
// `ps -axo` sees every process, ancestors included.
func claudePIDs(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil
	}
	return parseClaudePIDs(string(out))
}

// parseClaudePIDs extracts the pids of `claude` processes from `ps -axo
// pid=,comm=` output. A line is "<pid> <comm>", where comm may itself be a path
// with spaces (the Claude *desktop app* is
// /Applications/Claude.app/…/Claude); we match on the executable basename being
// exactly "claude" so the CLI is kept and the desktop app ("Claude") is not.
func parseClaudePIDs(psOutput string) []string {
	var pids []string
	for _, line := range strings.Split(psOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, comm, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if filepath.Base(strings.TrimSpace(comm)) == "claude" {
			pids = append(pids, pid)
		}
	}
	return pids
}

// procCwd returns the current working directory of a pid via lsof's field
// output (-Fn prints an "n<path>" line for the cwd fd).
func procCwd(ctx context.Context, pid string) string {
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", pid, "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimSpace(line[1:])
		}
	}
	return ""
}
