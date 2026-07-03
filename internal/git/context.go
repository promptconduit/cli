package git

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
)

const gitTimeout = 2 * time.Second

// ExtractContext extracts git repository information from the given directory
func ExtractContext(workingDir string) *envelope.GitContext {
	if workingDir == "" {
		return nil
	}

	// Check if it's a git repo
	repoRoot := runGitCmd(workingDir, "rev-parse", "--show-toplevel")
	if repoRoot == "" {
		return nil
	}

	ctx := &envelope.GitContext{
		WorkingDirectory: workingDir,
		RepoPath:         repoRoot,
		RepoName:         GetRepoName(workingDir),
	}

	// Commit info
	if hash := runGitCmd(workingDir, "rev-parse", "HEAD"); hash != "" {
		ctx.CommitHash = hash
	}
	if msg := runGitCmd(workingDir, "log", "-1", "--format=%s"); msg != "" {
		ctx.CommitMessage = msg
	}
	if author := runGitCmd(workingDir, "log", "-1", "--format=%an"); author != "" {
		ctx.CommitAuthor = author
	}

	// Branch info
	if branch := runGitCmd(workingDir, "branch", "--show-current"); branch != "" {
		ctx.Branch = branch
		ctx.IsDetachedHead = false
	} else {
		// Detached HEAD state
		ctx.IsDetachedHead = true
	}

	// Working tree state
	status := runGitCmd(workingDir, "status", "--porcelain")
	staged, unstaged, untracked := parseStatusOutput(status)
	ctx.StagedCount = staged
	ctx.UnstagedCount = unstaged
	ctx.UntrackedCount = untracked
	ctx.IsDirty = (staged + unstaged + untracked) > 0

	// Remote info
	if remote := runGitCmd(workingDir, "remote", "get-url", "origin"); remote != "" {
		ctx.RemoteURL = remote
	}

	// Worktree detection: a linked worktree has a per-worktree git dir that
	// differs from the shared common dir. This catches sessions started *inside*
	// an existing worktree, which the WorktreeCreate hook never reports. Reuse
	// repoRoot (already the worktree's top-level) for the path — no extra call.
	if detectWorktree(workingDir) {
		ctx.IsWorktree = true
		ctx.WorktreePath = repoRoot
	}

	// Ahead/behind counts
	if counts := runGitCmd(workingDir, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); counts != "" {
		ahead, behind := parseAheadBehind(counts)
		ctx.AheadCount = ahead
		ctx.BehindCount = behind
	}

	return ctx
}

// detectWorktree reports whether workingDir is a linked git worktree (not the
// main checkout). A linked worktree's per-worktree git dir
// (.git/worktrees/<name>) differs from the shared common dir; in the main
// checkout the two are identical.
//
// Both paths come from a SINGLE `git rev-parse --git-dir --git-common-dir`
// invocation so they're resolved with identical semantics — this avoids
// false positives from symlink/case differences between two separate
// subcommands, needs no `--path-format` (so it works on git >= 2.5), and adds
// just one subprocess to the latency-sensitive hook path.
func detectWorktree(workingDir string) bool {
	out := runGitCmd(workingDir, "rev-parse", "--git-dir", "--git-common-dir")
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return false
	}
	gitDir, commonDir := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	if gitDir == "" || commonDir == "" {
		return false
	}
	// Resolve both relative to workingDir for a stable comparison (git may emit
	// either path relative to the cwd).
	abs := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(workingDir, p)
		}
		return filepath.Clean(p)
	}
	return abs(gitDir) != abs(commonDir)
}

// runGitCmd executes a git command with timeout and returns trimmed stdout
func runGitCmd(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return ""
	}

	return strings.TrimSpace(stdout.String())
}

// parseStatusOutput parses git status --porcelain output
func parseStatusOutput(status string) (staged, unstaged, untracked int) {
	if status == "" {
		return 0, 0, 0
	}

	for _, line := range strings.Split(status, "\n") {
		if len(line) < 2 {
			continue
		}
		index := line[0]
		workTree := line[1]

		// Untracked files
		if index == '?' && workTree == '?' {
			untracked++
			continue
		}

		// Staged changes (index column has change marker)
		if index != ' ' && index != '?' {
			staged++
		}

		// Unstaged changes (work tree column has change marker)
		if workTree != ' ' && workTree != '?' {
			unstaged++
		}
	}

	return staged, unstaged, untracked
}

// parseAheadBehind parses output of git rev-list --left-right --count
func parseAheadBehind(counts string) (ahead, behind int) {
	parts := strings.Fields(counts)
	if len(parts) != 2 {
		return 0, 0
	}

	behind, _ = strconv.Atoi(parts[0])
	ahead, _ = strconv.Atoi(parts[1])
	return ahead, behind
}

// DefaultBranch returns origin's HEAD branch name (e.g. "main"), or "" when
// unknown. Reads the local ref only — no network. The ref may be absent on
// fresh clones that never ran `git remote set-head`; that's fine, we omit it.
func DefaultBranch(workingDir string) string {
	ref := runGitCmd(workingDir, "symbolic-ref", "refs/remotes/origin/HEAD")
	const prefix = "refs/remotes/origin/"
	if strings.HasPrefix(ref, prefix) {
		return strings.TrimPrefix(ref, prefix)
	}
	return ""
}

// GetRepoName extracts repository name from path or git remote
func GetRepoName(workingDir string) string {
	// Try to get from git remote URL
	if remote := runGitCmd(workingDir, "remote", "get-url", "origin"); remote != "" {
		// Extract repo name from URL
		// github.com/user/repo.git -> repo
		remote = strings.TrimSuffix(remote, ".git")
		if idx := strings.LastIndex(remote, "/"); idx != -1 {
			return remote[idx+1:]
		}
	}

	// Fall back to directory name
	return filepath.Base(workingDir)
}
