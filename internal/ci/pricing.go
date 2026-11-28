// Package ci estimates what GitHub Actions costs per repo, PR and agent
// session, and flags CI waste. It reads only Actions metadata (runs and jobs:
// no logs, artifacts or code) with the user's own GitHub token, and never
// writes to the repo.
package ci

import (
	"math"
	"regexp"
	"strings"
	"time"
)

// RatesEffective is the date GitHub's current hosted-runner rate card took effect.
const RatesEffective = "2026-01-01"

// Rates is USD per minute by SKU, from
// docs.github.com/en/billing/reference/actions-runner-pricing. GitHub rounds
// each job up to the whole minute; there are no OS multipliers. Standard
// runners are free on public repos; larger runners are billed everywhere.
// Self-hosted runners cost $0 here. Keep identical to the service's table.
var Rates = map[string]float64{
	"actions_linux_slim":  0.002,
	"actions_linux":       0.006,
	"actions_linux_arm":   0.005,
	"actions_windows":     0.01,
	"actions_windows_arm": 0.01,
	"actions_macos":       0.062,
	"macos_l":             0.077,
	"macos_xl":            0.102,
	"linux_4_core":        0.012,
	"linux_8_core":        0.022,
	"linux_16_core":       0.042,
	"linux_32_core":       0.082,
	"linux_64_core":       0.162,
	"linux_96_core":       0.252,
	"linux_4_core_arm":    0.008,
	"linux_8_core_arm":    0.014,
	"linux_16_core_arm":   0.026,
	"linux_32_core_arm":   0.05,
	"linux_64_core_arm":   0.098,
	"windows_4_core":      0.022,
	"windows_8_core":      0.042,
	"windows_16_core":     0.082,
	"windows_32_core":     0.162,
	"windows_64_core":     0.322,
	"windows_96_core":     0.552,
	"linux_4_core_gpu":    0.052,
	"windows_4_core_gpu":  0.102,
	"self_hosted":         0,
}

var standardSKUs = map[string]bool{
	"actions_linux_slim": true, "actions_linux": true, "actions_linux_arm": true,
	"actions_windows": true, "actions_windows_arm": true, "actions_macos": true,
}

var (
	reUbuntuSlim = regexp.MustCompile(`^ubuntu-slim$`)
	reUbuntuArm  = regexp.MustCompile(`^ubuntu-.*-arm$`)
	reWindowsArm = regexp.MustCompile(`^windows-.*-arm$`)
	reMacXL      = regexp.MustCompile(`^macos-.*-xlarge$`)
	reMacL       = regexp.MustCompile(`^macos-.*-large$`)
	reMac        = regexp.MustCompile(`^macos-`)
	reWindows    = regexp.MustCompile(`^windows-`)
	reLarger     = regexp.MustCompile(`(linux|ubuntu|windows)[^0-9]*(\d+)[-_]?cores?([-_]arm(64)?)?([-_]gpu)?`)
	reHosted     = regexp.MustCompile(`(?i)^(github actions|github-hosted)`)
)

// SKUForLabels maps a job's runs-on labels to a SKU. Standard labels map
// exactly; larger runner names are org-defined, so "<os>-<n>-core[-arm][-gpu]"
// is parsed, falling back to standard Linux 2-core.
func SKUForLabels(labels []string, runnerName string) string {
	l := make([]string, len(labels))
	for i, x := range labels {
		l[i] = strings.ToLower(x)
	}
	has := func(re *regexp.Regexp) bool {
		for _, x := range l {
			if re.MatchString(x) {
				return true
			}
		}
		return false
	}
	for _, x := range l {
		if x == "self-hosted" {
			return "self_hosted"
		}
	}
	if len(l) == 0 && runnerName != "" && !reHosted.MatchString(runnerName) {
		return "self_hosted"
	}
	switch {
	case has(reUbuntuSlim):
		return "actions_linux_slim"
	case has(reUbuntuArm):
		return "actions_linux_arm"
	case has(reWindowsArm):
		return "actions_windows_arm"
	case has(reMacXL):
		return "macos_xl"
	case has(reMacL):
		return "macos_l"
	case has(reMac):
		return "actions_macos"
	}
	for _, x := range l {
		m := reLarger.FindStringSubmatch(x)
		if m == nil {
			continue
		}
		os := "linux"
		if m[1] == "windows" {
			os = "windows"
		}
		sku := os + "_" + m[2] + "_core"
		if m[3] != "" {
			sku += "_arm"
		}
		if m[5] != "" {
			sku += "_gpu"
		}
		if _, ok := Rates[sku]; ok {
			return sku
		}
	}
	if has(reWindows) {
		return "actions_windows"
	}
	return "actions_linux"
}

// BillableMinutes rounds a job up to the whole minute; 0 if it never ran.
func BillableMinutes(started, completed time.Time) int {
	if started.IsZero() || completed.IsZero() {
		return 0
	}
	d := completed.Sub(started)
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Minutes()))
}

// JobUSD prices one job.
func JobUSD(sku string, minutes int, private bool) float64 {
	if !private && standardSKUs[sku] {
		return 0
	}
	rate, ok := Rates[sku]
	if !ok {
		rate = Rates["actions_linux"]
	}
	return float64(minutes) * rate
}
