package sessions

import (
	"context"
	"os/exec"
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

func claudePIDs(ctx context.Context) []string {
	// -x: match the exact process name, so we don't catch "claude-something".
	out, err := exec.CommandContext(ctx, "pgrep", "-x", "claude").Output()
	if err != nil {
		return nil
	}
	var pids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			pids = append(pids, p)
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
