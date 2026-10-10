package enrich

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/git"
)

// Git extraction costs several subprocesses, and tool-level hooks fire many
// times a second across concurrent agents (Pre + Post + Batch per tool call).
// So high-frequency events reuse a short-lived per-cwd snapshot of the git
// context instead of re-running git each time. Turn-boundary events (prompt,
// stop, session/subagent start and end) always extract fresh and refresh the
// snapshot, so the events reports anchor on are exact; a tool event's vcs
// slug is at most vcsSnapshotTTL old.

const (
	vcsSnapshotSubdir = "enrich/vcs-snapshots"
	vcsSnapshotTTL    = 5 * time.Second
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

// vcsSnapshot is one cached extraction. Git is nil when the cwd is not a git
// repo — cached too, so non-repo cwds skip git entirely within the TTL.
type vcsSnapshot struct {
	CapturedAt time.Time            `json:"captured_at"`
	Git        *envelope.GitContext `json:"git"`
}

// extractGitContext is swapped in tests to count extractions.
var extractGitContext = git.ExtractContext

// gitContext returns the git context for ctx.Cwd, from a fresh snapshot when
// the event is high-frequency, otherwise by extracting (and re-snapshotting).
func gitContext(ctx *Context) *envelope.GitContext {
	path := vcsSnapshotPath(ctx.Cwd)
	if highFrequencyEvents[ctx.HookEvent] {
		if snap, ok := loadVCSSnapshot(path); ok {
			// age < 0 means the clock moved backwards; don't trust the entry.
			if age := time.Since(snap.CapturedAt); age >= 0 && age < vcsSnapshotTTL {
				return snap.Git
			}
		}
	}
	gc := extractGitContext(ctx.Cwd)
	if data, err := json.Marshal(vcsSnapshot{CapturedAt: time.Now(), Git: gc}); err == nil {
		dir := filepath.Dir(path)
		if writeFileAtomic(dir, path, data) {
			maybeGCState(dir)
		}
	}
	return gc
}

func vcsSnapshotPath(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	base := client.ConfigDir()
	if stateDirOverride != "" {
		base = stateDirOverride
	}
	return filepath.Join(base, vcsSnapshotSubdir, hex.EncodeToString(sum[:12])+".json")
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
