package extension

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Editor is a supported editor we can sideload the cost extension into via its
// `--install-extension` CLI (Cursor and VS Code share the same interface).
type Editor struct {
	Name    string // human label, e.g. "Cursor"
	Command string // CLI command name on PATH, e.g. "cursor"
	// appSuffix is the path to the editor's CLI *inside* its macOS .app bundle,
	// relative to an Applications dir. Used as a fallback when Command isn't on
	// PATH (common: the user never ran "Shell Command: Install 'cursor'").
	appSuffix string
}

// Cursor is the primary target — the realtime cost status bar lives in Cursor.
var Cursor = Editor{
	Name:      "Cursor",
	Command:   "cursor",
	appSuffix: "Cursor.app/Contents/Resources/app/bin/cursor",
}

// VSCode is supported too (identical --install-extension CLI).
var VSCode = Editor{
	Name:      "VS Code",
	Command:   "code",
	appSuffix: "Visual Studio Code.app/Contents/Resources/app/bin/code",
}

// resolveCLI returns the path to the editor's command-line launcher, or "" if it
// can't be found. It checks PATH first, then well-known macOS .app locations
// (/Applications and ~/Applications).
func (e Editor) resolveCLI() string {
	if p, err := exec.LookPath(e.Command); err == nil {
		return p
	}
	if runtime.GOOS == "darwin" && e.appSuffix != "" {
		roots := []string{"/Applications"}
		if home, err := os.UserHomeDir(); err == nil {
			roots = append(roots, filepath.Join(home, "Applications"))
		}
		for _, root := range roots {
			cand := filepath.Join(root, e.appSuffix)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
	}
	return ""
}

// InstallResult is the outcome of a sideload attempt.
type InstallResult struct {
	Editor    string // editor label
	Version   string // embedded extension version
	Installed bool   // true when the editor CLI accepted the .vsix
	CLIPath   string // resolved editor CLI used (when Installed)
	// SkippedReason is set (with Installed=false, err=nil) when the editor CLI
	// wasn't found — a normal "editor not installed / CLI not on PATH" case the
	// caller surfaces as a hint, not a failure.
	SkippedReason string
}

// Available reports whether the editor's CLI can be found, so callers can decide
// whether to attempt an install at all.
func (e Editor) Available() bool { return e.resolveCLI() != "" }

// Install sideloads the embedded cost extension into the given editor using its
// `--install-extension <vsix> --force` CLI (--force so it also updates an older
// already-installed copy).
//
// Best-effort: if the editor CLI can't be located it returns a result with
// SkippedReason set and a nil error, so wiring this into `install` never breaks
// the hook setup. A non-nil error means the CLI was found but the install
// command itself failed.
func Install(e Editor) (InstallResult, error) {
	// The interactive `install` command is user-driven and shouldn't impose an
	// arbitrary deadline, so it runs unbounded. The background reconcile path
	// uses InstallContext with a timeout instead.
	return InstallContext(context.Background(), e)
}

// InstallContext is Install with a caller-supplied context, so callers that run
// outside an interactive command (e.g. the post-upgrade reconcile in
// PersistentPreRun) can bound the editor subprocess and never hang the user's
// command if the editor CLI stalls.
func InstallContext(ctx context.Context, e Editor) (InstallResult, error) {
	res := InstallResult{Editor: e.Name}
	if v, err := Version(); err == nil {
		res.Version = v
	}

	cli := e.resolveCLI()
	if cli == "" {
		res.SkippedReason = fmt.Sprintf("%s command-line launcher not found", e.Name)
		return res, nil
	}
	res.CLIPath = cli

	vsixPath, cleanup, err := writeTempVSIX()
	if err != nil {
		return res, err
	}
	defer cleanup()

	out, err := exec.CommandContext(ctx, cli, "--install-extension", vsixPath, "--force").CombinedOutput()
	if err != nil {
		return res, fmt.Errorf("%s --install-extension failed: %w: %s",
			e.Command, err, strings.TrimSpace(string(out)))
	}
	res.Installed = true
	return res, nil
}

// writeTempVSIX writes the embedded .vsix to a temp file and returns its path
// plus a cleanup func to remove it.
func writeTempVSIX() (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "promptconduit-cost-*.vsix")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp vsix: %w", err)
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	if _, err := f.Write(vsix); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write temp vsix: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close temp vsix: %w", err)
	}
	return f.Name(), cleanup, nil
}
