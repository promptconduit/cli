package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	skillspkg "github.com/promptconduit/cli/internal/skills"
	"github.com/spf13/cobra"
)

// Flags. Reused state variables would collide with cmd/skills.go, so these
// are install/uninstall-specific.
var (
	skillsInstallAll       bool
	skillsInstallScope     string
	skillsInstallForce     bool
	skillsUninstallAll     bool
	skillsUninstallForce   bool
)

// Exit codes the commands can return through cobra. We surface them via
// fmt.Errorf — main.go currently exits 1 on any RunE error; richer exit
// codes would require a wrapper around Execute(). Out of scope for Phase 1.

// ============================================================================
// install
// ============================================================================

var skillsInstallCmd = &cobra.Command{
	Use:   "install [name]",
	Short: "Install a skill into .claude/skills/<name>/SKILL.md",
	Long: `Download a skill from the platform and write it to the Claude Code
skills directory. Tracks the install in a local manifest so it can be
safely uninstalled later.

Examples:
  promptconduit skills install shipping-features
  promptconduit skills install shipping-features --scope project
  promptconduit skills install --all
  promptconduit skills install shipping-features --force   # overwrite local edits`,
	RunE: runSkillsInstall,
}

// ============================================================================
// uninstall
// ============================================================================

var skillsUninstallCmd = &cobra.Command{
	Use:   "uninstall [name]",
	Short: "Remove a previously installed skill",
	Long: `Remove a skill that was installed via 'promptconduit skills install'.

Refuses if the file on disk has been modified since install (sha mismatch),
unless --force is given. Skills not tracked by the local manifest are
never touched.

Examples:
  promptconduit skills uninstall shipping-features
  promptconduit skills uninstall --all
  promptconduit skills uninstall shipping-features --force`,
	RunE: runSkillsUninstall,
}

// ============================================================================
// approve / reject  (server-side state via PATCH /v1/skills/:id)
// ============================================================================

var skillsApproveCmd = &cobra.Command{
	Use:   "approve [name]",
	Short: "Mark a skill as approved on the platform (the Ready tab)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runApproveOrReject(args[0], true)
	},
}

var skillsRejectCmd = &cobra.Command{
	Use:   "reject [name]",
	Short: "Mark a skill as rejected on the platform (the Removed tab)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runApproveOrReject(args[0], false)
	},
}

// ============================================================================
// implementations
// ============================================================================

func runSkillsInstall(cmd *cobra.Command, args []string) error {
	if skillsInstallAll && len(args) > 0 {
		return errors.New("--all is mutually exclusive with a positional skill name")
	}
	if !skillsInstallAll && len(args) != 1 {
		return errors.New("expected a skill name or --all")
	}

	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return errors.New(`API key not configured. Run: promptconduit config set --api-key="your-key"`)
	}

	apiClient := client.NewClient(cfg, Version)

	manifest, err := skillspkg.Load(client.ConfigDir())
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	// Resolve which skills to install: either the named one, or every
	// approved skill on the server.
	var targets []map[string]interface{}
	if skillsInstallAll {
		resp := apiClient.GetSkills("true", "", 100, "")
		if !resp.Success {
			return fmt.Errorf("fetch approved skills: %s", resp.Error)
		}
		list, _ := resp.Data["skills"].([]interface{})
		for _, s := range list {
			if m, ok := s.(map[string]interface{}); ok {
				targets = append(targets, m)
			}
		}
		if len(targets) == 0 {
			cmd.Println("No approved skills to install.")
			return nil
		}
	} else {
		name := args[0]
		if err := skillspkg.ValidateName(name); err != nil {
			return err
		}
		skill, err := findSkillByName(apiClient, name)
		if err != nil {
			return err
		}
		targets = []map[string]interface{}{skill}
	}

	installed, skipped, failed := 0, 0, 0
	for _, skill := range targets {
		name, _ := skill["name"].(string)
		switch err := installOne(cmd, apiClient, manifest, skill); {
		case err == nil:
			installed++
		case errors.Is(err, errAlreadyCurrent):
			cmd.Printf("  [SKIP] %s — already up to date\n", name)
			skipped++
		case errors.Is(err, errAbortLocalChanges):
			cmd.Printf("  [SKIP] %s — local modifications, use --force to overwrite\n", name)
			skipped++
		default:
			cmd.Printf("  [FAIL] %s: %v\n", name, err)
			failed++
		}
	}

	// Persist whatever we managed to record. Each installOne already
	// updates the in-memory manifest; we save once at the end so a
	// crash mid-batch doesn't leave a half-recorded state.
	if err := skillspkg.Save(client.ConfigDir(), manifest); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}

	cmd.Printf("\n%d installed, %d skipped, %d failed\n", installed, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d skill(s) failed to install", failed)
	}
	return nil
}

// Sentinel errors for non-fatal install outcomes that the loop above
// presents differently from real failures.
var (
	errAlreadyCurrent    = errors.New("already up to date")
	errAbortLocalChanges = errors.New("local modifications present")
)

func installOne(cmd *cobra.Command, apiClient *client.Client, manifest *skillspkg.Manifest, skill map[string]interface{}) error {
	id, _ := skill["id"].(string)
	name, _ := skill["name"].(string)
	repoName, _ := skill["repo_name"].(string)
	if id == "" || name == "" {
		return errors.New("skill record missing id or name")
	}
	if err := skillspkg.ValidateName(name); err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	scope, base, err := skillspkg.ResolveScope(repoName, skillsInstallScope, cwd)
	if err != nil {
		return err
	}

	// Fetch the canonical SKILL.md body from the platform.
	content, err := apiClient.GetSkillCommandFile(id)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	platformSHA := skillspkg.HashContent([]byte(content))

	target := skillspkg.SkillFile(base, name)

	// Idempotency check: if we already track this skill at the same sha,
	// the file on disk almost certainly hasn't drifted — fast no-op.
	if existing := manifest.Find(name); existing != nil {
		if existing.PlatformSHA256 == platformSHA && fileExistsAtSha(target, platformSHA) {
			return errAlreadyCurrent
		}
		// Drifted. Refuse unless --force.
		if !skillsInstallForce && fileExists(target) {
			diskSHA, _ := skillspkg.HashFile(target)
			if diskSHA != "" && diskSHA != existing.PlatformSHA256 {
				if !confirm(fmt.Sprintf("%s has local modifications — overwrite?", name)) {
					return errAbortLocalChanges
				}
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	if err := writeAtomic(target, []byte(content)); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}

	manifest.Add(skillspkg.Entry{
		ID:             id,
		Name:           name,
		Scope:          scope,
		InstalledAt:    time.Now().UTC(),
		PlatformSHA256: platformSHA,
		Files: []skillspkg.File{
			{Path: target, SHA256: platformSHA},
		},
	})

	cmd.Printf("  [OK]   %s → %s\n", name, target)
	return nil
}

func runSkillsUninstall(cmd *cobra.Command, args []string) error {
	if skillsUninstallAll && len(args) > 0 {
		return errors.New("--all is mutually exclusive with a positional skill name")
	}
	if !skillsUninstallAll && len(args) != 1 {
		return errors.New("expected a skill name or --all")
	}

	manifest, err := skillspkg.Load(client.ConfigDir())
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}

	var names []string
	if skillsUninstallAll {
		for _, e := range manifest.Skills {
			names = append(names, e.Name)
		}
		if len(names) == 0 {
			cmd.Println("No PromptConduit-installed skills to uninstall.")
			return nil
		}
	} else {
		names = []string{args[0]}
	}

	removed, skipped, failed := 0, 0, 0
	for _, name := range names {
		switch err := uninstallOne(cmd, manifest, name); {
		case err == nil:
			removed++
		case errors.Is(err, errNotTracked):
			cmd.Printf("  [SKIP] %s — not tracked by promptconduit\n", name)
			skipped++
		case errors.Is(err, errAbortLocalChanges):
			cmd.Printf("  [SKIP] %s — local modifications, use --force\n", name)
			skipped++
		default:
			cmd.Printf("  [FAIL] %s: %v\n", name, err)
			failed++
		}
	}

	if err := skillspkg.Save(client.ConfigDir(), manifest); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}

	cmd.Printf("\n%d removed, %d skipped, %d failed\n", removed, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d skill(s) failed to uninstall", failed)
	}
	return nil
}

var errNotTracked = errors.New("not tracked by manifest")

func uninstallOne(cmd *cobra.Command, manifest *skillspkg.Manifest, name string) error {
	entry := manifest.Find(name)
	if entry == nil {
		return errNotTracked
	}

	for _, f := range entry.Files {
		// Hand-edit check (skip the sha verification with --force).
		if !skillsUninstallForce && fileExists(f.Path) {
			onDisk, _ := skillspkg.HashFile(f.Path)
			if onDisk != "" && onDisk != f.SHA256 {
				return errAbortLocalChanges
			}
		}
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", f.Path, err)
		}
		// Best-effort: clean up the skill's parent dir if empty.
		_ = removeEmptyDir(filepath.Dir(f.Path))
	}

	manifest.Remove(name)
	cmd.Printf("  [OK]   %s\n", name)
	return nil
}

func runApproveOrReject(name string, approve bool) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return errors.New(`API key not configured. Run: promptconduit config set --api-key="your-key"`)
	}
	apiClient := client.NewClient(cfg, Version)
	skill, err := findSkillByName(apiClient, name)
	if err != nil {
		return err
	}
	id, _ := skill["id"].(string)
	if id == "" {
		return fmt.Errorf("skill %q has no id", name)
	}
	resp := apiClient.ApproveSkill(id, approve)
	if !resp.Success {
		verb := "approve"
		if !approve {
			verb = "reject"
		}
		return fmt.Errorf("%s %s: %s", verb, name, resp.Error)
	}
	verb := "approved"
	if !approve {
		verb = "rejected"
	}
	fmt.Printf("%s %s\n", verb, name)
	return nil
}

// ============================================================================
// helpers
// ============================================================================

// findSkillByName lists up to 100 skills (any approval state) and returns
// the first one whose name matches. Returns a structured error when not
// found. Used by install + approve + reject.
func findSkillByName(c *client.Client, name string) (map[string]interface{}, error) {
	resp := c.GetSkills("", "", 100, "")
	if !resp.Success {
		return nil, fmt.Errorf("list skills: %s", resp.Error)
	}
	list, _ := resp.Data["skills"].([]interface{})
	for _, s := range list {
		m, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _ := m["name"].(string); n == name {
			return m, nil
		}
	}
	return nil, fmt.Errorf("skill %q not found on the platform", name)
}

// writeAtomic writes data to path via tempfile + rename in the same dir.
// Same pattern as internal/skills/manifest.go's Save, but for arbitrary
// content. Same dir is important for atomic rename across filesystems.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pc-skill-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Ensure 0644 explicitly — CreateTemp gives 0600.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileExistsAtSha(path, want string) bool {
	got, err := skillspkg.HashFile(path)
	return err == nil && got == want
}

// removeEmptyDir removes dir only if it's empty. Used to keep
// ~/.claude/skills/<name>/ from lingering after we remove its SKILL.md.
func removeEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return nil
	}
	return os.Remove(dir)
}

// confirm prompts the user [Y/n]. Defaults to Y. On non-TTY (CI, piped
// input) it returns false so the install/uninstall is treated as
// "needs --force" rather than silently overwriting.
func confirm(prompt string) bool {
	if !stdinIsTerminal() {
		return false
	}
	fmt.Printf("%s [Y/n] ", prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false
		}
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "" || answer == "y" || answer == "yes"
}

// stdinIsTerminal reports whether stdin is a character device (e.g. a tty).
// Stdlib-only check that avoids dragging in golang.org/x/term and its
// Go-toolchain floor.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
