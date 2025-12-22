package cmd

import (
	"fmt"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/schema"
	"github.com/spf13/cobra"
)

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Test API connectivity",
	Long: `Send a test event to the PromptConduit API to verify connectivity and authentication.

Prerequisites:
  - PROMPTCONDUIT_API_KEY must be set`,
	RunE: runTest,
}

func runTest(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()

	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Set PROMPTCONDUIT_API_KEY environment variable")
	}

	fmt.Printf("Testing connection to %s...\n", cfg.APIURL)

	// Create a test event
	event := schema.NewCanonicalEvent(schema.ToolClaudeCode, schema.EventSessionStart, Version)
	source := "cli_test"
	event.Session = &schema.SessionPayload{
		Source: &source,
	}

	// Send the event
	apiClient := client.NewClient(cfg, Version)
	response := apiClient.SendEvent(event)

	if response.Success {
		fmt.Println("Success! API connection verified.")
		fmt.Printf("  Status: %d\n", response.StatusCode)
		return nil
	}

	return fmt.Errorf("API test failed: %s", response.Error)
}
