package git

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/schema"
)

const gitTimeout = 2 * time.Second

// ExtractContext extracts git repository information from the given directory
func ExtractContext(workingDir string) *schema.GitContext {
	if workingDir == "" {
		return nil
	}

	// Check if it's a git repo
	repoRoot := runGitCmd(workingDir, "rev-parse", "--show-toplevel")
	if repoRoot == "" {
		return nil
	}

	ctx := &schema.GitContext{}

	// Commit info
	if hash := runGitCmd(workingDir, "rev-parse", "HEAD"); hash != "" {
		ctx.CommitHash = &hash
	}
	if msg := runGitCmd(workingDir, "log", "-1", "--format=%s"); msg != "" {
		ctx.CommitMessage = &msg
	}
	if author := runGitCmd(workingDir, "log", "-1", "--format=%an"); author != "" {
		ctx.CommitAuthor = &author
	}

	// Branch info
	if branch := runGitCmd(workingDir, "branch", "--show-current"); branch != "" {
		ctx.Branch = &branch
		detached := false
		ctx.IsDetachedHead = &detached
	} else {
		// Detached HEAD state
		detached := true
		ctx.IsDetachedHead = &detached
	}

	// Working tree state
	status := runGitCmd(workingDir, "status", "--porcelain")
	staged, unstaged, untracked := parseStatusOutput(status)
	ctx.StagedCount = &staged
	ctx.UnstagedCount = &unstaged
	ctx.UntrackedCount = &untracked

	dirty := (staged + unstaged + untracked) > 0
	ctx.IsDirty = &dirty

	// Remote info
	if remote := runGitCmd(workingDir, "remote", "get-url", "origin"); remote != "" {
		ctx.RemoteURL = &remote
	}

	// Ahead/behind counts
	if counts := runGitCmd(workingDir, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); counts != "" {
		ahead, behind := parseAheadBehind(counts)
		ctx.AheadCount = &ahead
		ctx.BehindCount = &behind
	}

	return ctx
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
