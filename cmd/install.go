package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/outbound"
	"github.com/spf13/cobra"
)

var installCmd = &cobra.Command{
	Use:   "install [tool...]",
	Short: "Set up PromptConduit for one or more AI tools",
	Long: `Set up PromptConduit hooks for your AI coding assistants.

Run with no arguments to pick interactively, or name one or more tools:
  promptconduit install                      # interactive multi-select
  promptconduit install cursor               # a single tool
  promptconduit install cursor claude-code   # several at once
  promptconduit install all                  # every supported tool

Supported tools:
  - claude-code: Claude Code CLI            (~/.claude/settings.json)
  - cursor:      Cursor IDE                 (~/.cursor/hooks.json)
  - gemini-cli:  Gemini CLI                 (~/.gemini/settings.json; also accepts "gemini")
  - codex:       OpenAI Codex CLI           (~/.codex/hooks.json)
  - copilot:     GitHub Copilot CLI         (~/.copilot/hooks/promptconduit.json)

The hooks capture events locally and realtime cost tracking works immediately.
This is the Free tier: 100% local — every event is captured to
~/.promptconduit/events.jsonl and nothing is sent anywhere. Set an API key
(promptconduit config set --api-key=...) only if you also want to sync events to
the PromptConduit platform. To stay local-only even after setting a key, pass
--local-only.`,
	Args: cobra.ArbitraryArgs,
	RunE: runInstall,
}

// installLocalOnly opts the install into Free / local-only mode: events are
// captured to the local event log but never sent to the cloud.
var installLocalOnly bool

func init() {
	installCmd.Flags().BoolVar(&installLocalOnly, "local-only", false,
		"Free / local-only mode: capture events locally, never send to the cloud")
}

// installableTools is the ordered set offered in the interactive picker.
var installableTools = []struct{ name, label string }{
	{"claude-code", "Claude Code"},
	{"cursor", "Cursor"},
	{"gemini-cli", "Gemini CLI"},
	{"codex", "OpenAI Codex"},
	{"copilot", "GitHub Copilot"},
}

func runInstall(cmd *cobra.Command, args []string) error {
	tools, err := resolveInstallTools(cmd, args)
	if err != nil {
		return err
	}
	if len(tools) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "No tools selected.")
		return nil
	}

	if installLocalOnly {
		if err := persistLocalOnly(true); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not save local-only setting: %v\n", err)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Free / local-only mode enabled — events are captured locally and never sent.")
			fmt.Fprintln(cmd.OutOrStdout())
		}
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = stableExecutablePath(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	for i, tool := range tools {
		if i > 0 {
			fmt.Fprintln(cmd.OutOrStdout())
		}
		if err := installTool(tool, exePath); err != nil {
			return fmt.Errorf("install %s: %w", tool, err)
		}
	}
	return nil
}

// persistLocalOnly writes the local_only flag into the config file so the Free
// tier survives across hook invocations. Mirrors how `config set --local-only`
// persists the same setting.
func persistLocalOnly(v bool) error {
	fc, err := client.LoadFileConfig()
	if err != nil {
		return err
	}
	if fc == nil {
		fc = &client.FileConfig{}
	}
	fc.LocalOnly = v
	return client.SaveFileConfig(fc)
}

// stableExecutablePath returns an absolute path to the running binary that is
// safe to bake into AI-tool hook configs.
//
// It resolves symlinks to canonicalize the path, but for Homebrew installs it
// rewrites the result back to Homebrew's version-independent symlink (e.g.
// /opt/homebrew/opt/promptconduit/bin/promptconduit). Homebrew stores each
// version under .../Cellar/<formula>/<version>/ and deletes the old directory
// on `brew upgrade`, so a fully-resolved Cellar path written into a hook stops
// working the moment the user upgrades. The opt-prefix (and linked bin)
// symlinks always point at the current keg, so the hook survives upgrades.
func stableExecutablePath(exePath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", err
	}
	if stable, ok := homebrewStablePath(resolved); ok {
		return stable, nil
	}
	return resolved, nil
}

// homebrewStablePath maps a fully-resolved Homebrew Cellar binary path to a
// version-independent symlink that resolves back to it. It returns ok=false for
// non-Homebrew paths, or when no such symlink is present (in which case callers
// keep the resolved path — the previous behavior).
func homebrewStablePath(resolved string) (string, bool) {
	const marker = "/Cellar/"
	idx := strings.Index(resolved, marker)
	if idx < 0 {
		return "", false
	}
	prefix := resolved[:idx]           // e.g. /opt/homebrew
	rest := resolved[idx+len(marker):] // <formula>/<version>/bin/<binary>
	formula, _, ok := strings.Cut(rest, "/")
	if !ok || formula == "" {
		return "", false
	}
	binName := filepath.Base(resolved)
	// Prefer the opt-prefix symlink (Homebrew's canonical stable reference),
	// then the linked bin symlink. Only accept a candidate that resolves back
	// to the same binary, so a hook never points at a different formula.
	for _, candidate := range []string{
		filepath.Join(prefix, "opt", formula, "bin", binName),
		filepath.Join(prefix, "bin", binName),
	} {
		if target, err := filepath.EvalSymlinks(candidate); err == nil && target == resolved {
			return candidate, true
		}
	}
	return "", false
}

// installTool dispatches to the per-tool installer.
func installTool(name, exePath string) error {
	switch name {
	case "claude-code":
		return installClaudeCode(exePath)
	case "cursor":
		return installCursor(exePath)
	case "gemini-cli", "gemini":
		return installGemini(exePath)
	case "codex":
		return installCodex(exePath)
	case "copilot":
		return installCopilot(exePath)
	default:
		return fmt.Errorf("installation not implemented for: %s", name)
	}
}

// resolveInstallTools turns CLI args — or an interactive prompt when none are
// given — into a validated, de-duplicated list of tool names. "all" expands to
// every supported tool.
func resolveInstallTools(cmd *cobra.Command, args []string) ([]string, error) {
	if len(args) == 0 {
		if !outbound.IsTerminal(os.Stdin) {
			return nil, fmt.Errorf("name one or more tools (e.g. `install cursor claude-code` or `install all`), or run in a terminal to choose interactively.\nSupported: %s", strings.Join(toolNames(), ", "))
		}
		return selectToolsInteractive(cmd)
	}
	seen := map[string]bool{}
	var out []string
	for _, tok := range args {
		t := normalizeToolName(strings.ToLower(strings.TrimSpace(tok)))
		if t == "all" {
			return toolNames(), nil
		}
		if !envelope.IsValidTool(t) {
			return nil, fmt.Errorf("unknown tool: %s. Supported: %s (or 'all')", tok, strings.Join(toolNames(), ", "))
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// selectToolsInteractive prompts the user to pick tools by number or name.
func selectToolsInteractive(cmd *cobra.Command) ([]string, error) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Which AI tools should PromptConduit set up?")
	for i, t := range installableTools {
		fmt.Fprintf(out, "  %d) %s\n", i+1, t.label)
	}
	fmt.Fprint(out, "Enter numbers (e.g. 1,2), names, or 'all': ")

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return nil, nil
	}
	return parseToolSelection(line)
}

// parseToolSelection resolves a comma-separated picker response (numbers,
// names, or "all").
func parseToolSelection(input string) ([]string, error) {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return nil, nil
	}
	if input == "all" || input == "*" {
		return toolNames(), nil
	}
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.Split(input, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		var name string
		if n, err := strconv.Atoi(tok); err == nil {
			if n < 1 || n > len(installableTools) {
				return nil, fmt.Errorf("invalid choice: %s (pick 1-%d)", tok, len(installableTools))
			}
			name = installableTools[n-1].name
		} else {
			name = normalizeToolName(tok)
			if !envelope.IsValidTool(name) {
				return nil, fmt.Errorf("unknown tool: %s", tok)
			}
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func normalizeToolName(s string) string {
	if s == "gemini" {
		return "gemini-cli"
	}
	return s
}

func toolNames() []string {
	names := make([]string, len(installableTools))
	for i, t := range installableTools {
		names[i] = t.name
	}
	return names
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

	fmt.Println("✓ Installed PromptConduit hooks for Claude Code")
	fmt.Printf("  %s\n", settingsPath)
	fmt.Println()
	fmt.Println("Realtime token-cost tracking works for Claude Code too (from local transcripts):")
	fmt.Println("  Live spend:    promptconduit cost watch      (or install the editor extension)")
	fmt.Println("  This session:  promptconduit cost")
	fmt.Println()
	fmt.Println("Optional — also sync events to the PromptConduit platform:")
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

	fmt.Println("✓ Installed PromptConduit hooks for Cursor")
	fmt.Printf("  %s\n", settingsPath)
	fmt.Println()
	fmt.Println("Realtime token-cost tracking is now ON for Cursor — computed 100% locally.")
	fmt.Println("  Live spend:    promptconduit cost watch      (or install the editor extension)")
	fmt.Println("  This session:  promptconduit cost")
	fmt.Println()
	fmt.Println("Optional — also sync events to the PromptConduit platform:")
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

	fmt.Println("✓ Installed PromptConduit hooks for Gemini CLI")
	fmt.Printf("  %s\n", settingsPath)
	fmt.Println()
	fmt.Println("Optional — sync events to the PromptConduit platform:")
	fmt.Println("  promptconduit config set --api-key=\"your-api-key\"")

	return nil
}

// buildGeminiHooks registers for every event Gemini CLI's hooks reference
// exposes (https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/reference.md).
// Matchers are only meaningful on BeforeTool/AfterTool (regex on tool name);
// other events use `matcher: "*"` only when the spec calls for matcher
// filtering, otherwise we omit it.
//
// NOTE: AfterModel fires per response chunk during streaming, so it can be
// high-volume on long Gemini turns. We register it for completeness —
// downstream consumers (the platform + `promptconduit watch`) can filter
// or sample if it becomes a problem in practice.
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

	plainEvent := func() []map[string]interface{} {
		return []map[string]interface{}{{"hooks": makeHook(5000)}}
	}

	return map[string]interface{}{
		// Session lifecycle
		"SessionStart": plainEvent(),
		"SessionEnd":   plainEvent(),
		// Per-turn lifecycle
		"BeforeAgent": plainEvent(), // user submitted a prompt, before planning
		"AfterAgent":  plainEvent(), // final turn response produced
		// Model interaction (BeforeModel fires per LLM call; AfterModel
		// fires per response chunk — see note above)
		"BeforeModel":         plainEvent(),
		"AfterModel":          plainEvent(),
		"BeforeToolSelection": plainEvent(),
		// Tool execution (matcher = regex on tool name)
		"BeforeTool": makeMatcherHook(5000),
		"AfterTool":  makeMatcherHook(5000),
		// Context compaction
		"PreCompress": plainEvent(),
		// System alerts
		"Notification": plainEvent(),
	}
}

// isCommandAvailable checks if a command exists in PATH
func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// installCodex registers our hook handler with OpenAI Codex CLI.
//
// Codex's config format mirrors Claude Code's (event keys → matcher groups →
// hook handlers), with two important differences from Claude Code:
//   - timeout is in SECONDS (not milliseconds).
//   - SessionEnd does not exist; sessions only emit SessionStart.
//
// We pass `--tool codex` to our hook binary because Codex's payload uses the
// same `hook_event_name` field name as Claude Code, so the existing
// detectTool heuristic would otherwise mistag the events.
//
// Spec: https://developers.openai.com/codex/hooks
func installCodex(exePath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	settingsPath := filepath.Join(homeDir, ".codex", "hooks.json")

	// Read existing settings or create new
	settings := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("failed to parse existing settings: %w", err)
		}
	}

	hookCmd := fmt.Sprintf("%s hook --tool codex", exePath)
	hooks := buildCodexHooks(hookCmd)

	// Strip any of OUR previously-installed entries first so removed events
	// (or stale --tool flags from earlier versions) self-heal on re-install.
	// User-owned hook entries are left alone.
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

	fmt.Println("✓ Installed PromptConduit hooks for OpenAI Codex CLI")
	fmt.Printf("  %s\n", settingsPath)
	fmt.Println()
	fmt.Println("Optional — sync events to the PromptConduit platform:")
	fmt.Println("  promptconduit config set --api-key=\"your-api-key\"")

	return nil
}

// buildCodexHooks registers for every event in the Codex CLI hooks reference
// (https://developers.openai.com/codex/hooks). Codex's matcher rules:
//   - SessionStart matches start source: startup|resume|clear
//   - PreToolUse / PostToolUse / PermissionRequest match tool name
//   - UserPromptSubmit and Stop do not support matchers
//
// Codex's timeout field is in seconds; 30 is plenty for our hook
// (read stdin → spawn async send → return).
func buildCodexHooks(hookCmd string) map[string]interface{} {
	makeHook := func(timeoutSec int) []map[string]interface{} {
		return []map[string]interface{}{
			{
				"type":    "command",
				"command": hookCmd,
				"timeout": timeoutSec,
			},
		}
	}

	makeMatcherHook := func(timeoutSec int) []map[string]interface{} {
		return []map[string]interface{}{
			{
				"matcher": "*",
				"hooks":   makeHook(timeoutSec),
			},
		}
	}

	plainEvent := func() []map[string]interface{} {
		return []map[string]interface{}{{"hooks": makeHook(30)}}
	}

	return map[string]interface{}{
		// Session lifecycle (matcher = startup|resume|clear)
		"SessionStart": makeMatcherHook(30),
		// Per-turn (no matcher support)
		"UserPromptSubmit": plainEvent(),
		"Stop":             plainEvent(),
		// Tool execution (matcher = tool name regex: Bash, apply_patch, mcp__*)
		"PreToolUse":        makeMatcherHook(30),
		"PostToolUse":       makeMatcherHook(30),
		"PermissionRequest": makeMatcherHook(30),
	}
}

// CopilotHookFile is the basename we write into ~/.copilot/hooks/. Copilot
// loads every *.json in that directory, so owning a dedicated file lets us
// uninstall cleanly without parsing or merging anything else.
const CopilotHookFile = "promptconduit.json"

// installCopilot registers our hook handler with GitHub Copilot CLI.
//
// Unlike Claude Code / Codex / Gemini which use a single merged settings
// file, Copilot reads every *.json under ~/.copilot/hooks/ and combines
// them, so we own a dedicated file (promptconduit.json) — uninstall is a
// `rm` and idempotency is automatic.
//
// We pass `--tool copilot` to our hook binary because Copilot supports
// both camelCase events (its native format, with sessionId/cwd payload
// fields) AND PascalCase events (VS Code compatible, with hook_event_name
// payload field). We use camelCase below; setting --tool ensures correct
// attribution regardless of payload shape.
//
// Spec: https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-hooks-reference
func installCopilot(exePath string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	hooksDir := filepath.Join(homeDir, ".copilot", "hooks")
	settingsPath := filepath.Join(hooksDir, CopilotHookFile)

	hookCmd := fmt.Sprintf("%s hook --tool copilot", exePath)
	doc := map[string]interface{}{
		"version": 1,
		"hooks":   buildCopilotHooks(hookCmd),
	}

	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("failed to create hooks directory: %w", err)
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks: %w", err)
	}

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write hooks file: %w", err)
	}

	fmt.Println("✓ Installed PromptConduit hooks for GitHub Copilot CLI")
	fmt.Printf("  %s\n", settingsPath)
	fmt.Println()
	fmt.Println("Optional — sync events to the PromptConduit platform:")
	fmt.Println("  promptconduit config set --api-key=\"your-api-key\"")

	return nil
}

// buildCopilotHooks registers for every event in the Copilot CLI hooks
// reference (camelCase form). Copilot's command-hook field semantics:
//   - `command` is a cross-platform fallback used when neither `bash` nor
//     `powershell` is set for the current platform. Since our hook is a
//     single binary that invokes the same way on both, we use `command`
//     alone and let Copilot pick.
//   - timeoutSec, not timeout. Default is 30s which is plenty for our path.
//
// Matcher-supporting events get `"*"` for now (matches everything).
// Note: under Copilot cloud agent, `notification` and `permissionRequest`
// don't fire; harmless to register, just no-ops there.
func buildCopilotHooks(hookCmd string) map[string]interface{} {
	makeCmd := func() []map[string]interface{} {
		return []map[string]interface{}{
			{
				"type":       "command",
				"command":    hookCmd,
				"timeoutSec": 30,
			},
		}
	}

	makeMatcherCmd := func(matcher string) []map[string]interface{} {
		return []map[string]interface{}{
			{
				"type":       "command",
				"command":    hookCmd,
				"timeoutSec": 30,
				"matcher":    matcher,
			},
		}
	}

	return map[string]interface{}{
		// Session lifecycle
		"sessionStart": makeCmd(),
		"sessionEnd":   makeCmd(),
		// Per-turn / agent lifecycle
		"userPromptSubmitted": makeCmd(),
		"agentStop":           makeCmd(),
		// Subagent lifecycle (matcher = agent name regex)
		"subagentStart": makeMatcherCmd("*"),
		"subagentStop":  makeCmd(),
		// Tool execution (matcher = tool name regex on preToolUse + permissionRequest)
		"preToolUse":         makeMatcherCmd("*"),
		"postToolUse":        makeCmd(),
		"postToolUseFailure": makeCmd(),
		"permissionRequest":  makeMatcherCmd("*"),
		// Context compaction (matcher = "manual"|"auto")
		"preCompact": makeMatcherCmd("*"),
		// System / errors
		"notification":  makeMatcherCmd("*"),
		"errorOccurred": makeCmd(),
	}
}
