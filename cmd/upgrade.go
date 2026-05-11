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
	Long: `Check GitHub for a newer release and atomically replace the running binary.

Examples:
  promptconduit upgrade           # upgrade to the latest release (if newer)
  promptconduit upgrade --check   # only check; do not download or replace
  promptconduit upgrade --force   # download and replace even if up to date

Homebrew users should run "brew upgrade promptconduit" instead — this command
will refuse to replace a binary it can't write to.`,
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

	// Make sure the destination is writable before pulling a release down.
	if err := canReplaceSelf(); err != nil {
		cmd.Printf("Cannot self-upgrade: %v\n", err)
		cmd.Println("If you installed via Homebrew, run: brew upgrade promptconduit")
		return err
	}

	// Take an advisory lock so two concurrent invocations don't race on
	// the same binary swap.
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
