package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptconduit/cli/internal/client"
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

	// Check API key configuration
	cfg := client.LoadConfig()
	if cfg.IsConfigured() {
		fmt.Printf("API Key: %s (configured)\n", client.MaskAPIKey(cfg.APIKey))
	} else {
		fmt.Println("API Key: Not configured")
		fmt.Println("  Set with: promptconduit config set --api-key=\"your-api-key\"")
	}

	fmt.Printf("API URL: %s\n", cfg.APIURL)
	fmt.Printf("Debug:   %v\n", cfg.Debug)
	fmt.Println()

	// Check tool installations
	fmt.Println("Tool Installations:")
	checkClaudeCodeInstallation()
	checkCursorInstallation()
	checkGeminiInstallation()

	return nil
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
