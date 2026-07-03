package enrich

import (
	"os"
	"regexp"
	"runtime"
	"strings"
)

// EnvEnrichment is the "env" slug: where the event was produced.
type EnvEnrichment struct {
	Host string `json:"host,omitempty"` // machine hostname (best-effort)
	OS   string `json:"os,omitempty"`   // runtime.GOOS
	// OSVersion is the human OS release (e.g. "26.1" on macOS, the
	// PRETTY_NAME on Linux). Best-effort file read; omitted when unknown.
	OSVersion string `json:"os_version,omitempty"`
	Arch      string `json:"arch,omitempty"` // runtime.GOARCH
	Cwd       string `json:"cwd,omitempty"`  // tool-reported working directory
}

type envEnricher struct{}

func init() { Register(envEnricher{}) }

func (envEnricher) Slug() string              { return "env" }
func (envEnricher) Applies(ctx *Context) bool { return true }

func (envEnricher) Enrich(ctx *Context) (any, error) {
	host, _ := os.Hostname()
	return EnvEnrichment{
		Host:      host,
		OS:        runtime.GOOS,
		OSVersion: osVersion(),
		Arch:      runtime.GOARCH,
		Cwd:       ctx.Cwd,
	}, nil
}

// osVersion reads the OS release from the platform's version file — a plain
// file read on the hook's hot path, no subprocess. Returns "" when unknown.
func osVersion() string {
	switch runtime.GOOS {
	case "darwin":
		data, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
		if err != nil {
			return ""
		}
		return osVersionFromPlist(data)
	case "linux":
		data, err := os.ReadFile("/etc/os-release")
		if err != nil {
			return ""
		}
		return osVersionFromOSRelease(data)
	}
	return ""
}

var productVersionRE = regexp.MustCompile(`<key>ProductVersion</key>\s*<string>([^<]+)</string>`)

// osVersionFromPlist pulls ProductVersion out of macOS's SystemVersion.plist.
func osVersionFromPlist(data []byte) string {
	if m := productVersionRE.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// osVersionFromOSRelease prefers PRETTY_NAME ("Ubuntu 24.04.1 LTS"), falling
// back to VERSION_ID, from /etc/os-release.
func osVersionFromOSRelease(data []byte) string {
	var versionID string
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		value = strings.Trim(value, `"`)
		switch key {
		case "PRETTY_NAME":
			if value != "" {
				return value
			}
		case "VERSION_ID":
			versionID = value
		}
	}
	return versionID
}
