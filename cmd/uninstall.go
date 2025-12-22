package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptconduit/cli/internal/adapters"
	"github.com/spf13/cobra"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall <tool>",
	Short: "Uninstall PromptConduit hooks from an AI tool",
	Long: `Remove PromptConduit hooks from the specified AI coding assistant.

Supported tools:
  - claude-code: Claude Code CLI
  - cursor: Cursor IDE
  - gemini-cli: Gemini CLI (also accepts "gemini")`,
	Args: cobra.ExactArgs(1),
	RunE: runUninstall,
}

func runUninstall(cmd *cobra.Command, args []string) error {
	toolName := args[0]

	if !adapters.IsValidTool(toolName) {
		return fmt.Errorf("unknown tool: %s. Supported: %v", toolName, adapters.SupportedTools())
	}

	switch toolName {
	case "claude-code":
		return uninstallClaudeCode()
	case "cursor":
		return uninstallCursor()
	case "gemini-cli", "gemini":
		return uninstallGemini()
	default:
		return fmt.Errorf("uninstallation not implemented for: %s", toolName)
	}
}

func uninstallClaudeCode() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No Claude Code settings file found - nothing to uninstall")
			return nil
		}
		return fmt.Errorf("failed to read settings: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("No hooks found in Claude Code settings")
		return nil
	}

	// Remove all hooks containing "promptconduit"
	removed := 0
	for hookName, hookConfig := range hooks {
		if containsPromptConduit(hookConfig) {
			delete(hooks, hookName)
			removed++
		}
	}

	if removed == 0 {
		fmt.Println("No PromptConduit hooks found in Claude Code settings")
		return nil
	}

	// Clean up empty hooks object
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}

	// Write settings back
	data, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	fmt.Printf("Successfully removed %d PromptConduit hook(s) from Claude Code\n", removed)
	return nil
}

func uninstallCursor() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".cursor", "hooks.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No Cursor hooks file found - nothing to uninstall")
			return nil
		}
		return fmt.Errorf("failed to read settings: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("No hooks found in Cursor settings")
		return nil
	}

	// Remove all hooks containing "promptconduit"
	removed := 0
	for hookName, hookConfig := range hooks {
		if containsPromptConduit(hookConfig) {
			delete(hooks, hookName)
			removed++
		}
	}

	if removed == 0 {
		fmt.Println("No PromptConduit hooks found in Cursor settings")
		return nil
	}

	// Clean up empty hooks object
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}

	// Write settings back
	data, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	fmt.Printf("Successfully removed %d PromptConduit hook(s) from Cursor\n", removed)
	return nil
}

func uninstallGemini() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".gemini", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No Gemini CLI settings file found - nothing to uninstall")
			return nil
		}
		return fmt.Errorf("failed to read settings: %w", err)
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("failed to parse settings: %w", err)
	}

	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		fmt.Println("No hooks found in Gemini CLI settings")
		return nil
	}

	// Remove all hooks containing "promptconduit"
	removed := 0
	for hookName, hookConfig := range hooks {
		if containsPromptConduit(hookConfig) {
			delete(hooks, hookName)
			removed++
		}
	}

	if removed == 0 {
		fmt.Println("No PromptConduit hooks found in Gemini CLI settings")
		return nil
	}

	// Clean up empty hooks object
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}

	// Write settings back
	data, err = json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	fmt.Printf("Successfully removed %d PromptConduit hook(s) from Gemini CLI\n", removed)
	return nil
}

// containsPromptConduit recursively checks if a value contains "promptconduit"
func containsPromptConduit(v interface{}) bool {
	switch val := v.(type) {
	case string:
		return strings.Contains(strings.ToLower(val), "promptconduit")
	case map[string]interface{}:
		for _, v := range val {
			if containsPromptConduit(v) {
				return true
			}
		}
	case []interface{}:
		for _, item := range val {
			if containsPromptConduit(item) {
				return true
			}
		}
	}
	return false
}
