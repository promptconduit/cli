package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

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

	// Read JSON from stdin
	inputData, err := io.ReadAll(os.Stdin)
	if err != nil {
		debugLog("Failed to read stdin: %v", err)
		return nil // Don't return error - always succeed
	}

	if len(inputData) == 0 {
		debugLog("Empty input, skipping")
		return nil
	}

	// Parse native event
	var nativeEvent map[string]interface{}
	if err := json.Unmarshal(inputData, &nativeEvent); err != nil {
		debugLog("Failed to parse JSON: %v", err)
		return nil
	}

	// Load config
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		debugLog("API key not configured, skipping")
		return nil
	}

	// Detect tool
	tool := adapters.DetectTool(nativeEvent)
	if tool == "" {
		debugLog("Could not detect tool from event")
		return nil
	}

	// Get adapter for tool
	adapter := adapters.GetAdapter(tool, cfg.Debug)
	if adapter == nil {
		debugLog("No adapter for tool: %s", tool)
		return nil
	}

	// Translate event to canonical format
	canonicalEvent := adapter.TranslateEvent(nativeEvent)
	if canonicalEvent == nil {
		debugLog("Event translation returned nil (unsupported event)")
		return nil
	}

	// Send event asynchronously (non-blocking)
	apiClient := client.NewClient(cfg, Version)
	if err := apiClient.SendEventAsync(canonicalEvent); err != nil {
		debugLog("Failed to send event async: %v", err)
		// Don't return error - always succeed
	}

	return nil
}

// sendEventFromStdin sends event data directly (called by async subprocess)
func sendEventFromStdin() error {
	inputData, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured")
	}

	apiClient := client.NewClient(cfg, Version)
	return apiClient.SendEventDirect(inputData)
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
