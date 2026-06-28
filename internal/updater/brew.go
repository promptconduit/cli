package updater

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

// HomebrewFormula is the tap formula name passed to `brew upgrade`.
const HomebrewFormula = "promptconduit"

// IsHomebrewManaged reports whether the binary at exePath is managed by
// Homebrew — i.e. `brew` is installed and the resolved binary lives under a
// Homebrew Cellar/prefix. When true, the upgrade flow delegates to
// `brew upgrade` instead of self-replacing the Cellar file. Self-replacing a
// brew-managed binary leaves Homebrew's metadata pointing at the old version
// while the file on disk is newer — the drift that results from two update
// channels fighting.
func IsHomebrewManaged(exePath string) bool {
	brew, err := exec.LookPath("brew")
	if err != nil {
		return false
	}

	resolved := exePath
	if r, err := filepath.EvalSymlinks(exePath); err == nil {
		resolved = r
	}

	// Standard case: the binary lives under a `Cellar` directory.
	if isUnderCellar(resolved) {
		return true
	}

	// Fallback for non-standard prefixes: ask brew where it installs.
	if out, err := exec.Command(brew, "--prefix").Output(); err == nil {
		if prefix := strings.TrimSpace(string(out)); prefix != "" && isUnderDir(resolved, prefix) {
			return true
		}
	}
	return false
}

// isUnderCellar is the pure path check: true when path traverses a `Cellar`
// directory component (Homebrew's standard install location).
func isUnderCellar(path string) bool {
	sep := string(filepath.Separator)
	return strings.Contains(path, sep+"Cellar"+sep)
}

// isUnderDir reports whether path is dir itself or nested within it.
func isUnderDir(path, dir string) bool {
	sep := string(filepath.Separator)
	dir = strings.TrimRight(dir, sep)
	return path == dir || strings.HasPrefix(path, dir+sep)
}

// BrewUpgrade runs `brew upgrade <formula>` and returns its combined output for
// surfacing to the user. Homebrew auto-refreshes its formula index before
// upgrading (unless HOMEBREW_NO_AUTO_UPDATE is set), so a freshly published
// release is picked up without a separate `brew update`.
func BrewUpgrade(ctx context.Context, formula string) (string, error) {
	brew, err := exec.LookPath("brew")
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, brew, "upgrade", formula).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// IsNewerVersion reports whether semver a is strictly newer than b. Inputs that
// don't parse yield false (treated as "not newer") so callers stay conservative
// and never act on an ambiguous comparison.
func IsNewerVersion(a, b string) bool {
	cmp, ok := compareSemver(a, b)
	return ok && cmp > 0
}
