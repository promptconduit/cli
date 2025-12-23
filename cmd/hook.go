package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/adapters"
	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

var (
	sendEvent bool
)

var hookCmd = &cobra.Command{
	Use:    "hook",
	Short:  "Process hook events from AI tools",
	Long:   `Internal command called by AI tool hooks. Reads JSON events from stdin and sends to API.`,
	Hidden: true, // Hide from help since it's internal
	RunE:   runHook,
}

func init() {
	hookCmd.Flags().BoolVar(&sendEvent, "send-event", false, "Send event data from stdin (internal use)")
}

func runHook(cmd *cobra.Command, args []string) error {
	// If --send-event flag is set, we're being called as a subprocess to send the event
	if sendEvent {
		return sendEventFromStdin()
	}

	// Normal hook processing
	return processHookEvent()
}

// processHookEvent is the main hook entry point called by AI tools
func processHookEvent() error {
	// Always output success response to never block the tool
	defer outputContinueResponse()

	fileLog("Hook started")

	// Read JSON from stdin
	inputData, err := io.ReadAll(os.Stdin)
	if err != nil {
		debugLog("Failed to read stdin: %v", err)
		fileLog("Failed to read stdin: %v", err)
		return nil // Don't return error - always succeed
	}

	if len(inputData) == 0 {
		debugLog("Empty input, skipping")
		fileLog("Empty input, skipping")
		return nil
	}

	previewLen := len(inputData)
	if previewLen > 200 {
		previewLen = 200
	}
	fileLog("Received %d bytes: %s", len(inputData), string(inputData[:previewLen]))

	// Parse native event
	var nativeEvent map[string]interface{}
	if err := json.Unmarshal(inputData, &nativeEvent); err != nil {
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

	// Detect tool
	tool := adapters.DetectTool(nativeEvent)
	if tool == "" {
		debugLog("Could not detect tool from event")
		fileLog("Could not detect tool from event: %v", nativeEvent)
		return nil
	}

	fileLog("Detected tool: %s", tool)

	// Get adapter for tool
	adapter := adapters.GetAdapter(tool, cfg.Debug)
	if adapter == nil {
		debugLog("No adapter for tool: %s", tool)
		fileLog("No adapter for tool: %s", tool)
		return nil
	}

	// Translate event to canonical format
	canonicalEvent := adapter.TranslateEvent(nativeEvent)
	if canonicalEvent == nil {
		debugLog("Event translation returned nil (unsupported event)")
		fileLog("Event translation returned nil for event: %v", nativeEvent)
		return nil
	}

	fileLog("Translated event type: %s, session: %v", canonicalEvent.EventType, canonicalEvent.SessionID)

	// Send event asynchronously (non-blocking)
	apiClient := client.NewClient(cfg, Version)
	if err := apiClient.SendEventAsync(canonicalEvent); err != nil {
		debugLog("Failed to send event async: %v", err)
		fileLog("Failed to send event async: %v", err)
		// Don't return error - always succeed
	}

	fileLog("Event queued for async send")
	return nil
}

// sendEventFromStdin sends event data directly (called by async subprocess)
func sendEventFromStdin() error {
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
	err = apiClient.SendEventDirect(inputData)
	if err != nil {
		fileLog("Async subprocess API error: %v", err)
		return err
	}
	fileLog("Async subprocess: event sent successfully")
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
