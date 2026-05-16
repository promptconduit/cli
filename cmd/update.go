package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const githubReleasesAPI = "https://api.github.com/repos/promptconduit/cli/releases/latest"

var (
	updateCheckOnly bool
	updateYes       bool
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update PromptConduit to the latest version",
	Long: `Check for a newer release on GitHub and upgrade in place when possible.

If the binary was installed via Homebrew, this command runs:
    brew update && brew upgrade promptconduit

For other install methods (manual download, go install), this command prints
the manual upgrade instructions appropriate for your platform.`,
	RunE: runUpdate,
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheckOnly, "check", false, "Only check for updates, do not install")
	updateCmd.Flags().BoolVarP(&updateYes, "yes", "y", false, "Skip confirmation prompt")
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func runUpdate(cmd *cobra.Command, args []string) error {
	latest, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}

	current := strings.TrimPrefix(Version, "v")
	latestVer := strings.TrimPrefix(latest.TagName, "v")

	cmd.Printf("Current: v%s\n", current)
	cmd.Printf("Latest:  v%s\n", latestVer)

	if current == latestVer {
		cmd.Println("\nAlready on the latest version.")
		return nil
	}

	if current == "dev" {
		cmd.Println("\nRunning a dev build — skipping upgrade. Install a release with Homebrew or download from:")
		cmd.Println("  " + latest.HTMLURL)
		return nil
	}

	if updateCheckOnly {
		cmd.Printf("\nA new version is available: v%s\n", latestVer)
		cmd.Println("Run `promptconduit update` to install it.")
		return nil
	}

	method := detectInstallMethod()
	cmd.Printf("\nInstall method: %s\n", method)

	switch method {
	case "homebrew":
		return runHomebrewUpgrade(cmd)
	default:
		printManualInstructions(cmd, latest)
		return nil
	}
}

func fetchLatestRelease() (*githubRelease, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", githubReleasesAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "promptconduit-cli/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("empty tag_name in GitHub response")
	}
	return &rel, nil
}

// detectInstallMethod inspects the executable path to guess how the binary
// was installed. Returns "homebrew" or "other".
func detectInstallMethod() string {
	exe, err := os.Executable()
	if err != nil {
		return "other"
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	if strings.Contains(resolved, "/Cellar/promptconduit/") ||
		strings.Contains(resolved, "/homebrew/") ||
		strings.Contains(resolved, "/linuxbrew/") {
		return "homebrew"
	}
	return "other"
}

func runHomebrewUpgrade(cmd *cobra.Command) error {
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("brew not found in PATH")
	}

	if !updateYes {
		cmd.Print("\nRun `brew update && brew upgrade promptconduit`? [y/N] ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			cmd.Println("Aborted.")
			return nil
		}
	}

	cmd.Println("\n$ brew update")
	brewUpdate := exec.Command("brew", "update")
	brewUpdate.Stdout = cmd.OutOrStdout()
	brewUpdate.Stderr = cmd.ErrOrStderr()
	if err := brewUpdate.Run(); err != nil {
		return fmt.Errorf("brew update failed: %w", err)
	}

	cmd.Println("\n$ brew upgrade promptconduit")
	brewUpgrade := exec.Command("brew", "upgrade", "promptconduit")
	brewUpgrade.Stdout = cmd.OutOrStdout()
	brewUpgrade.Stderr = cmd.ErrOrStderr()
	if err := brewUpgrade.Run(); err != nil {
		return fmt.Errorf("brew upgrade failed: %w", err)
	}

	cmd.Println("\nUpgrade complete. Run `promptconduit version` to confirm.")
	return nil
}

func printManualInstructions(cmd *cobra.Command, latest *githubRelease) {
	cmd.Println("\nAutomatic upgrade is only supported for Homebrew installs.")
	cmd.Println("To upgrade manually:")
	cmd.Println()
	switch runtime.GOOS {
	case "darwin", "linux":
		cmd.Println("  # Homebrew (recommended)")
		cmd.Println("  brew update && brew upgrade promptconduit")
		cmd.Println()
		cmd.Println("  # Or download the latest release archive:")
		cmd.Println("  " + latest.HTMLURL)
	case "windows":
		cmd.Println("  Download the latest release archive:")
		cmd.Println("  " + latest.HTMLURL)
	default:
		cmd.Println("  " + latest.HTMLURL)
	}
}
