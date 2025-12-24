package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/promptconduit/cli/internal/envelope"
	"github.com/spf13/cobra"
)

var installCmd = &cobra.Command{
	Use:   "install <tool>",
	Short: "Install PromptConduit hooks for an AI tool",
	Long: `Install PromptConduit hooks for the specified AI coding assistant.

Supported tools:
  - claude-code: Claude Code CLI
  - cursor: Cursor IDE
  - gemini-cli: Gemini CLI (also accepts "gemini")

The hooks will capture events from the tool and send them to the PromptConduit API.

Prerequisites:
  1. Set your API key: export PROMPTCONDUIT_API_KEY="your-key"
  2. Have the target tool installed`,
	Args: cobra.ExactArgs(1),
	RunE: runInstall,
}

func runInstall(cmd *cobra.Command, args []string) error {
	toolName := args[0]

	if !envelope.IsValidTool(toolName) {
		return fmt.Errorf("unknown tool: %s. Supported: %v", toolName, envelope.SupportedTools())
	}

	// Get the executable path for hook commands
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	// Resolve symlinks to get actual binary path
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	switch toolName {
	case "claude-code":
		return installClaudeCode(exePath)
	case "cursor":
		return installCursor(exePath)
	case "gemini-cli", "gemini":
		return installGemini(exePath)
	default:
		return fmt.Errorf("installation not implemented for: %s", toolName)
	}
}

func installClaudeCode(exePath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")

	// Read existing settings or create new
	settings := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse existing settings: %w", err)
		}
	}

	// Build hook configuration
	hookCmd := fmt.Sprintf("%s hook", exePath)
	hooks := buildClaudeCodeHooks(hookCmd)

	// Merge hooks into settings
	if existingHooks, ok := settings["hooks"].(map[string]interface{}); ok {
		for name, config := range hooks {
			existingHooks[name] = config
		}
	} else {
		settings["hooks"] = hooks
	}

	// Write settings back
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return fmt.Errorf("failed to create settings directory: %w", err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	fmt.Println("Successfully installed PromptConduit hooks for Claude Code")
	fmt.Printf("Settings file: %s\n", settingsPath)
	fmt.Println("\nMake sure you have set your API key:")
	fmt.Println("  export PROMPTCONDUIT_API_KEY=\"your-api-key\"")

	return nil
}

func buildClaudeCodeHooks(hookCmd string) map[string]interface{} {
	makeHook := func(timeout int) []map[string]interface{} {
		return []map[string]interface{}{
			{
				"type":    "command",
				"command": hookCmd,
				"timeout": timeout,
			},
		}
	}

	makeMatcherHook := func(timeout int) []map[string]interface{} {
		return []map[string]interface{}{
			{
				"matcher": "*",
				"hooks":   makeHook(timeout),
			},
		}
	}

	return map[string]interface{}{
		"UserPromptSubmit": []map[string]interface{}{
			{"hooks": makeHook(5000)},
		},
		"PreToolUse":   makeMatcherHook(5000),
		"PostToolUse":  makeMatcherHook(5000),
		"SessionStart": []map[string]interface{}{{"hooks": makeHook(5000)}},
		"SessionEnd":   []map[string]interface{}{{"hooks": makeHook(5000)}},
	}
}

func installCursor(exePath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".cursor", "hooks.json")

	// Read existing settings or create new
	settings := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse existing settings: %w", err)
		}
	}

	// Build hook configuration
	hookCmd := fmt.Sprintf("%s hook", exePath)
	hooks := buildCursorHooks(hookCmd)

	// Merge hooks into settings
	if existingHooks, ok := settings["hooks"].(map[string]interface{}); ok {
		for name, config := range hooks {
			existingHooks[name] = config
		}
	} else {
		settings["hooks"] = hooks
	}

	// Write settings back
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return fmt.Errorf("failed to create settings directory: %w", err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	fmt.Println("Successfully installed PromptConduit hooks for Cursor")
	fmt.Printf("Settings file: %s\n", settingsPath)
	fmt.Println("\nMake sure you have set your API key:")
	fmt.Println("  export PROMPTCONDUIT_API_KEY=\"your-api-key\"")

	return nil
}

func buildCursorHooks(hookCmd string) map[string]interface{} {
	makeHook := func() []map[string]interface{} {
		return []map[string]interface{}{
			{"command": hookCmd},
		}
	}

	return map[string]interface{}{
		"beforeSubmitPrompt":   makeHook(),
		"beforeShellExecution": makeHook(),
		"afterShellExecution":  makeHook(),
		"afterFileEdit":        makeHook(),
	}
}

func installGemini(exePath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".gemini", "settings.json")

	// Read existing settings or create new
	settings := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse existing settings: %w", err)
		}
	}

	// Build hook configuration
	hookCmd := fmt.Sprintf("%s hook", exePath)
	hooks := buildGeminiHooks(hookCmd)

	// Merge hooks into settings
	if existingHooks, ok := settings["hooks"].(map[string]interface{}); ok {
		for name, config := range hooks {
			existingHooks[name] = config
		}
	} else {
		settings["hooks"] = hooks
	}

	// Write settings back
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return fmt.Errorf("failed to create settings directory: %w", err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}

	fmt.Println("Successfully installed PromptConduit hooks for Gemini CLI")
	fmt.Printf("Settings file: %s\n", settingsPath)
	fmt.Println("\nMake sure you have set your API key:")
	fmt.Println("  export PROMPTCONDUIT_API_KEY=\"your-api-key\"")

	return nil
}

func buildGeminiHooks(hookCmd string) map[string]interface{} {
	makeHook := func(timeout int) []map[string]interface{} {
		return []map[string]interface{}{
			{
				"type":    "command",
				"command": hookCmd,
				"timeout": timeout,
			},
		}
	}

	makeMatcherHook := func(timeout int) []map[string]interface{} {
		return []map[string]interface{}{
			{
				"matcher": "*",
				"hooks":   makeHook(timeout),
			},
		}
	}

	return map[string]interface{}{
		"BeforeAgent":  []map[string]interface{}{{"hooks": makeHook(5000)}},
		"BeforeTool":   makeMatcherHook(5000),
		"AfterTool":    makeMatcherHook(5000),
		"SessionStart": []map[string]interface{}{{"hooks": makeHook(5000)}},
		"SessionEnd":   []map[string]interface{}{{"hooks": makeHook(5000)}},
	}
}

// isCommandAvailable checks if a command exists in PATH
func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
