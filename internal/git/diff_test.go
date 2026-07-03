package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDiffShortstat(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")

	// Clean tree.
	files, ins, del, ok := DiffShortstat(dir)
	if !ok || files != 0 || ins != 0 || del != 0 {
		t.Fatalf("clean tree = %d/%d/%d ok=%v, want zeros/true", files, ins, del, ok)
	}

	// Modify: one line changed becomes 1 insertion + 1 deletion, plus one added line.
	if err := os.WriteFile(file, []byte("one\nTWO\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, ins, del, ok = DiffShortstat(dir)
	if !ok || files != 1 || ins != 2 || del != 1 {
		t.Fatalf("dirty tree = %d/%d/%d ok=%v, want 1/2/1/true", files, ins, del, ok)
	}

	// Not a repo.
	if _, _, _, notRepoOK := DiffShortstat(t.TempDir()); notRepoOK {
		t.Fatal("non-repo must return ok=false")
	}
}
