package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// nameRule matches Anthropic's SKILL.md name validation: lowercase letters,
// digits, hyphens, 1–64 chars. Anything else gets rejected before we touch
// the filesystem.
var nameRule = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// ValidateName returns an error if name doesn't conform to the Anthropic
// skill-name rule. Caller is expected to short-circuit before any I/O.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name is empty")
	}
	if !nameRule.MatchString(name) {
		return fmt.Errorf("skill name %q must match %s", name, nameRule.String())
	}
	return nil
}

// ResolveScope decides whether a skill should land in the project tree or
// the user's home. Rules, applied in order:
//
//  1. If flag is "project" or "global", honor it.
//  2. If repoName is set AND cwd is inside a git repo, use project.
//  3. Otherwise, global.
//
// Returns the chosen scope and the base dir for that scope (the parent of
// the skill bundle directory).
func ResolveScope(repoName, flag, cwd string) (Scope, string, error) {
	switch flag {
	case string(ScopeProject):
		gitRoot := DetectGitRoot(cwd)
		if gitRoot == "" {
			return "", "", fmt.Errorf("--scope=project but %s is not inside a git repo", cwd)
		}
		return ScopeProject, filepath.Join(gitRoot, ".claude", "skills"), nil
	case string(ScopeGlobal):
		base, err := globalBase()
		if err != nil {
			return "", "", err
		}
		return ScopeGlobal, base, nil
	case "":
		// Auto: prefer project if we're in a repo AND the skill is bound to
		// one; otherwise global.
		if repoName != "" {
			if gitRoot := DetectGitRoot(cwd); gitRoot != "" {
				return ScopeProject, filepath.Join(gitRoot, ".claude", "skills"), nil
			}
		}
		base, err := globalBase()
		if err != nil {
			return "", "", err
		}
		return ScopeGlobal, base, nil
	default:
		return "", "", fmt.Errorf("invalid --scope %q (want project|global)", flag)
	}
}

// SkillDir returns the directory for a named skill under the given base.
// Layout: <base>/<name>/
func SkillDir(base, name string) string {
	return filepath.Join(base, name)
}

// SkillFile returns the canonical SKILL.md path for a named skill.
func SkillFile(base, name string) string {
	return filepath.Join(SkillDir(base, name), "SKILL.md")
}

// DetectGitRoot walks up from dir looking for a .git entry. Returns "" if
// none found (e.g. caller is outside any repo).
func DetectGitRoot(dir string) string {
	if dir == "" {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func globalBase() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "skills"), nil
}
