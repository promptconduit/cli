package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/updater"
	"github.com/spf13/cobra"
)

var (
	upgradeCheckOnly bool
	upgradeForce     bool
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade promptconduit to the latest release",
	Long: `Check GitHub for a newer release and upgrade in place.

Examples:
  promptconduit upgrade           # upgrade to the latest release (if newer)
  promptconduit upgrade --check   # only check; do not download or replace
  promptconduit upgrade --force   # download and replace even if up to date

Homebrew-managed installs are detected automatically and upgraded via
"brew upgrade promptconduit" so Homebrew's metadata stays in sync; all other
installs are upgraded by atomically replacing the running binary.`,
	RunE: runUpgrade,
}

func init() {
	upgradeCmd.Flags().BoolVar(&upgradeCheckOnly, "check", false, "only check for a newer release; do not download")
	upgradeCmd.Flags().BoolVar(&upgradeForce, "force", false, "reinstall even when already on the latest version")
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	if Version == "dev" && !upgradeForce {
		cmd.Println("Running a development build (version=dev); skipping upgrade.")
		cmd.Println("Use --force to reinstall anyway, or build with a release tag.")
		return nil
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	cmd.Printf("Checking %s for releases newer than %s...\n", updater.Repo, Version)
	rel, newer, err := updater.CheckLatest(ctx, Version)
	if err != nil {
		return fmt.Errorf("check failed: %w", err)
	}

	cmd.Printf("Latest release: %s\n", rel.TagName)

	cachePath := filepath.Join(client.ConfigDir(), updater.CacheFileName)
	_ = updater.SaveCache(cachePath, &updater.CheckResult{
		CheckedAt:      time.Now(),
		LatestVersion:  rel.TagName,
		CurrentVersion: Version,
		ReleaseURL:     rel.HTMLURL,
	})

	if !newer && !upgradeForce {
		cmd.Println("Already up to date.")
		return nil
	}
	if upgradeCheckOnly {
		cmd.Printf("Newer release available: %s\n", rel.HTMLURL)
		cmd.Println("Run `promptconduit upgrade` to install.")
		return nil
	}

	// Take an advisory lock so two concurrent invocations (e.g. a manual run and
	// the detached background upgrade) don't both upgrade at once. Covers the
	// brew and self-replace paths alike.
	lockPath := filepath.Join(client.ConfigDir(), updater.LockFileName)
	release, err := updater.Lock(lockPath)
	defer release()
	if err != nil {
		if errors.Is(err, updater.ErrLocked) {
			cmd.Println("Another upgrade is already in progress; skipping.")
			return nil
		}
		return fmt.Errorf("acquire upgrade lock: %w", err)
	}

	// Homebrew-managed installs must upgrade via brew, not by self-replacing the
	// Cellar binary (which would desync brew's metadata from the file on disk).
	if exe, err := os.Executable(); err == nil && updater.IsHomebrewManaged(exe) {
		cmd.Println("Homebrew-managed install detected — upgrading via `brew upgrade promptconduit`...")
		bctx, bcancel := context.WithTimeout(cmd.Context(), updater.DownloadTimeout)
		defer bcancel()
		out, err := updater.BrewUpgrade(bctx, updater.HomebrewFormula)
		if out != "" {
			cmd.Println(out)
		}
		if err != nil {
			return fmt.Errorf("brew upgrade failed: %w", err)
		}
		cmd.Printf("Upgraded via Homebrew (target %s). The bundled editor extension updates on next run.\n", rel.TagName)
		return nil
	}

	// Make sure the destination is writable before pulling a release down.
	if err := canReplaceSelf(); err != nil {
		cmd.Printf("Cannot self-upgrade: %v\n", err)
		cmd.Println("If you installed via Homebrew, run: brew upgrade promptconduit")
		return err
	}

	archive, checksums, err := updater.AssetForCurrent(rel)
	if err != nil {
		return fmt.Errorf("no matching release asset: %w", err)
	}

	cmd.Printf("Downloading %s...\n", archive.Name)
	dlCtx, dlCancel := context.WithTimeout(cmd.Context(), updater.DownloadTimeout)
	defer dlCancel()

	replaced, err := updater.Apply(dlCtx, archive, checksums)
	if err != nil {
		return fmt.Errorf("upgrade failed: %w", err)
	}

	cmd.Printf("Upgraded to %s (replaced %s)\n", rel.TagName, replaced)
	return nil
}

// canReplaceSelf returns nil when the directory containing the running
// binary is writable by the current user. We check the directory rather
// than the file because os.Rename writes a new inode into the directory.
func canReplaceSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	probe, err := os.CreateTemp(dir, ".promptconduit-write-probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return nil
}
