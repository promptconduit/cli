package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A main checkout is not a worktree; a linked worktree is, and its path is
// reported. This is the signal coaching relies on for "ran in a worktree".
func TestDetectWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	main := filepath.Join(root, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}

	git(t, main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, main, "add", ".")
	git(t, main, "commit", "-q", "-m", "init")

	// Main checkout: not a worktree.
	if isWt, _ := detectWorktree(main); isWt {
		t.Errorf("main checkout reported as worktree")
	}
	if ctx := ExtractContext(main); ctx == nil || ctx.IsWorktree {
		t.Errorf("ExtractContext(main).IsWorktree = true, want false")
	}

	// Linked worktree: detected, with its top-level path.
	wt := filepath.Join(root, "wt")
	git(t, main, "worktree", "add", "-q", "-b", "feat", wt)

	isWt, path := detectWorktree(wt)
	if !isWt {
		t.Fatalf("linked worktree not detected")
	}
	if filepath.Base(path) != "wt" {
		t.Errorf("worktree path = %q, want .../wt", path)
	}
	ctx := ExtractContext(wt)
	if ctx == nil || !ctx.IsWorktree {
		t.Fatalf("ExtractContext(wt).IsWorktree = false, want true")
	}
	if ctx.WorktreePath == "" {
		t.Errorf("ExtractContext(wt).WorktreePath empty")
	}
}
