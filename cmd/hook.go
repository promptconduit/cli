package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/git"
	"github.com/spf13/cobra"
)

var (
	sendEvent bool
)

var hookCmd = &cobra.Command{
	Use:    "hook",
	Short:  "Process hook events from AI tools",
	Long:   `Internal command called by AI tool hooks. Reads JSON events from stdin and sends to API.`,
	Hidden: true,
	RunE:   runHook,
}

func init() {
	hookCmd.Flags().BoolVar(&sendEvent, "send-event", false, "Send event data from stdin (internal use)")
}

func runHook(cmd *cobra.Command, args []string) error {
	if sendEvent {
		return sendEnvelopeFromStdin()
	}
	return processHookEvent()
}

// processHookEvent is the main hook entry point - wraps native event in envelope
func processHookEvent() error {
	defer outputContinueResponse()

	fileLog("Hook started")

	// Read raw input from stdin
	rawInput, err := io.ReadAll(os.Stdin)
	if err != nil {
		debugLog("Failed to read stdin: %v", err)
		fileLog("Failed to read stdin: %v", err)
		return nil
	}

	if len(rawInput) == 0 {
		debugLog("Empty input, skipping")
		fileLog("Empty input, skipping")
		return nil
	}

	previewLen := len(rawInput)
	if previewLen > 200 {
		previewLen = 200
	}
	fileLog("Received %d bytes: %s", len(rawInput), string(rawInput[:previewLen]))

	// Parse just enough to detect tool and event name
	var nativeEvent map[string]interface{}
	if err := json.Unmarshal(rawInput, &nativeEvent); err != nil {
		debugLog("Failed to parse JSON: %v", err)
		fileLog("Failed to parse JSON: %v", err)
		return nil
	}

	// Load config
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		debugLog("API key not configured, skipping")
		fileLog("API key not configured, skipping")
		return nil
	}

	// Detect tool (simple heuristics)
	tool := detectTool(nativeEvent)
	hookEvent := getHookEventName(nativeEvent)

	fileLog("Detected tool: %s, hook event: %s", tool, hookEvent)

	// Extract git context from working directory
	var gitCtx *envelope.GitContext
	if cwd := getWorkingDirectory(nativeEvent); cwd != "" {
		gitCtx = git.ExtractContext(cwd)
		if gitCtx != nil {
			fileLog("Extracted git context: repo=%s, branch=%s", gitCtx.RepoName, gitCtx.Branch)
		}
	}

	// Create envelope with raw payload
	env := envelope.New(Version, tool, hookEvent, rawInput, gitCtx)

	fileLog("Created envelope: tool=%s, event=%s", tool, hookEvent)

	// Send async
	apiClient := client.NewClient(cfg, Version)
	if err := apiClient.SendEnvelopeAsync(env); err != nil {
		debugLog("Failed to send envelope async: %v", err)
		fileLog("Failed to send envelope async: %v", err)
	}

	fileLog("Envelope queued for async send")
	return nil
}

// detectTool identifies which AI tool generated the event
func detectTool(event map[string]interface{}) string {
	// Check environment variable override first
	if tool := os.Getenv(client.EnvTool); tool != "" {
		return tool
	}

	// Claude Code: has hook_event_name field
	if _, ok := event["hook_event_name"]; ok {
		return "claude-code"
	}

	// Cursor: has cursor_version field
	if _, ok := event["cursor_version"]; ok {
		return "cursor"
	}

	// Gemini: has gemini_session field
	if _, ok := event["gemini_session"]; ok {
		return "gemini-cli"
	}

	// Generic: has event field
	if _, ok := event["event"]; ok {
		return "unknown"
	}

	return "unknown"
}

// getHookEventName extracts the hook event name from native event
func getHookEventName(event map[string]interface{}) string {
	// Claude Code uses hook_event_name
	if name, ok := event["hook_event_name"].(string); ok {
		return name
	}

	// Generic event field
	if name, ok := event["event"].(string); ok {
		return name
	}

	return ""
}

// getWorkingDirectory extracts working directory from native event
func getWorkingDirectory(event map[string]interface{}) string {
	// Claude Code uses cwd
	if cwd, ok := event["cwd"].(string); ok {
		return cwd
	}

	// Cursor might use workspace_dir
	if dir, ok := event["workspace_dir"].(string); ok {
		return dir
	}

	// Fallback to current directory
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}

	return ""
}

// sendEnvelopeFromStdin sends envelope data directly (called by async subprocess)
func sendEnvelopeFromStdin() error {
	fileLog("Async subprocess started")

	inputData, err := io.ReadAll(os.Stdin)
	if err != nil {
		fileLog("Async subprocess failed to read stdin: %v", err)
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	fileLog("Async subprocess received %d bytes", len(inputData))

	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		fileLog("Async subprocess: API key not configured")
		return fmt.Errorf("API key not configured")
	}

	fileLog("Async subprocess sending to API: %s", cfg.APIURL)
	apiClient := client.NewClient(cfg, Version)
	err = apiClient.SendEnvelopeDirect(inputData)
	if err != nil {
		fileLog("Async subprocess API error: %v", err)
		return err
	}
	fileLog("Async subprocess: envelope sent successfully")
	return nil
}

// outputContinueResponse writes the success response to stdout
func outputContinueResponse() {
	response := map[string]interface{}{
		"continue": true,
	}
	data, _ := json.Marshal(response)
	fmt.Println(string(data))
}

// debugLog logs a message only if debug mode is enabled
func debugLog(format string, args ...interface{}) {
	cfg := client.LoadConfig()
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "[promptconduit] "+format+"\n", args...)
	}
}

// fileLog logs a message to a file (for debugging hook issues)
func fileLog(format string, args ...interface{}) {
	cfg := client.LoadConfig()
	if !cfg.Debug {
		return
	}
	logPath := filepath.Join(os.TempDir(), "promptconduit-hook.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	f.WriteString(fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), msg))
}
