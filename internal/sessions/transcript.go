package sessions

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// launchDirMaxLineBytes bounds a single transcript line we're willing to scan
// for the launch cwd. The first lines of a Claude Code transcript are small
// session metadata, but a later line can be a huge tool result — cap the
// scanner so a pathological line can't blow up memory.
const launchDirMaxLineBytes = 8 * 1024 * 1024

// claudeProjectsDir is where Claude Code stores per-project transcripts
// (~/.claude/projects/<encoded-launch-dir>/<session-id>.jsonl). Returns "" when
// the home directory can't be determined.
func claudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ResolveLaunchDir returns the directory `claude --resume <id>` must be run from
// — the directory Claude Code launched the session in, which is where it stored
// the transcript. This is deliberately NOT the session's tool cwd
// (native_payload.cwd), which can point at an --add-dir subdirectory: Claude
// Code scopes --resume to the project derived from the *current* cwd, so
// resuming from the tool cwd fails with "No conversation found". We recover the
// launch dir by locating the session's transcript under ~/.claude/projects and
// reading its recorded cwd (exact, unlike decoding the mangled folder name).
// Returns "" when no transcript is found, so callers can fall back to the event
// cwd.
func ResolveLaunchDir(sessionID string) string {
	return resolveLaunchDir(claudeProjectsDir(), sessionID)
}

func resolveLaunchDir(projectsRoot, sessionID string) string {
	if projectsRoot == "" || sessionID == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(projectsRoot, "*", sessionID+".jsonl"))
	if err != nil {
		return ""
	}
	for _, m := range matches {
		if dir := launchCwdFromTranscript(m); dir != "" {
			return dir
		}
	}
	return ""
}

// launchCwdFromTranscript returns the first "cwd" recorded in a Claude Code
// transcript — the directory the session was launched in. Some leading lines
// (e.g. a summary record) carry no cwd, so we scan until one appears.
func launchCwdFromTranscript(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), launchDirMaxLineBytes)
	for sc.Scan() {
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err == nil && rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// EnrichLaunchDirs rewrites each session's Cwd to the directory `claude
// --resume` must run from, resolved from the Claude Code transcript. Sessions
// whose transcript can't be found keep the cwd reconstructed from the event log.
// This is what makes both `resume` (cd into the right project dir) and
// MarkAlive (match the live process's real cwd) correct when a session was
// launched from a parent dir but worked in an --add-dir subdirectory.
func EnrichLaunchDirs(list []Session) {
	enrichLaunchDirsFrom(claudeProjectsDir(), list)
}

func enrichLaunchDirsFrom(projectsRoot string, list []Session) {
	if projectsRoot == "" {
		return
	}
	for i := range list {
		if dir := resolveLaunchDir(projectsRoot, list[i].SessionID); dir != "" {
			list[i].Cwd = dir
		}
	}
}
