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
// Homebrew — i.e. `brew` is installed and the resolved binary lives inside a
// Homebrew `Cellar`. When true, the upgrade flow delegates to `brew upgrade`
// instead of self-replacing the Cellar file. Self-replacing a brew-managed
// binary leaves Homebrew's metadata pointing at the old version while the file
// on disk is newer — the drift that results from two update channels fighting.
//
// Detection keys ONLY on a `Cellar` path component, which every real Homebrew
// install resolves into (prefix/bin/promptconduit symlinks into
// .../Cellar/promptconduit/<v>/bin/promptconduit) on all platforms. We
// deliberately do NOT also treat "under `brew --prefix`" as managed: on Intel
// macs the prefix is /usr/local and the curl install script drops the binary in
// /usr/local/bin, so a script-installed binary on a machine that merely has brew
// would be misclassified — and then every `brew upgrade promptconduit` would
// fail (brew doesn't own it) with no self-replace fallback, stranding the user.
func IsHomebrewManaged(exePath string) bool {
	if _, err := exec.LookPath("brew"); err != nil {
		return false
	}
	resolved := exePath
	if r, err := filepath.EvalSymlinks(exePath); err == nil {
		resolved = r
	}
	return isUnderCellar(resolved)
}

// isUnderCellar is the pure path check: true when path traverses a `Cellar`
// directory component (Homebrew's install location on every platform).
func isUnderCellar(path string) bool {
	sep := string(filepath.Separator)
	return strings.Contains(path, sep+"Cellar"+sep)
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
