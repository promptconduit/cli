package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/extension"
	"github.com/promptconduit/cli/internal/updater"
	"github.com/spf13/cobra"
)

var (
	// Version is set at build time via ldflags
	Version = "dev"
)

var rootCmd = &cobra.Command{
	Use:   "promptconduit",
	Short: "PromptConduit CLI - Capture AI assistant events",
	Long: `PromptConduit captures events from AI coding assistants and sends them to
the PromptConduit API for analysis and insights.

Supported tools:
  - Claude Code
  - Cursor
  - Gemini CLI

Get started:
  1. Set your API key: promptconduit config set --api-key="your-key"
  2. Install hooks: promptconduit install claude-code
  3. Use your AI assistant as normal - events are captured automatically`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		maybeBackgroundUpdateCheck(cmd)
	},
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(testCmd)
	rootCmd.AddCommand(hookCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(insightsCmd)
	rootCmd.AddCommand(skillsCmd)
	rootCmd.AddCommand(debugCmd)
	rootCmd.AddCommand(upgradeCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(collectCmd)
	rootCmd.AddCommand(logsCmd)
	rootCmd.AddCommand(eventsCmd)
	rootCmd.AddCommand(costCmd)
	rootCmd.AddCommand(pruneCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Printf("promptconduit %s\n", Version)
	},
}

// maybeBackgroundUpdateCheck performs a once-per-day check against GitHub
// releases. It never blocks the foreground command for more than the
// HTTP timeout (5s), and when auto-update is enabled it spawns a detached
// `promptconduit upgrade` subprocess so the swap happens in the background.
//
// Skipped for:
//   - the `hook` subcommand (runs per-event; must stay fast)
//   - the `upgrade` subcommand (it checks itself)
//   - dev builds (Version=="dev"): nothing to compare against
//   - when we're already a spawned child of another check
func maybeBackgroundUpdateCheck(cmd *cobra.Command) {
	// Clean up any leftover .old binary from a prior Windows upgrade.
	updater.CleanupOldBinary()

	if Version == "dev" {
		return
	}
	if skipUpdateCheckFor(cmd) {
		return
	}

	cfg := client.LoadConfig()

	cachePath := filepath.Join(client.ConfigDir(), updater.CacheFileName)

	// If a previous run upgraded us, the cache's recorded CurrentVersion no
	// longer matches the running binary — surface a one-time success notice
	// and rewrite the cache so we don't show it again. Gated on a real
	// forward step so a downgrade (e.g. `brew install --version` older
	// build) doesn't print a misleading "upgraded" line.
	if cached, _ := updater.LoadCache(cachePath); cached != nil && cached.DetectUpgrade(Version) {
		notifyUpgraded(cmd, cached.CurrentVersion, Version, cached.ReleaseURL)
		cached.CurrentVersion = Version
		_ = updater.SaveCache(cachePath, cached)
		// The binary just changed (self-replace or brew), so the embedded
		// extension .vsix may be newer than what's installed in the editor.
		// This is the one moment we know they can differ — reconcile now,
		// unless the user opted out of auto-update.
		if !cfg.DisableAutoUpdate {
			reconcileCostExtension(cmd)
		}
	}

	if !updater.ShouldCheck(cachePath, Version, updater.CheckTTL) {
		// Use the cached result to still nudge the user if a known
		// upgrade has been pending since the last check.
		if cached, _ := updater.LoadCache(cachePath); cached != nil && cached.IsNewer() {
			notifyNewer(cmd, cached.LatestVersion, cached.ReleaseURL, cfg.DisableAutoUpdate)
			if !cfg.DisableAutoUpdate {
				spawnBackgroundUpgrade()
			}
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), updater.DefaultTimeout)
	defer cancel()

	rel, newer, err := updater.CheckLatest(ctx, Version)
	if err != nil {
		// Network hiccup or rate limit — silent failure, try again tomorrow.
		return
	}

	result := &updater.CheckResult{
		CheckedAt:      time.Now(),
		LatestVersion:  rel.TagName,
		CurrentVersion: Version,
		ReleaseURL:     rel.HTMLURL,
	}
	_ = updater.SaveCache(cachePath, result)

	if !newer {
		return
	}

	notifyNewer(cmd, rel.TagName, rel.HTMLURL, cfg.DisableAutoUpdate)
	if !cfg.DisableAutoUpdate {
		spawnBackgroundUpgrade()
	}
}

func notifyNewer(cmd *cobra.Command, latest, releaseURL string, disabled bool) {
	hint := "run `promptconduit upgrade` to install"
	if !disabled {
		hint = "downloading in background; next invocation will use the new version"
	}
	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "promptconduit: %s available (you have %s) — %s\n", latest, Version, hint)
	if releaseURL != "" {
		_, _ = fmt.Fprintf(w, "  release notes: %s\n", releaseURL)
	}
}

// reconcileCostExtension brings the editor's installed cost extension up to the
// version bundled in this (freshly upgraded) binary. It runs once — right after
// a CLI upgrade is first detected — because that's the only moment the embedded
// .vsix is known to differ from what's installed. Best-effort and quiet: it
// only prints when it actually updated something, and never blocks or fails the
// user's command (Reconcile no-ops when the editor or extension isn't present).
func reconcileCostExtension(cmd *cobra.Command) {
	for _, e := range []extension.Editor{extension.Cursor, extension.VSCode} {
		res, err := extension.Reconcile(e)
		if err != nil {
			continue
		}
		if res.Updated {
			// Drop a marker the running extension watches so it can offer a
			// one-click reload. Best-effort — the terminal notice below is the
			// fallback for anyone who doesn't see the in-editor toast.
			_ = extension.WriteUpdateMarker(res.Bundled, res.Editor)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"promptconduit: updated the %s cost extension to v%s\n"+
					"  → Reload %s to apply: press Cmd/Ctrl+R, or run \"Developer: Reload Window\".\n"+
					"    A full restart isn't needed — and would close terminals running Claude Code.\n",
				res.Editor, res.Bundled, res.Editor)
		}
	}
}

func notifyUpgraded(cmd *cobra.Command, from, to, releaseURL string) {
	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "promptconduit: upgraded %s → %s\n", from, to)
	if releaseURL != "" {
		_, _ = fmt.Fprintf(w, "  release notes: %s\n", releaseURL)
	}
}

// spawnBackgroundUpgrade launches `promptconduit upgrade` as a detached
// subprocess. The parent returns immediately; the swap completes after the
// current invocation is done. Best-effort: any errors are swallowed.
func spawnBackgroundUpgrade() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(exe, "upgrade")
	// Don't inherit the parent's stdio — the user is about to see other
	// output from their actual command.
	c.Stdin = nil
	c.Stdout = nil
	c.Stderr = nil
	// Mark this invocation so the spawned child can detect it and skip its
	// own background check (we already checked).
	c.Env = append(os.Environ(), "PROMPTCONDUIT_AUTO_UPDATE_CHILD=1")
	if err := c.Start(); err != nil {
		return
	}
	if c.Process != nil {
		_ = c.Process.Release()
	}
}

// skipUpdateCheckFor returns true when the current command must not pay
// the cost of (or output) an update check. Hooks run many times per
// session and must stay fast; upgrade does its own checking; long-running
// loops shouldn't be interrupted with an update banner.
func skipUpdateCheckFor(cmd *cobra.Command) bool {
	if os.Getenv("PROMPTCONDUIT_AUTO_UPDATE_CHILD") == "1" {
		return true
	}
	name := commandPathRoot(cmd)
	switch name {
	case "hook", "upgrade", "watch", "collect", "cost", "sessions", "resume":
		// sessions/resume are called programmatically (the editor extension's
		// auto-restore) and resume execs straight into claude — keep them quiet
		// and fast, no update banner.
		return true
	}
	return false
}

// commandPathRoot returns the first subcommand under "promptconduit" for
// the given cobra command — e.g. for `promptconduit config show` it
// returns "config".
func commandPathRoot(cmd *cobra.Command) string {
	path := cmd.CommandPath() // "promptconduit config show"
	parts := strings.Fields(path)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}
