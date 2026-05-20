package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"shipping-features", false},
		{"a", false},
		{"x9", false},
		{strings.Repeat("a", 64), false},
		{"", true},
		{strings.Repeat("a", 65), true},
		{"Shipping", true},
		{"my_skill", true},
		{"my.skill", true},
		{"my skill", true},
		{"my/skill", true},
		{"../escape", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.name)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateName(%q): want error, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateName(%q): %v", tc.name, err)
			}
		})
	}
}

func TestSkillPaths(t *testing.T) {
	base := "/tmp/x"
	if got := SkillDir(base, "foo"); got != "/tmp/x/foo" {
		t.Errorf("SkillDir = %q", got)
	}
	if got := SkillFile(base, "foo"); got != "/tmp/x/foo/SKILL.md" {
		t.Errorf("SkillFile = %q", got)
	}
}

// makeGitRepo creates a directory with a .git marker so DetectGitRoot
// returns it. We don't need a real repo.
func makeGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("seed .git: %v", err)
	}
	return root
}

func TestDetectGitRoot(t *testing.T) {
	root := makeGitRepo(t)
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}

	if got := DetectGitRoot(deep); got != root {
		t.Errorf("DetectGitRoot(deep) = %q, want %q", got, root)
	}
	if got := DetectGitRoot(root); got != root {
		t.Errorf("DetectGitRoot(root) = %q, want %q", got, root)
	}

	// Outside any repo (using TempDir which has no .git ancestor) should
	// eventually return "" — we can't easily test that without walking to
	// the real filesystem root, but an empty input is a fast no.
	if got := DetectGitRoot(""); got != "" {
		t.Errorf("DetectGitRoot(\"\") = %q, want empty", got)
	}
}

func TestResolveScope_ExplicitProject(t *testing.T) {
	root := makeGitRepo(t)
	scope, base, err := ResolveScope("any-repo", "project", root)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if scope != ScopeProject {
		t.Errorf("scope = %q, want project", scope)
	}
	want := filepath.Join(root, ".claude", "skills")
	if base != want {
		t.Errorf("base = %q, want %q", base, want)
	}
}

func TestResolveScope_ExplicitProject_NotInRepo(t *testing.T) {
	dir := t.TempDir() // no .git
	_, _, err := ResolveScope("any-repo", "project", dir)
	if err == nil {
		t.Error("expected error when --scope=project outside a repo, got nil")
	}
}

func TestResolveScope_ExplicitGlobal(t *testing.T) {
	scope, base, err := ResolveScope("any-repo", "global", "/anywhere")
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if scope != ScopeGlobal {
		t.Errorf("scope = %q, want global", scope)
	}
	if !strings.HasSuffix(base, filepath.Join(".claude", "skills")) {
		t.Errorf("base %q should end in .claude/skills", base)
	}
}

func TestResolveScope_Auto_RepoBoundInsideGit(t *testing.T) {
	root := makeGitRepo(t)
	scope, base, err := ResolveScope("some-repo", "", root)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if scope != ScopeProject {
		t.Errorf("scope = %q, want project (auto)", scope)
	}
	if base != filepath.Join(root, ".claude", "skills") {
		t.Errorf("base = %q", base)
	}
}

func TestResolveScope_Auto_GlobalSkillFallback(t *testing.T) {
	// Global-scoped skill (repoName empty) inside a repo → still global.
	root := makeGitRepo(t)
	scope, _, err := ResolveScope("", "", root)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if scope != ScopeGlobal {
		t.Errorf("scope = %q, want global", scope)
	}
}

func TestResolveScope_InvalidFlag(t *testing.T) {
	_, _, err := ResolveScope("any", "user", "/tmp")
	if err == nil {
		t.Error("expected error for invalid --scope value")
	}
}
