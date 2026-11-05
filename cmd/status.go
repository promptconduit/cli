package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show PromptConduit installation status",
	Long: `Display the current configuration and installation status of PromptConduit hooks.

Exits 1 when no AI tool has PromptConduit hooks installed (nothing is being
captured), so scripts can use it as a health check. Local-only mode and a
missing API key are normal states, not failures.`,
	Example: `  promptconduit status
  promptconduit status --json | jq '.tools[] | select(.installed)'
  promptconduit status >/dev/null || promptconduit install`,
	RunE: runStatus,
}

var statusJSON bool

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Print machine-readable JSON")
}

// errNothingInstalled is status's failure: no tool has hooks installed.
var errNothingInstalled = fmt.Errorf("no AI tool has PromptConduit hooks installed. Run: promptconduit install")

// statusReport is the `status --json` payload. Keep fields stable: scripts
// read it.
type statusReport struct {
	CLIVersion       string              `json:"cli_version"`
	Mode             string              `json:"mode"` // "cloud" or "local-only"
	APIKeyConfigured bool                `json:"api_key_configured"`
	APIURL           string              `json:"api_url"`
	Debug            bool                `json:"debug"`
	EventLog         eventLogReport      `json:"event_log"`
	Tools            []toolInstallStatus `json:"tools"`
	MCP              mcpStatusReport     `json:"mcp"`
	// Ready is true when at least one tool has hooks installed.
	Ready bool `json:"ready"`
}

type eventLogReport struct {
	Enabled        bool            `json:"enabled"`
	Counters       eventlog.Status `json:"counters"`
	Queued         int             `json:"queued"`
	CapturedPath   string          `json:"captured_path"`
	CapturedEvents *int            `json:"captured_events,omitempty"`
}

func buildStatusReport() statusReport {
	cfg := client.LoadConfig()
	home, _ := os.UserHomeDir()
	r := statusReport{
		CLIVersion:       Version,
		Mode:             "local-only",
		APIKeyConfigured: cfg.IsConfigured(),
		APIURL:           cfg.APIURL,
		Debug:            cfg.Debug,
		EventLog: eventLogReport{
			Enabled:      cfg.EventLogEnabled(),
			CapturedPath: eventlog.EventsJSONLPath(),
		},
		Tools: inspectToolInstalls(home),
		MCP:   buildMCPStatus(home, loadMCPState()),
	}
	if cfg.ShouldSend() {
		r.Mode = "cloud"
	}
	if r.EventLog.Enabled {
		r.EventLog.Counters = eventlog.LoadStatus()
		r.EventLog.Queued = eventlog.OutboxCount(cfg.APIURL)
		if n, ok := eventlog.CountCaptured(); ok {
			r.EventLog.CapturedEvents = &n
		}
	}
	for _, t := range r.Tools {
		if t.Installed {
			r.Ready = true
		}
	}
	return r
}

func runStatus(cmd *cobra.Command, args []string) error {
	if statusJSON {
		r := buildStatusReport()
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			return err
		}
		if !r.Ready {
			return errNothingInstalled
		}
		return nil
	}

	fmt.Printf("PromptConduit CLI v%s\n\n", Version)

	if self, err := os.Executable(); err == nil {
		home, _ := os.UserHomeDir()
		printDuplicateBinaries(os.Stdout, binariesOnPath(os.Getenv("PATH"), "promptconduit"), self, hookBinaryPaths(home), func(dir string) bool { return dirWritable(dir) == nil })
	}

	// Mode + API key. Free / local-only is a first-class mode, not an error:
	// events are captured locally and nothing is sent. Cloud sync requires an
	// API key and local_only off.
	cfg := client.LoadConfig()
	if cfg.ShouldSend() {
		fmt.Println("Mode:    Cloud sync — events sent to the platform")
		fmt.Printf("API Key: %s (configured)\n", client.MaskAPIKey(cfg.APIKey))
	} else {
		fmt.Println("Mode:    Free (local-only) — events captured locally, nothing sent")
		if cfg.LocalOnly {
			fmt.Println("  local_only is on. Enable cloud sync with: promptconduit config set --local-only=false")
		}
		if cfg.IsConfigured() {
			fmt.Printf("API Key: %s (configured, unused in local-only mode)\n", client.MaskAPIKey(cfg.APIKey))
		} else {
			fmt.Println("API Key: Not set — add one to enable cloud sync:")
			fmt.Println("  promptconduit login")
		}
	}

	fmt.Printf("API URL: %s\n", cfg.APIURL)
	fmt.Printf("Debug:   %v\n", cfg.Debug)
	fmt.Println()

	// Local event log health (sent/failed/dropped counters)
	printEventLogStatus(cfg)

	// Check tool installations
	fmt.Println("Tool Installations:")
	home, _ := os.UserHomeDir()
	tools := inspectToolInstalls(home)
	ready := false
	for _, t := range tools {
		fmt.Printf("  %-12s %s\n", t.Label+":", t.describe())
		ready = ready || t.Installed
	}

	printMCPStatus()
	if !ready {
		fmt.Println()
		return errNothingInstalled
	}
	return nil
}

// toolInstallStatus is one tool's hook installation, as `status --json`
// reports it.
type toolInstallStatus struct {
	Tool         string `json:"tool"`
	Label        string `json:"label"`
	Experimental bool   `json:"experimental"`
	Installed    bool   `json:"installed"`
	Hooks        int    `json:"hooks"`
	// Detail explains a not-installed state ("no settings file") or an
	// error reading the tool's config.
	Detail string `json:"detail,omitempty"`
	Error  bool   `json:"error,omitempty"`
}

func (t toolInstallStatus) describe() string {
	switch {
	case t.Error:
		return t.Detail
	case t.Installed && t.Hooks > 0:
		return fmt.Sprintf("Installed (%d hooks)", t.Hooks)
	case t.Installed:
		return "Installed"
	case t.Detail != "":
		return "Not installed (" + t.Detail + ")"
	default:
		return "Not installed"
	}
}

// inspectToolInstalls checks every tool's hook config for PromptConduit
// hooks, supported tools first.
func inspectToolInstalls(home string) []toolInstallStatus {
	return []toolInstallStatus{
		inspectHooksMap("claude-code", "Claude Code", filepath.Join(home, ".claude", "settings.json"), "no settings file"),
		inspectHooksMap("cursor", "Cursor", filepath.Join(home, ".cursor", "hooks.json"), "no hooks file"),
		inspectHooksMap("gemini-cli", "Gemini CLI", filepath.Join(home, ".gemini", "settings.json"), "no settings file"),
		inspectHooksMap("codex", "Codex", filepath.Join(home, ".codex", "hooks.json"), "no hooks file"),
		inspectCopilot(home),
		inspectGrok(home),
	}
}

func newToolStatus(tool, label string) toolInstallStatus {
	return toolInstallStatus{Tool: tool, Label: label, Experimental: isExperimentalTool(tool)}
}

// inspectHooksMap handles tools whose config is JSON with a top-level
// "hooks" object keyed by event name.
func inspectHooksMap(tool, label, path, missing string) toolInstallStatus {
	s := newToolStatus(tool, label)
	data, err := os.ReadFile(path)
	if err != nil {
		s.Detail = missing
		return s
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		s.Error, s.Detail = true, "Error reading "+filepath.Base(path)
		return s
	}
	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		s.Detail = "no hooks"
		return s
	}
	for _, hookConfig := range hooks {
		if containsPromptConduitString(hookConfig) {
			s.Hooks++
		}
	}
	s.Installed = s.Hooks > 0
	return s
}

func inspectCopilot(home string) toolInstallStatus {
	s := newToolStatus("copilot", "Copilot")
	hooksDir := filepath.Join(home, ".copilot", "hooks")
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		s.Detail = "no hooks directory"
		return s
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(hooksDir, entry.Name()))
		if err != nil {
			continue
		}
		var hook map[string]interface{}
		if err := json.Unmarshal(data, &hook); err != nil {
			continue
		}
		if containsPromptConduitString(hook) {
			s.Hooks++
		}
	}
	s.Installed = s.Hooks > 0
	return s
}

func inspectGrok(home string) toolInstallStatus {
	s := newToolStatus("grok", "Grok Build")
	data, err := os.ReadFile(filepath.Join(home, ".grok", "hooks", GrokHookFile))
	if err != nil {
		s.Detail = "no hooks file"
		return s
	}
	var hook map[string]interface{}
	if err := json.Unmarshal(data, &hook); err != nil {
		s.Detail = "invalid hooks file"
		return s
	}
	s.Installed = containsPromptConduitString(hook)
	return s
}

// printMCPStatus shows where the PromptConduit MCP server is registered.
func printMCPStatus() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	writeMCPStatus(os.Stdout, home, loadMCPState())
}

// mcpToolStatus is one tool's MCP registration, as `mcp status --json`
// reports it. The editor extension reads this shape: keep fields stable.
type mcpToolStatus struct {
	Tool           string `json:"tool"`
	Label          string `json:"label"`
	Registered     bool   `json:"registered"`
	HooksInstalled bool   `json:"hooks_installed"`
	TurnedOff      bool   `json:"turned_off"`
}

// mcpStatusReport is the full `mcp status --json` payload. Suggest lists the
// tools worth registering: hooks installed, not registered, not opted out.
type mcpStatusReport struct {
	Tools   []mcpToolStatus `json:"tools"`
	Suggest []string        `json:"suggest"`
}

func buildMCPStatus(home string, state mcpState) mcpStatusReport {
	r := mcpStatusReport{Tools: []mcpToolStatus{}, Suggest: []string{}}
	for _, t := range mcpTargets {
		s := mcpToolStatus{
			Tool:           t.Tool,
			Label:          t.Label,
			Registered:     t.registered(home),
			HooksInstalled: t.hooksInstalled(home),
			TurnedOff:      state.disabledFor(t.Tool),
		}
		if !s.Registered && !s.TurnedOff && s.HooksInstalled {
			r.Suggest = append(r.Suggest, t.Tool)
		}
		r.Tools = append(r.Tools, s)
	}
	return r
}

// writeMCPStatus lists each MCP-capable tool's registration. The register
// hint names only tools that have PromptConduit hooks, aren't registered,
// and weren't opted out — never a tool the user doesn't have.
func writeMCPStatus(out io.Writer, home string, state mcpState) {
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "MCP Server (agents can query your cost data):")
	r := buildMCPStatus(home, state)
	for _, s := range r.Tools {
		var line string
		switch {
		case s.Registered:
			line = "Registered"
		case s.TurnedOff:
			line = "Not registered (turned off)"
		case !s.HooksInstalled:
			line = "Not registered (no PromptConduit hooks)"
		default:
			line = "Not registered"
		}
		_, _ = fmt.Fprintf(out, "  %-12s %s\n", s.Label+":", line)
	}
	if len(r.Suggest) > 0 {
		_, _ = fmt.Fprintf(out, "  Register with: promptconduit mcp install %s\n", strings.Join(r.Suggest, " "))
	}
}

// printEventLogStatus shows the rolling sent/failed/dropped counters from the
// local event log so the user can tell at a glance whether events are flowing.
func printEventLogStatus(cfg *client.Config) {
	if !cfg.EventLogEnabled() {
		fmt.Println("Event Log: disabled")
		fmt.Println("  Re-enable with: promptconduit config set --disable-event-log=false")
		fmt.Println()
		return
	}

	st := eventlog.LoadStatus()
	fmt.Println("Event Log: enabled")
	fmt.Printf("  %d sent · %d failed · %d dropped\n", st.Sent, st.Failed, st.Dropped)
	// Recovery: retried = succeeded after an in-process retry; replayed =
	// delivered later from the outbox; queued = waiting in the outbox now.
	if queued := eventlog.OutboxCount(cfg.APIURL); st.Retried > 0 || st.Replayed > 0 || queued > 0 {
		fmt.Printf("  %d recovered by retry · %d replayed later · %d queued for replay\n", st.Retried, st.Replayed, queued)
		if queued > 0 && cfg.ShouldSend() {
			fmt.Println("  Replay the queue now with: promptconduit events flush")
		}
	}
	if st.LastSuccessAt != "" {
		fmt.Printf("  Last success: %s\n", localTime(st.LastSuccessAt))
	}
	if st.LastError != "" {
		fmt.Printf("  Last error:   %s — %s\n", localTime(st.LastErrorAt), st.LastError)
		if cfg.ShouldSend() {
			if hint := authFailureHint(st.LastError, st.LastErrorAt, st.LastSuccessAt); hint != "" {
				fmt.Printf("  %s\n", hint)
			}
		}
	}
	// Captured events: the send-independent local stream (events.jsonl), written
	// for every event even in Free / local-only mode.
	fmt.Printf("  Captured:     %s", eventlog.EventsJSONLPath())
	if n, ok := eventlog.CountCaptured(); ok {
		fmt.Printf(" (%d events)", n)
	}
	fmt.Println()
	fmt.Println("  Inspect with: promptconduit events")
	fmt.Println()
}

// localTime renders an RFC3339 timestamp in the local zone, falling back to the
// raw value if it can't be parsed.
func localTime(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Local().Format("2006-01-02 15:04:05")
	}
	return ts
}

// containsPromptConduitString checks if a value contains "promptconduit" string
func containsPromptConduitString(v interface{}) bool {
	switch val := v.(type) {
	case string:
		return strings.Contains(strings.ToLower(val), "promptconduit")
	case map[string]interface{}:
		for _, v := range val {
			if containsPromptConduitString(v) {
				return true
			}
		}
	case []interface{}:
		for _, item := range val {
			if containsPromptConduitString(item) {
				return true
			}
		}
	}
	return false
}
