// Package sessions reconstructs resumable AI coding sessions from the local
// event log (~/.promptconduit/events.jsonl). It answers "what sessions were
// recently active, and where did they live?" — the substrate for reopening a
// session that was interrupted (e.g. when the editor restarted and took its
// terminals down with it).
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

// Session is a resumable session reconstructed from the event log. It carries
// everything needed to reopen it: the directory `claude --resume` must run from
// and the session id to hand to it.
//
// Cwd is the session's *launch* directory — the one Claude Code stored the
// transcript under and scopes --resume to (worktree-aware: a session launched in
// a worktree points at the worktree path). ReadRecent resolves it from the
// transcript via EnrichLaunchDirs; only when no transcript is found does it fall
// back to the tool cwd from the event log, which can differ (e.g. an --add-dir
// subdirectory) and would make --resume fail.
type Session struct {
	SessionID  string    `json:"session_id"`
	Tool       string    `json:"tool"`
	Cwd        string    `json:"cwd"`
	Repo       string    `json:"repo,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	LastPrompt string    `json:"last_prompt,omitempty"`
	LastActive time.Time `json:"last_active"`
	EventCount int       `json:"event_count"`
	Alive      bool      `json:"alive"` // a live process is already running in Cwd
	// AddDirs are extra directories the session worked in that live *outside*
	// its launch dir (Cwd) — the `--add-dir` set to re-attach on resume so a
	// session that reached across repos comes back with the same working set.
	// Dirs already under Cwd are omitted (resuming from Cwd already covers them).
	AddDirs []string `json:"add_dirs,omitempty"`

	// touched collects every distinct tool cwd seen for this session, in
	// first-seen order. It's the raw material for AddDirs, computed once the
	// launch dir is known; unexported so it never reaches JSON.
	touched []string
}

// resumableTools are the CLI tools whose sessions can be reopened with a resume
// command. Cursor's built-in agent is intentionally excluded — it's not a CLI
// and has no equivalent to `claude --resume`.
var resumableTools = map[string]bool{
	"claude-code": true,
}

// envelope is the minimal slice of an events.jsonl line we need. The event log
// stores far more; we only decode the resume-relevant fields.
type envelope struct {
	Tool       string `json:"tool"`
	HookEvent  string `json:"hook_event"`
	CapturedAt string `json:"captured_at"`
	Native     struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Prompt    string `json:"prompt"`
	} `json:"native_payload"`
	Git struct {
		RepoName string `json:"repo_name"`
		Branch   string `json:"branch"`
	} `json:"git"`
}

// Aggregate folds a chronological run of event lines into one Session per
// session id, keeping the newest cwd/branch/tool and the most recent prompt
// seen. Lines that don't parse, lack a session id or cwd, or belong to a
// non-resumable tool are skipped. Result is sorted newest-active first.
func Aggregate(lines []string) []Session {
	bySession := map[string]*Session{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e envelope
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		sid := e.Native.SessionID
		if sid == "" || e.Native.Cwd == "" || !resumableTools[e.Tool] {
			continue
		}
		t, _ := time.Parse(time.RFC3339, e.CapturedAt)

		s := bySession[sid]
		if s == nil {
			s = &Session{SessionID: sid, Tool: e.Tool}
			bySession[sid] = s
		}
		s.EventCount++
		s.addTouched(e.Native.Cwd)
		// Keep the fields from the latest event so cwd/branch reflect where the
		// session ended up (it can move between worktrees mid-session).
		if !t.IsZero() && !t.Before(s.LastActive) {
			s.LastActive = t
			s.Cwd = e.Native.Cwd
			s.Repo = e.Git.RepoName
			s.Branch = e.Git.Branch
		} else if s.Cwd == "" {
			// No usable timestamp yet — still capture a cwd so the session is
			// restorable.
			s.Cwd = e.Native.Cwd
			s.Repo = e.Git.RepoName
			s.Branch = e.Git.Branch
		}
		// Use the event's prompt as a human label, but skip system/tool-injected
		// messages (task notifications, tool XML) that start with "<" — they're
		// not something the user typed and make for a confusing label.
		if p := strings.TrimSpace(e.Native.Prompt); p != "" && !strings.HasPrefix(p, "<") {
			s.LastPrompt = truncate(p, 140)
		}
	}

	out := make([]Session, 0, len(bySession))
	for _, s := range bySession {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActive.Equal(out[j].LastActive) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].LastActive.After(out[j].LastActive)
	})
	return out
}

// Filter keeps only sessions active at or after cutoff.
func Filter(all []Session, cutoff time.Time) []Session {
	out := make([]Session, 0, len(all))
	for _, s := range all {
		if !s.LastActive.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

// Default bound on the tail read of the event log. The newest bytes hold the
// recent sessions; reading the whole (potentially 100MB+) file on every call
// would be wasteful. Tens of thousands of events fit in 16MB.
const defaultMaxBytes int64 = 16 * 1024 * 1024

// ReadRecent returns sessions active within `since` of `now`, newest first. It
// reads only the tail of the event log for speed. A missing log yields an empty
// slice, not an error (Free/local-only users with logging on still have it; a
// user who disabled logging simply has nothing to restore).
func ReadRecent(since time.Duration, now time.Time) ([]Session, error) {
	list, err := readRecentFrom(eventlog.EventsJSONLPath(), since, now, defaultMaxBytes)
	if err != nil {
		return nil, err
	}
	// Correct each Cwd from the event's tool cwd to the session's launch dir,
	// so `resume` cd's where --resume actually works and MarkAlive matches the
	// live process's real cwd.
	EnrichLaunchDirs(list)
	// With the launch dir settled, derive the --add-dir set (dirs the session
	// touched that live outside it).
	for i := range list {
		list[i].AddDirs = additionalDirs(list[i].Cwd, list[i].touched)
		list[i].touched = nil
	}
	return list, nil
}

func readRecentFrom(path string, since time.Duration, now time.Time, maxBytes int64) ([]Session, error) {
	lines, err := tailLines(path, maxBytes)
	if err != nil {
		return nil, err
	}
	all := Aggregate(lines)
	return Filter(all, now.Add(-since)), nil
}

// tailLines reads the last maxBytes of path and returns complete lines. When the
// read starts mid-file the leading partial line is dropped. A missing file is
// not an error — it returns no lines.
func tailLines(path string, maxBytes int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err.Error() != "EOF" {
		return nil, err
	}
	parts := strings.Split(string(buf), "\n")
	if start > 0 && len(parts) > 0 {
		parts = parts[1:] // drop the leading partial line
	}
	return parts, nil
}

// addTouched records a distinct tool cwd (first-seen order preserved).
func (s *Session) addTouched(cwd string) {
	if cwd == "" {
		return
	}
	for _, d := range s.touched {
		if d == cwd {
			return
		}
	}
	s.touched = append(s.touched, cwd)
}

// additionalDirs returns the subset of touched dirs that live outside launch —
// the `--add-dir` set to re-attach on resume. A dir equal to launch, or nested
// under it, is dropped: resuming from launch already grants access to it, so
// re-adding it would only clutter the command. Order follows first-seen.
func additionalDirs(launch string, touched []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range touched {
		if d == "" || seen[d] || d == launch || isSubpath(launch, d) {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// isSubpath reports whether child is base or nested beneath it. Both are assumed
// absolute (they come from process/tool cwds); a non-absolute or escaping
// relation yields false.
func isSubpath(base, child string) bool {
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
