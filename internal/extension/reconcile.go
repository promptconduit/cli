package extension

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/updater"
)

// ExtensionID is the editor extension identifier (<publisher>.<name>), used to
// read the installed version from `--list-extensions --show-versions`.
const ExtensionID = "promptconduit.promptconduit"

// Editor subprocesses (Cursor/VS Code CLIs) can be slow to spawn or hang, and
// reconcile runs inside PersistentPreRun, so every call is bounded — a stalled
// editor must never block the user's actual command.
const (
	listTimeout    = 10 * time.Second
	installTimeout = 90 * time.Second
)

// InstalledVersion returns the version of the cost extension currently installed
// in the editor, or "" when the editor CLI isn't found or the extension isn't
// installed. Bounded by listTimeout.
func (e Editor) InstalledVersion() (string, error) {
	cli := e.resolveCLI()
	if cli == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, cli, "--list-extensions", "--show-versions").CombinedOutput()
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

// reconcileAction is the decision Reconcile makes from availability + versions.
type reconcileAction int

const (
	actionSkipNoCLI reconcileAction = iota
	actionSkipNotInstalled
	actionSkipUpToDate
	actionUpdate
)

func (a reconcileAction) reason() string {
	switch a {
	case actionSkipNoCLI:
		return "editor CLI not found"
	case actionSkipNotInstalled:
		return "extension not installed"
	case actionSkipUpToDate:
		return "up to date"
	default:
		return ""
	}
}

// decideReconcile is the pure decision: given whether the editor CLI is
// available and the bundled vs installed versions, what should Reconcile do?
// Conservative — only an already-installed, strictly-older extension is updated.
//
// Note: version comparison requires plain MAJOR.MINOR.PATCH (updater.IsNewerVersion
// returns false for anything else), so the editor extension must keep using
// 3-part semver or reconcile will silently never fire.
func decideReconcile(available bool, bundled, installed string) reconcileAction {
	if !available {
		return actionSkipNoCLI
	}
	if installed == "" {
		return actionSkipNotInstalled
	}
	if !updater.IsNewerVersion(bundled, installed) {
		return actionSkipUpToDate
	}
	return actionUpdate
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
// without surprising the user. Best-effort: callers ignore errors. All editor
// subprocesses are bounded by a timeout so a stalled CLI can't block the caller.
func Reconcile(e Editor) (ReconcileResult, error) {
	res := ReconcileResult{Editor: e.Name}
	bundled, err := Version()
	if err != nil {
		return res, err
	}
	res.Bundled = bundled

	available := e.Available()
	installed := ""
	if available {
		if installed, err = e.InstalledVersion(); err != nil {
			return res, err
		}
	}
	res.Installed = installed

	action := decideReconcile(available, bundled, installed)
	if action != actionUpdate {
		res.Skipped = action.reason()
		return res, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	defer cancel()
	if _, err := InstallContext(ctx, e); err != nil {
		return res, err
	}
	res.Updated = true
	return res, nil
}
