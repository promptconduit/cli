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
  1. Set your API key: promptconduit config set --api-key="your-key"
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

	// Strip any of OUR previously-installed hook entries first so events we
	// no longer ship (e.g. WorktreeCreate, which we used to register but now
	// deliberately skip) get cleaned up rather than lingering forever. Any
	// hook whose value doesn't reference "promptconduit" is left alone —
	// that's the user's own configuration.
	if existingHooks, ok := settings["hooks"].(map[string]interface{}); ok {
		for name, config := range existingHooks {
			if containsPromptConduit(config) {
				delete(existingHooks, name)
			}
		}
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
	fmt.Println("\nMake sure you have configured your API key:")
	fmt.Println("  promptconduit config set --api-key=\"your-api-key\"")

	return nil
}

// buildClaudeCodeHooks registers for every event in the current Claude Code
// hooks reference (https://code.claude.com/docs/en/hooks) except for
// WorktreeCreate. Configuring a WorktreeCreate hook *replaces* the default
// `git worktree` behavior — Claude Code expects the hook to print the
// new worktree path on stdout, but our hook handler always prints
// `{"continue": true}`, which would cause `claude --worktree` and
// `isolation: "worktree"` subagents to fail outright. Until our hook
// handler can detect WorktreeCreate events and behave correctly (return
// the path), we don't register for that event.
//
// Matchers on no-matcher events (TaskCreated, TaskCompleted, TeammateIdle,
// PostToolBatch, etc.) are silently ignored per the spec, so we use
// `makeHook` for those rather than `makeMatcherHook` to keep the
// settings.json output tidy.
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

	plainEvent := func() []map[string]interface{} {
		return []map[string]interface{}{{"hooks": makeHook(5000)}}
	}

	return map[string]interface{}{
		// Session lifecycle
		"SessionStart": plainEvent(),
		"Setup":        plainEvent(), // --init-only / -p --init / -p --maintenance
		"SessionEnd":   plainEvent(),
		// Per-turn events
		"UserPromptSubmit":    plainEvent(),
		"UserPromptExpansion": plainEvent(), // slash-command expansion path
		"Stop":                plainEvent(), // Agent completes response
		"StopFailure":         plainEvent(), // Turn ends due to API error
		// Tool execution events (matchers filter on tool name)
		"PreToolUse":         makeMatcherHook(5000),
		"PostToolUse":        makeMatcherHook(5000),
		"PostToolUseFailure": makeMatcherHook(5000),
		"PostToolBatch":      plainEvent(), // no matcher support — fires once per batch
		"PermissionRequest":  makeMatcherHook(5000),
		"PermissionDenied":   makeMatcherHook(5000), // Auto mode denies a tool call
		// Agent and task events
		"SubagentStart": makeMatcherHook(5000), // matcher filters on agent type
		"SubagentStop":  makeMatcherHook(5000),
		"TaskCreated":   plainEvent(), // no matcher support
		"TaskCompleted": plainEvent(), // no matcher support
		"TeammateIdle":  plainEvent(), // no matcher support
		// File and configuration events
		"InstructionsLoaded": plainEvent(), // CLAUDE.md / .claude/rules/*.md loaded
		"ConfigChange":       plainEvent(),
		"CwdChanged":         plainEvent(),
		"FileChanged":        plainEvent(),
		// Context compaction events
		"PreCompact":  plainEvent(),
		"PostCompact": plainEvent(),
		// Worktree (only Remove — see Create note in function comment)
		"WorktreeRemove": plainEvent(),
		// MCP events (matcher filters on MCP server name)
		"Elicitation":       makeMatcherHook(5000),
		"ElicitationResult": makeMatcherHook(5000),
		// Notifications
		"Notification": makeMatcherHook(5000),
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

	// Strip any of OUR previously-installed hook entries first so events we
	// no longer ship (e.g. WorktreeCreate, which we used to register but now
	// deliberately skip) get cleaned up rather than lingering forever. Any
	// hook whose value doesn't reference "promptconduit" is left alone —
	// that's the user's own configuration.
	if existingHooks, ok := settings["hooks"].(map[string]interface{}); ok {
		for name, config := range existingHooks {
			if containsPromptConduit(config) {
				delete(existingHooks, name)
			}
		}
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
	fmt.Println("\nMake sure you have configured your API key:")
	fmt.Println("  promptconduit config set --api-key=\"your-api-key\"")

	return nil
}

// buildCursorHooks registers for every event the current Cursor agent + Tab
// hooks spec exposes (https://cursor.com/docs/hooks). Agent hooks cover the
// Cmd+K / Agent Chat flow; Tab hooks cover inline-completion autonomous
// edits, which deliberately get a separate policy from user-directed Agent
// operations.
//
// We register both the generic `preToolUse`/`postToolUse` AND the
// specific-tool variants (`beforeShellExecution`, `beforeMCPExecution`,
// `beforeReadFile`, `afterFileEdit`). The specific events are richer
// (they carry the actual command / file path / MCP server), and the
// generic ones backfill any tool category that doesn't have a dedicated
// hook. The platform dedupes server-side by event id.
func buildCursorHooks(hookCmd string) map[string]interface{} {
	makeHook := func() []map[string]interface{} {
		return []map[string]interface{}{
			{"command": hookCmd},
		}
	}

	return map[string]interface{}{
		// Session lifecycle
		"sessionStart": makeHook(),
		"sessionEnd":   makeHook(),
		// Generic tool use (fires for every tool, including the specific
		// ones below; we keep both for coverage of unknown tool kinds)
		"preToolUse":         makeHook(),
		"postToolUse":        makeHook(),
		"postToolUseFailure": makeHook(),
		// Subagent (Task tool) lifecycle
		"subagentStart": makeHook(),
		"subagentStop":  makeHook(),
		// Shell command execution
		"beforeShellExecution": makeHook(),
		"afterShellExecution":  makeHook(),
		// MCP tool execution
		"beforeMCPExecution": makeHook(),
		"afterMCPExecution":  makeHook(),
		// File access and edits
		"beforeReadFile": makeHook(),
		"afterFileEdit":  makeHook(),
		// Prompts and agent output
		"beforeSubmitPrompt": makeHook(),
		"afterAgentResponse": makeHook(),
		"afterAgentThought":  makeHook(),
		// Context window
		"preCompact": makeHook(),
		// Stop
		"stop": makeHook(),
		// Tab (inline completions) — separate policy from Agent operations
		"beforeTabFileRead": makeHook(),
		"afterTabFileEdit":  makeHook(),
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

	// Strip any of OUR previously-installed hook entries first so events we
	// no longer ship (e.g. WorktreeCreate, which we used to register but now
	// deliberately skip) get cleaned up rather than lingering forever. Any
	// hook whose value doesn't reference "promptconduit" is left alone —
	// that's the user's own configuration.
	if existingHooks, ok := settings["hooks"].(map[string]interface{}); ok {
		for name, config := range existingHooks {
			if containsPromptConduit(config) {
				delete(existingHooks, name)
			}
		}
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
	fmt.Println("\nMake sure you have configured your API key:")
	fmt.Println("  promptconduit config set --api-key=\"your-api-key\"")

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
