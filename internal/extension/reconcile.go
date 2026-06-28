package extension

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/promptconduit/cli/internal/updater"
)

// ExtensionID is the editor extension identifier (<publisher>.<name>), used to
// read the installed version from `--list-extensions --show-versions`.
const ExtensionID = "promptconduit.promptconduit-cost"

// InstalledVersion returns the version of the cost extension currently installed
// in the editor, or "" when the editor CLI isn't found or the extension isn't
// installed.
func (e Editor) InstalledVersion() (string, error) {
	cli := e.resolveCLI()
	if cli == "" {
		return "", nil
	}
	out, err := exec.Command(cli, "--list-extensions", "--show-versions").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --list-extensions failed: %w: %s",
			e.Command, err, strings.TrimSpace(string(out)))
	}
	return parseInstalledVersion(string(out), ExtensionID), nil
}

// parseInstalledVersion extracts the version for id from `--show-versions`
// output, whose lines look like "publisher.name@1.2.3". Returns "" when the id
// isn't present.
func parseInstalledVersion(output, id string) string {
	prefix := id + "@"
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return v
		}
	}
	return ""
}

// ReconcileResult is the outcome of a Reconcile attempt.
type ReconcileResult struct {
	Editor    string // editor label
	Bundled   string // extension version embedded in this binary
	Installed string // version currently in the editor ("" if none)
	Updated   bool   // true when a newer bundle was re-sideloaded
	Skipped   string // why nothing happened (CLI missing / not installed / up to date)
}

// Reconcile updates the editor's installed cost extension to the bundled version
// when the bundle is newer. It is intentionally conservative:
//   - editor CLI not found            → skip
//   - extension not already installed → skip (don't force-install; respects a
//     user who declined the extension via `--no-extension`)
//   - installed version >= bundled    → skip
//
// Only an already-installed, older extension is upgraded (via
// `--install-extension --force`), so the extension tracks the CLI on upgrade
// without surprising the user. Best-effort: callers ignore errors.
func Reconcile(e Editor) (ReconcileResult, error) {
	res := ReconcileResult{Editor: e.Name}
	bundled, err := Version()
	if err != nil {
		return res, err
	}
	res.Bundled = bundled

	if !e.Available() {
		res.Skipped = "editor CLI not found"
		return res, nil
	}
	installed, err := e.InstalledVersion()
	if err != nil {
		return res, err
	}
	res.Installed = installed
	if installed == "" {
		res.Skipped = "extension not installed"
		return res, nil
	}
	if !updater.IsNewerVersion(bundled, installed) {
		res.Skipped = "up to date"
		return res, nil
	}

	if _, err := Install(e); err != nil {
		return res, err
	}
	res.Updated = true
	return res, nil
}
