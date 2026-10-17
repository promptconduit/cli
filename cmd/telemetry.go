package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/extension"
	"github.com/promptconduit/cli/internal/logger"
	"github.com/promptconduit/cli/internal/outbound"
	"github.com/promptconduit/cli/internal/telemetry"
	"github.com/spf13/cobra"
)

// telemetry-ping is the detached worker that sends the anonymous daily usage
// ping (see internal/telemetry). Interactive commands and the hook spawn it at
// most once per retry window; it never runs in the foreground of either.
var telemetryPingCmd = &cobra.Command{
	Use:           "telemetry-ping",
	Short:         "Send the anonymous daily usage ping (internal use)",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		runTelemetryPing()
		return nil
	},
}

func init() {
	rootCmd.AddCommand(telemetryPingCmd)
}

func runTelemetryPing() {
	cfg := client.LoadConfig()
	if !telemetry.Enabled(cfg.DisableTelemetry, Version) {
		return
	}
	dir := client.ConfigDir()
	st := telemetry.Load(dir)
	if st.InstallID == "" || st.NoticeShownAt.IsZero() {
		return
	}

	p := telemetry.Payload{
		InstallID:        st.InstallID,
		CLIVersion:       Version,
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		Tools:            installedHookTools(),
		ExtensionVersion: installedExtensionVersion(),
		LoggedIn:         cfg.APIKey != "",
	}
	// Mirror the request to outbound.ndjson like every other CLI request, so
	// `promptconduit watch` shows exactly what was sent.
	hc := &http.Client{Transport: outbound.New(filepath.Join(dir, outbound.MirrorFileName), http.DefaultTransport)}
	if err := telemetry.Send(context.Background(), hc, cfg.APIURL, "promptconduit/"+Version, p); err != nil {
		logger.Debug("telemetry: send failed: %v", err)
		return
	}
	// Re-read so a concurrent notice/attempt write isn't clobbered.
	st = telemetry.Load(dir)
	st.MarkSent()
	_ = telemetry.Save(dir, st)
}

// maybeTelemetry runs from interactive commands (PersistentPreRun). The first
// time telemetry is enabled on a terminal it prints the notice and sends
// nothing; on later runs it spawns the sender when a ping is due.
func maybeTelemetry(cmd *cobra.Command) {
	if skipUpdateCheckFor(cmd) || commandPathRoot(cmd) == "config" {
		return
	}
	cfg := client.LoadConfig()
	if !telemetry.Enabled(cfg.DisableTelemetry, Version) {
		return
	}
	dir := client.ConfigDir()
	st := telemetry.Load(dir)
	if st.NoticeShownAt.IsZero() {
		// Only count the notice as shown when a person can actually see it,
		// not when a tool runs the CLI with stderr captured.
		if !outbound.IsTerminal(os.Stderr) {
			return
		}
		printTelemetryNotice(cmd.ErrOrStderr())
		st.MarkNoticeShown()
		_ = telemetry.Save(dir, st)
		return
	}
	spawnTelemetryIfDue(dir, st)
}

// maybeSpawnTelemetry runs at the end of the hook. It never prints, and does
// nothing until an interactive command has shown the notice. Hot-path cost is
// one small file read.
func maybeSpawnTelemetry(cfg *client.Config) {
	if !telemetry.Enabled(cfg.DisableTelemetry, Version) {
		return
	}
	dir := client.ConfigDir()
	spawnTelemetryIfDue(dir, telemetry.Load(dir))
}

func spawnTelemetryIfDue(dir string, st *telemetry.State) {
	if !st.Due() {
		return
	}
	// Claim the retry window before spawning so sibling hooks firing in the
	// same instant don't each start a sender.
	st.MarkAttempt()
	if err := telemetry.Save(dir, st); err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(exe, "telemetry-ping")
	c.Stdin, c.Stdout, c.Stderr = nil, nil, nil
	c.Env = append(os.Environ(), "PROMPTCONDUIT_AUTO_UPDATE_CHILD=1")
	detachProcess(c)
	if err := c.Start(); err != nil {
		logger.Debug("telemetry: spawn failed: %v", err)
		return
	}
	_ = c.Process.Release()
}

func printTelemetryNotice(w io.Writer) {
	_, _ = fmt.Fprintln(w, telemetry.Notice)
}

// installedHookTools lists the tools whose hook config references promptconduit,
// using the same checks as `promptconduit status`.
func installedHookTools() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	hookFiles := map[string]string{
		"claude-code": filepath.Join(home, ".claude", "settings.json"),
		"cursor":      filepath.Join(home, ".cursor", "hooks.json"),
		"gemini-cli":  filepath.Join(home, ".gemini", "settings.json"),
		"codex":       filepath.Join(home, ".codex", "hooks.json"),
	}
	var tools []string
	for tool, path := range hookFiles {
		if hooksReferencePromptConduit(path) {
			tools = append(tools, tool)
		}
	}
	if copilotHooksInstalled(filepath.Join(home, ".copilot", "hooks")) {
		tools = append(tools, "copilot")
	}
	if fileReferencesPromptConduit(filepath.Join(home, ".grok", "hooks", GrokHookFile)) {
		tools = append(tools, "grok")
	}
	sort.Strings(tools)
	return tools
}

// hooksReferencePromptConduit reports whether the "hooks" object of a JSON
// settings file mentions promptconduit.
func hooksReferencePromptConduit(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var settings map[string]interface{}
	if json.Unmarshal(data, &settings) != nil {
		return false
	}
	hooks, ok := settings["hooks"].(map[string]interface{})
	return ok && containsPromptConduitString(hooks)
}

func fileReferencesPromptConduit(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var v map[string]interface{}
	return json.Unmarshal(data, &v) == nil && containsPromptConduitString(v)
}

func copilotHooksInstalled(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && fileReferencesPromptConduit(filepath.Join(dir, e.Name())) {
			return true
		}
	}
	return false
}

// installedExtensionVersion returns the cost extension version installed in
// Cursor or VS Code, or "" when neither has it.
func installedExtensionVersion() string {
	for _, e := range []extension.Editor{extension.Cursor, extension.VSCode} {
		if v, err := e.InstalledVersion(); err == nil && v != "" {
			return v
		}
	}
	return ""
}
