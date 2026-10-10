package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
)

const gitTimeout = 2 * time.Second

// ExtractContext extracts git repository information from the given directory.
//
// It runs on the hook's hot path — once per captured event, for every
// concurrent agent — so it is built to spawn as few git processes as possible
// (4: rev-parse, status, log, remote). Branch, HEAD, upstream ahead/behind and
// the working-tree counts all come from one `git status --porcelain=v2
// --branch`, and origin's default branch is read from the ref file directly.
func ExtractContext(workingDir string) *envelope.GitContext {
	if workingDir == "" {
		return nil
	}

	// One rev-parse for the repo check, top-level, and worktree detection.
	repoRoot, gitDir, commonDir, ok := revParseDirs(workingDir)
	if !ok {
		return nil
	}

	ctx := &envelope.GitContext{
		WorkingDirectory: workingDir,
		RepoPath:         repoRoot,
		GitDir:           gitDir,
	}

	// Branch, HEAD oid, ahead/behind and working-tree counts.
	st := parseStatusV2(runGitCmd(workingDir, "status", "--porcelain=v2", "--branch"))
	if !st.ok {
		// status failed or timed out (e.g. a huge untracked scan). Don't report
		// a bogus detached HEAD: recover the cheap identity fields directly;
		// counts and ahead/behind stay zero.
		st.oid = runGitCmd(workingDir, "rev-parse", "HEAD")
		st.branch = runGitCmd(workingDir, "branch", "--show-current")
	}
	ctx.CommitHash = st.oid
	ctx.Branch = st.branch
	ctx.IsDetachedHead = st.branch == ""
	ctx.StagedCount = st.staged
	ctx.UnstagedCount = st.unstaged
	ctx.UntrackedCount = st.untracked
	ctx.IsDirty = (st.staged + st.unstaged + st.untracked) > 0
	ctx.AheadCount = st.ahead
	ctx.BehindCount = st.behind

	// Commit subject + author in one call (skipped on an unborn branch).
	if ctx.CommitHash != "" {
		if out := runGitCmd(workingDir, "log", "-1", "--format=%s%x00%an"); out != "" {
			msg, author, _ := strings.Cut(out, "\x00")
			ctx.CommitMessage = msg
			ctx.CommitAuthor = author
		}
	}

	ctx.RemoteURL = runGitCmd(workingDir, "remote", "get-url", "origin")
	ctx.RepoName = repoNameFromRemote(ctx.RemoteURL, workingDir)
	ctx.DefaultBranch = defaultBranchFromCommonDir(workingDir, commonDir)

	// Worktree detection: a linked worktree has a per-worktree git dir that
	// differs from the shared common dir. This catches sessions started *inside*
	// an existing worktree, which the WorktreeCreate hook never reports. Reuse
	// repoRoot (already the worktree's top-level) for the path — no extra call.
	if gitDir != commonDir {
		ctx.IsWorktree = true
		ctx.WorktreePath = repoRoot
	}

	return ctx
}

// revParseDirs returns the work-tree top level plus the per-worktree and
// shared git dirs (absolute, cleaned) from a SINGLE `git rev-parse`. ok is
// false when workingDir is not inside a work tree.
//
// Resolving git-dir and common-dir in one invocation gives them identical
// semantics, avoiding false worktree positives from symlink/case differences
// between separate subcommands, and needs no `--path-format`. (ExtractContext
// as a whole needs git >= 2.11 for `status --porcelain=v2`.)
func revParseDirs(workingDir string) (repoRoot, gitDir, commonDir string, ok bool) {
	out := runGitCmd(workingDir, "rev-parse", "--show-toplevel", "--git-dir", "--git-common-dir")
	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		return "", "", "", false
	}
	repoRoot = strings.TrimSpace(lines[0])
	gitDir, commonDir = strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2])
	if repoRoot == "" || gitDir == "" || commonDir == "" {
		return "", "", "", false
	}
	// git may emit either dir relative to the cwd; resolve both against
	// workingDir for a stable comparison.
	abs := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(workingDir, p)
		}
		return filepath.Clean(p)
	}
	return repoRoot, abs(gitDir), abs(commonDir), true
}

// statusV2 is what ExtractContext needs from `git status --porcelain=v2 --branch`.
type statusV2 struct {
	ok                          bool   // the branch header was present (status succeeded)
	oid                         string // "" on an unborn branch
	branch                      string // "" when HEAD is detached
	ahead, behind               int
	staged, unstaged, untracked int
}

// parseStatusV2 parses `git status --porcelain=v2 --branch` output:
//
//	# branch.oid <commit> | (initial)
//	# branch.head <branch> | (detached)
//	# branch.ab +<ahead> -<behind>      (only when an upstream resolves)
//	1 <XY> ...                          ordinary changed entry
//	2 <XY> ...                          renamed/copied entry
//	u <XY> ...                          unmerged entry
//	? <path>                            untracked
//
// In XY, X is the index (staged) state and Y the work-tree state; "." means
// unmodified.
func parseStatusV2(out string) statusV2 {
	var st statusV2
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			if oid := strings.TrimPrefix(line, "# branch.oid "); oid != "(initial)" {
				st.oid = oid
			}
		case strings.HasPrefix(line, "# branch.head "):
			st.ok = true
			if head := strings.TrimPrefix(line, "# branch.head "); head != "(detached)" {
				st.branch = head
			}
		case strings.HasPrefix(line, "# branch.ab "):
			for _, f := range strings.Fields(strings.TrimPrefix(line, "# branch.ab ")) {
				n, _ := strconv.Atoi(f[1:])
				switch f[0] {
				case '+':
					st.ahead = n
				case '-':
					st.behind = n
				}
			}
		case strings.HasPrefix(line, "? "):
			st.untracked++
		case len(line) >= 4 && (line[0] == '1' || line[0] == '2' || line[0] == 'u') && line[1] == ' ':
			if line[2] != '.' {
				st.staged++
			}
			if line[3] != '.' {
				st.unstaged++
			}
		}
	}
	return st
}

// defaultBranchFromCommonDir returns origin's HEAD branch name (e.g. "main"),
// or "" when unknown. Local ref only — no network; the ref may be absent on
// fresh clones that never ran `git remote set-head`, and that's fine.
//
// It reads refs/remotes/origin/HEAD straight from the shared git dir — symbolic refs are always loose files ("ref: refs/remotes/
// origin/main"), never packed — saving a subprocess on the hook path. Repos
// on the reftable backend have no loose refs, so they fall back to git.
func defaultBranchFromCommonDir(workingDir, commonDir string) string {
	const prefix = "refs/remotes/origin/"
	var ref string
	if data, err := os.ReadFile(filepath.Join(commonDir, "refs", "remotes", "origin", "HEAD")); err == nil {
		ref = strings.TrimSpace(strings.TrimPrefix(string(data), "ref:"))
	} else if _, err := os.Stat(filepath.Join(commonDir, "reftable")); err == nil {
		ref = runGitCmd(workingDir, "symbolic-ref", "refs/remotes/origin/HEAD")
	}
	if strings.HasPrefix(ref, prefix) {
		return strings.TrimPrefix(ref, prefix)
	}
	return ""
}

// runGitCmd executes a git command with timeout and returns trimmed stdout.
//
// GIT_OPTIONAL_LOCKS=0 stops read-only commands (status, diff) from taking
// .git/index.lock to opportunistically refresh the index. The hook fires on
// every tool call while the agent itself may be running `git add`/`commit`;
// without this, the two race and the agent's command can fail with
// "index.lock: File exists".
func runGitCmd(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return ""
	}

	return strings.TrimSpace(stdout.String())
}

// diffShortstatRE parses `git diff --shortstat` output, whose insertion and
// deletion clauses are each optional:
//
//	" 3 files changed, 120 insertions(+), 40 deletions(-)"
var diffShortstatRE = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

// DiffShortstat returns the working-tree change counts vs HEAD (staged +
// unstaged). ok is false when workingDir isn't a git repo or git failed; a
// clean tree returns (0, 0, 0, true).
func DiffShortstat(workingDir string) (files, insertions, deletions int, ok bool) {
	// Distinguish "clean tree" (empty output, success) from "not a repo"
	// (command failure, also empty via runGitCmd) with an explicit repo check.
	if runGitCmd(workingDir, "rev-parse", "--git-dir") == "" {
		return 0, 0, 0, false
	}
	out := runGitCmd(workingDir, "diff", "HEAD", "--shortstat")
	if out == "" {
		return 0, 0, 0, true // clean tree (or unborn HEAD — treat as no changes)
	}
	m := diffShortstatRE.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, 0, false
	}
	files, _ = strconv.Atoi(m[1])
	insertions, _ = strconv.Atoi(m[2])
	deletions, _ = strconv.Atoi(m[3])
	return files, insertions, deletions, true
}

// GetRepoName extracts repository name from path or git remote
func GetRepoName(workingDir string) string {
	return repoNameFromRemote(runGitCmd(workingDir, "remote", "get-url", "origin"), workingDir)
}

// repoNameFromRemote derives the repo name from a remote URL
// (github.com/user/repo.git -> repo), falling back to the directory name.
func repoNameFromRemote(remote, workingDir string) string {
	if remote != "" {
		remote = strings.TrimSuffix(remote, ".git")
		if idx := strings.LastIndex(remote, "/"); idx != -1 {
			return remote[idx+1:]
		}
	}
	return filepath.Base(workingDir)
}
