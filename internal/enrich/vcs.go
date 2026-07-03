package enrich

import (
	"strings"

	"github.com/promptconduit/cli/internal/git"
)

// VCSEnrichment is the "vcs" slug: normalized version-control context for the
// event — provider type, web links, branch/commit state, and (when a cached
// lookup has resolved one) the open PR for the current branch.
type VCSEnrichment struct {
	// Type is the provider: github, gitlab, bitbucket, azure, codeberg,
	// sourcehut, or "" when unknown / no remote.
	Type string `json:"type,omitempty"`
	// Repo is the provider-relative slug, e.g. "promptconduit/cli".
	Repo string `json:"repo,omitempty"`
	// RepoURL / BranchURL are browsable https links derived from the remote.
	RepoURL   string `json:"repo_url,omitempty"`
	Branch    string `json:"branch,omitempty"`
	BranchURL string `json:"branch_url,omitempty"`
	// DefaultBranch is origin's HEAD branch (e.g. "main"), when known locally.
	DefaultBranch string `json:"default_branch,omitempty"`
	// PR is the open pull/merge request for Branch, resolved via a disk-cached
	// `gh` lookup (github only for now). Omitted when none is known.
	PR *PRInfo `json:"pr,omitempty"`

	Commit *CommitInfo `json:"commit,omitempty"`

	Dirty     bool `json:"dirty,omitempty"`
	Staged    int  `json:"staged,omitempty"`
	Unstaged  int  `json:"unstaged,omitempty"`
	Untracked int  `json:"untracked,omitempty"`
	Ahead     int  `json:"ahead,omitempty"`
	Behind    int  `json:"behind,omitempty"`

	RemoteURL        string `json:"remote_url,omitempty"`
	RepoPath         string `json:"repo_path,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	DetachedHead     bool   `json:"detached_head,omitempty"`

	Worktree *WorktreeInfo `json:"worktree,omitempty"`
}

// PRInfo describes the open PR/MR associated with the branch.
type PRInfo struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"` // open, merged, closed
}

// CommitInfo is the HEAD commit at event time.
type CommitInfo struct {
	Hash    string `json:"hash"`
	Message string `json:"message,omitempty"`
	Author  string `json:"author,omitempty"`
}

// WorktreeInfo marks events produced inside a linked git worktree.
type WorktreeInfo struct {
	IsWorktree bool   `json:"is_worktree"`
	Path       string `json:"path,omitempty"`
}

type vcsEnricher struct{}

func init() { Register(vcsEnricher{}) }

func (vcsEnricher) Slug() string              { return "vcs" }
func (vcsEnricher) Applies(ctx *Context) bool { return ctx.Cwd != "" }

func (vcsEnricher) Enrich(ctx *Context) (any, error) {
	gc := git.ExtractContext(ctx.Cwd)
	if gc == nil {
		return nil, nil // not a git repo — omit the slug
	}

	v := VCSEnrichment{
		Type:             git.DetectSource(gc.RemoteURL),
		Branch:           gc.Branch,
		DefaultBranch:    git.DefaultBranch(ctx.Cwd),
		Dirty:            gc.IsDirty,
		Staged:           gc.StagedCount,
		Unstaged:         gc.UnstagedCount,
		Untracked:        gc.UntrackedCount,
		Ahead:            gc.AheadCount,
		Behind:           gc.BehindCount,
		RemoteURL:        gc.RemoteURL,
		RepoPath:         gc.RepoPath,
		WorkingDirectory: gc.WorkingDirectory,
		DetachedHead:     gc.IsDetachedHead,
	}
	if gc.CommitHash != "" {
		v.Commit = &CommitInfo{Hash: gc.CommitHash, Message: gc.CommitMessage, Author: gc.CommitAuthor}
	}
	if gc.IsWorktree {
		v.Worktree = &WorktreeInfo{IsWorktree: true, Path: gc.WorktreePath}
	}

	v.Repo, v.RepoURL = normalizeRemote(gc.RemoteURL)
	if v.Repo == "" && gc.RepoName != "" {
		v.Repo = gc.RepoName // no remote: fall back to the local repo name
	}
	v.BranchURL = branchURL(v.Type, v.RepoURL, gc.Branch)

	// PR link: served from the disk cache only; a stale/missing entry kicks
	// off a detached background refresh so the hook itself never waits on gh.
	v.PR = cachedPR(ctx.Cwd, v.Type, v.RepoURL, gc.Branch)

	return v, nil
}

// normalizeRemote turns a git remote URL into (slug, https URL):
// git@github.com:org/repo.git -> ("org/repo", "https://github.com/org/repo").
// Unrecognized forms yield ("", "").
func normalizeRemote(remoteURL string) (slug, url string) {
	if remoteURL == "" {
		return "", ""
	}
	host, path := splitRemote(remoteURL)
	if host == "" || path == "" {
		return "", ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if path == "" {
		return "", ""
	}
	return path, "https://" + host + "/" + path
}

// splitRemote extracts (host, path) from the common remote URL forms.
func splitRemote(remoteURL string) (host, path string) {
	// SSH scp-like: git@host:path
	if strings.HasPrefix(remoteURL, "git@") {
		rest := strings.TrimPrefix(remoteURL, "git@")
		if idx := strings.Index(rest, ":"); idx >= 0 {
			return rest[:idx], rest[idx+1:]
		}
		return rest, ""
	}
	// scheme://[user@]host[:port]/path
	if idx := strings.Index(remoteURL, "://"); idx >= 0 {
		rest := remoteURL[idx+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return rest, ""
		}
		hostPort := rest[:slash]
		if colon := strings.Index(hostPort, ":"); colon >= 0 {
			hostPort = hostPort[:colon]
		}
		return hostPort, rest[slash+1:]
	}
	return "", ""
}

// branchURL builds a browsable link to the branch for providers with a known
// URL scheme. Returns "" otherwise.
func branchURL(provider, repoURL, branch string) string {
	if repoURL == "" || branch == "" {
		return ""
	}
	switch provider {
	case "github", "codeberg":
		return repoURL + "/tree/" + branch
	case "gitlab":
		return repoURL + "/-/tree/" + branch
	case "bitbucket":
		return repoURL + "/branch/" + branch
	}
	return ""
}
