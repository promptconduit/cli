package cmd

import (
	"fmt"

	"github.com/promptconduit/cli/internal/client"
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

	// Free / local-only mode sends nothing, so there's no cloud connection to
	// test. Report it plainly rather than failing on a missing key.
	if cfg.LocalOnly {
		fmt.Println("Local-only mode: events are captured locally and never sent, so there's nothing to test.")
		fmt.Println("Enable cloud sync with: promptconduit config set --local-only=false (and set an API key).")
		return nil
	}

	if !cfg.IsConfigured() {
		fmt.Println("Local-only mode: no API key is set, so events are captured locally and nothing is sent.")
		fmt.Println("Enable cloud sync with: promptconduit config set --api-key=\"your-api-key\"")
		return nil
	}

	fmt.Printf("Testing connection to %s...\n", cfg.APIURL)

	// Send a test request using the new envelope-based API
	apiClient := client.NewClient(cfg, Version)
	response := apiClient.TestConnection()

	if response.Success {
		fmt.Println("Success! API connection verified.")
		fmt.Printf("  Status: %d\n", response.StatusCode)
		return nil
	}

	return fmt.Errorf("API test failed: %s", response.Error)
}
