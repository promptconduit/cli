package enrich

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/git"
)

// Git extraction costs several subprocesses, and tool-level hooks fire many
// times a second across concurrent agents (Pre + Post + Batch per tool call).
// So high-frequency events reuse a short-lived per-cwd snapshot of the git
// context instead of re-running git each time. A snapshot is trusted only
// while it is younger than vcsSnapshotTTL AND a stat fingerprint of the
// relevant git files is unchanged (see repoFingerprint), so a commit,
// checkout, stage, push or fetch made by the tool call is seen by its own
// Post event. Plain working-tree edits can leave
// the dirty counts up to vcsSnapshotTTL stale. Turn-boundary events (prompt,
// stop, session/subagent start and end) always extract fresh and refresh the
// snapshot, so the events reports anchor on are exact.

const (
	vcsSnapshotSubdir = "enrich/vcs-snapshots"
	vcsSnapshotTTL    = 5 * time.Second
	// vcsSnapshotMaxAge is when an abandoned cwd's snapshot file is GC'd.
	vcsSnapshotMaxAge = 24 * time.Hour
)

// highFrequencyEvents are the per-tool-call hook events (Claude Code, Cursor,
// Gemini CLI) allowed to use a cached snapshot. Anything not listed —
// including unknown future events — extracts fresh.
var highFrequencyEvents = map[string]bool{
	// Claude Code
	"PreToolUse": true, "PostToolUse": true, "PostToolUseFailure": true,
	"PostToolBatch": true, "PermissionRequest": true, "PermissionDenied": true,
	"Notification": true,
	// Cursor
	"preToolUse": true, "postToolUse": true, "postToolUseFailure": true,
	"beforeShellExecution": true, "afterShellExecution": true,
	"beforeMCPExecution": true, "afterMCPExecution": true,
	"beforeReadFile": true, "afterFileEdit": true, "afterAgentThought": true,
	"beforeTabFileRead": true, "afterTabFileEdit": true,
	// Gemini CLI
	"BeforeTool": true, "AfterTool": true, "BeforeModel": true,
	"AfterModel": true, "BeforeToolSelection": true,
}

// vcsSnapshot is one cached extraction plus the repo-state fingerprint it was
// taken under.
type vcsSnapshot struct {
	CapturedAt  time.Time            `json:"captured_at"`
	Fingerprint string               `json:"fingerprint"`
	Git         *envelope.GitContext `json:"git"`
}

// extractGitContext is swapped in tests to count extractions.
var extractGitContext = git.ExtractContext

// gitContext returns the git context for ctx.Cwd, from a still-valid snapshot
// when the event is high-frequency, otherwise by extracting (and
// re-snapshotting).
func gitContext(ctx *Context) *envelope.GitContext {
	path := vcsSnapshotPath(ctx.Cwd)
	prev, havePrev := loadVCSSnapshot(path)
	if highFrequencyEvents[ctx.HookEvent] && havePrev && snapshotValid(prev) {
		return prev.Git
	}

	// Fingerprint the repo BEFORE extracting (via the previous snapshot's dirs)
	// and again after. Only vouch for the snapshot when they match: a git
	// change landing mid-extraction would otherwise pair old data with the new
	// fingerprint. With no previous snapshot there's nothing to compare, so the
	// first snapshot for a cwd is stored unvouched (one extra extraction).
	capturedAt := time.Now()
	before := ""
	if havePrev && prev.Git != nil {
		before = repoFingerprint(prev.Git)
	}
	gc := extractGitContext(ctx.Cwd)
	// nil = not a repo OR a failed/timed-out rev-parse; we can't tell which.
	// Degraded = status failed, counts unknown. Never cache either: a transient
	// failure must not blank or falsify the slug for the TTL.
	if gc == nil || gc.Degraded {
		return gc
	}
	fp := repoFingerprint(gc)
	if fp != before {
		fp = ""
	}
	snap := vcsSnapshot{CapturedAt: capturedAt, Fingerprint: fp, Git: gc}
	if data, err := json.Marshal(snap); err == nil {
		dir := filepath.Dir(path)
		if writeFileAtomic(dir, path, data) {
			maybeGC(dir, vcsSnapshotMaxAge)
		}
	}
	return gc
}

func snapshotValid(snap vcsSnapshot) bool {
	// age < 0 means the clock moved backwards; don't trust the entry.
	if age := time.Since(snap.CapturedAt); age < 0 || age >= vcsSnapshotTTL {
		return false
	}
	if snap.Git == nil || snap.Fingerprint == "" {
		return false
	}
	return snap.Fingerprint == repoFingerprint(snap.Git)
}

// repoFingerprint summarizes the files git rewrites when anything the vcs
// slug reports changes. Per-worktree: HEAD (branch switch), index (stage,
// commit -a, checkout), logs/HEAD (every HEAD movement). Shared: the branch's
// loose ref (commit/reset even with reflogs off), origin's ref for the branch
// (push/fetch → ahead/behind), packed-refs, FETCH_HEAD and config (remote
// URL). Stats only — no subprocess. "" when the dirs are unknown; snapshotValid
// never accepts "".
func repoFingerprint(gc *envelope.GitContext) string {
	if gc == nil || gc.GitDir == "" || gc.CommonDir == "" {
		return ""
	}
	files := []string{
		filepath.Join(gc.GitDir, "HEAD"),
		filepath.Join(gc.GitDir, "index"),
		filepath.Join(gc.GitDir, "logs", "HEAD"),
		filepath.Join(gc.CommonDir, "packed-refs"),
		filepath.Join(gc.CommonDir, "FETCH_HEAD"),
		filepath.Join(gc.CommonDir, "config"),
	}
	if gc.Branch != "" {
		files = append(files,
			filepath.Join(gc.CommonDir, "refs", "heads", gc.Branch),
			filepath.Join(gc.CommonDir, "refs", "remotes", "origin", gc.Branch))
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			parts = append(parts, fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size()))
		} else {
			parts = append(parts, "-")
		}
	}
	return strings.Join(parts, "|")
}

func vcsSnapshotPath(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return filepath.Join(enrichBaseDir(), vcsSnapshotSubdir, hex.EncodeToString(sum[:12])+".json")
}

func loadVCSSnapshot(path string) (vcsSnapshot, bool) {
	var snap vcsSnapshot
	data, err := os.ReadFile(path)
	if err != nil {
		return snap, false
	}
	if err := json.Unmarshal(data, &snap); err != nil || snap.CapturedAt.IsZero() {
		return snap, false
	}
	return snap, true
}
