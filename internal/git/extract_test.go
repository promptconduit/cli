package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseStatusV2(t *testing.T) {
	out := "# branch.oid 4d78170ec02cc69809a7de76699aa8bdb34f3ed0\n" +
		"# branch.head feat/x\n" +
		"# branch.upstream origin/feat/x\n" +
		"# branch.ab +3 -2\n" +
		"1 M. N... 100644 100644 100644 aaa bbb staged.go\n" +
		"1 .M N... 100644 100644 100644 aaa bbb unstaged.go\n" +
		"1 MM N... 100644 100644 100644 aaa bbb both.go\n" +
		"2 R. N... 100644 100644 100644 aaa bbb R100 new.go\told.go\n" +
		"u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict.go\n" +
		"? untracked.txt\n" +
		"? dir/\n"
	st := parseStatusV2(out)
	want := statusV2{
		oid: "4d78170ec02cc69809a7de76699aa8bdb34f3ed0", branch: "feat/x",
		ahead: 3, behind: 2, staged: 4, unstaged: 3, untracked: 2,
	}
	if st != want {
		t.Errorf("parseStatusV2 = %+v, want %+v", st, want)
	}

	// Unborn branch, detached HEAD, no upstream.
	if st := parseStatusV2("# branch.oid (initial)\n# branch.head main\n"); st.oid != "" || st.branch != "main" {
		t.Errorf("unborn: %+v", st)
	}
	if st := parseStatusV2("# branch.oid abc\n# branch.head (detached)\n"); st.branch != "" || st.oid != "abc" {
		t.Errorf("detached: %+v", st)
	}
	if st := parseStatusV2(""); st != (statusV2{}) {
		t.Errorf("empty: %+v", st)
	}
}

// ExtractContext against a real repo with an upstream: every field the vcs
// enrichment relies on is populated from the reduced set of git calls.
func TestExtractContextFields(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git(t, root, "init", "-q", "--bare", "-b", "main", remote)
	git(t, root, "clone", "-q", remote, repo)
	write("a.txt", "a")
	write("b.txt", "b")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "first commit")
	git(t, repo, "push", "-q", "origin", "main")
	git(t, repo, "remote", "set-head", "origin", "main")
	write("c.txt", "c")
	git(t, repo, "add", "c.txt")
	git(t, repo, "commit", "-q", "-m", "second commit")

	write("a.txt", "staged")
	git(t, repo, "add", "a.txt")
	write("b.txt", "unstaged")
	write("new.txt", "untracked")

	ctx := ExtractContext(repo)
	if ctx == nil {
		t.Fatal("ExtractContext returned nil for a repo")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Branch", ctx.Branch, "main"},
		{"IsDetachedHead", ctx.IsDetachedHead, false},
		{"CommitMessage", ctx.CommitMessage, "second commit"},
		{"CommitAuthor", ctx.CommitAuthor, "t"},
		{"StagedCount", ctx.StagedCount, 1},
		{"UnstagedCount", ctx.UnstagedCount, 1},
		{"UntrackedCount", ctx.UntrackedCount, 1},
		{"IsDirty", ctx.IsDirty, true},
		{"AheadCount", ctx.AheadCount, 1},
		{"BehindCount", ctx.BehindCount, 0},
		{"RemoteURL", ctx.RemoteURL, remote},
		{"RepoName", ctx.RepoName, "remote"},
		{"DefaultBranch", ctx.DefaultBranch, "main"},
		{"IsWorktree", ctx.IsWorktree, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(ctx.CommitHash) != 40 {
		t.Errorf("CommitHash = %q, want a full sha", ctx.CommitHash)
	}
	if got := DefaultBranch(repo); got != "main" {
		t.Errorf("DefaultBranch = %q, want main", got)
	}

	// Detached HEAD.
	git(t, repo, "checkout", "-q", "--detach")
	if ctx := ExtractContext(repo); ctx == nil || !ctx.IsDetachedHead || ctx.Branch != "" {
		t.Errorf("detached: %+v", ctx)
	}

	// Not a repo.
	if ctx := ExtractContext(t.TempDir()); ctx != nil {
		t.Errorf("non-repo: got %+v, want nil", ctx)
	}
}

// An unborn branch (no commits) still reports the repo and branch.
func TestExtractContextUnborn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	ctx := ExtractContext(repo)
	if ctx == nil {
		t.Fatal("ExtractContext returned nil for an unborn repo")
	}
	if ctx.Branch != "main" || ctx.CommitHash != "" || ctx.CommitMessage != "" || ctx.DefaultBranch != "" {
		t.Errorf("unborn: %+v", ctx)
	}
}
