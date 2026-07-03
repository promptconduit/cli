package cmd

import (
	"encoding/json"
	"fmt"
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
	Long:  `Display the current configuration and installation status of PromptConduit hooks.`,
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, args []string) error {
	fmt.Printf("PromptConduit CLI v%s\n\n", Version)

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
			fmt.Println("  promptconduit config set --api-key=\"your-api-key\"")
		}
	}

	fmt.Printf("API URL: %s\n", cfg.APIURL)
	fmt.Printf("Debug:   %v\n", cfg.Debug)
	fmt.Println()

	// Local event log health (sent/failed/dropped counters)
	printEventLogStatus(cfg)

	// Check tool installations
	fmt.Println("Tool Installations:")
	checkClaudeCodeInstallation()
	checkCursorInstallation()
	checkGeminiInstallation()

	return nil
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
	if st.LastSuccessAt != "" {
		fmt.Printf("  Last success: %s\n", localTime(st.LastSuccessAt))
	}
	if st.LastError != "" {
		fmt.Printf("  Last error:   %s — %s\n", localTime(st.LastErrorAt), st.LastError)
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

func checkClaudeCodeInstallation() {
	homeDir, _ := os.UserHomeDir()
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Println("  Claude Code: Not installed (no settings file)")
		return
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Println("  Claude Code: Error reading settings")
		return
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("  Claude Code: Not installed (no hooks)")
		return
	}

	// Count PromptConduit hooks
	count := 0
	for _, hookConfig := range hooks {
		if containsPromptConduitString(hookConfig) {
			count++
		}
	}

	if count > 0 {
		fmt.Printf("  Claude Code: Installed (%d hooks)\n", count)
	} else {
		fmt.Println("  Claude Code: Not installed")
	}
}

func checkCursorInstallation() {
	homeDir, _ := os.UserHomeDir()
	settingsPath := filepath.Join(homeDir, ".cursor", "hooks.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Println("  Cursor:      Not installed (no hooks file)")
		return
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Println("  Cursor:      Error reading settings")
		return
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("  Cursor:      Not installed (no hooks)")
		return
	}

	// Count PromptConduit hooks
	count := 0
	for _, hookConfig := range hooks {
		if containsPromptConduitString(hookConfig) {
			count++
		}
	}

	if count > 0 {
		fmt.Printf("  Cursor:      Installed (%d hooks)\n", count)
	} else {
		fmt.Println("  Cursor:      Not installed")
	}
}

func checkGeminiInstallation() {
	homeDir, _ := os.UserHomeDir()
	settingsPath := filepath.Join(homeDir, ".gemini", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Println("  Gemini CLI:  Not installed (no settings file)")
		return
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		fmt.Println("  Gemini CLI:  Error reading settings")
		return
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("  Gemini CLI:  Not installed (no hooks)")
		return
	}

	// Count PromptConduit hooks
	count := 0
	for _, hookConfig := range hooks {
		if containsPromptConduitString(hookConfig) {
			count++
		}
	}

	if count > 0 {
		fmt.Printf("  Gemini CLI:  Installed (%d hooks)\n", count)
	} else {
		fmt.Println("  Gemini CLI:  Not installed")
	}
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
