package cmd

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Sign in to PromptConduit via browser",
	Long: `Authenticate with your PromptConduit account using a browser-based device flow.

Opens your browser to sign in and approve CLI access. On success, your API key
is saved to ~/.config/promptconduit/config.json.

Use --no-browser to print the verification URL instead of opening it automatically.`,
	RunE: runLogin,
}

var (
	loginNoBrowser bool
	loginAPIURL    string
)

func init() {
	loginCmd.Flags().BoolVar(&loginNoBrowser, "no-browser", false, "Don't open browser automatically")
	loginCmd.Flags().StringVar(&loginAPIURL, "api-url", "", "Override API URL (default: from config or https://api.promptconduit.dev)")
}

func runLogin(cmd *cobra.Command, args []string) error {
	apiKey, err := performLogin(cmd, loginAPIURL, !loginNoBrowser)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n✓ Logged in successfully\n")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  API Key: %s\n", client.MaskAPIKey(apiKey))
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  Config:  ", client.ConfigPath())
	return nil
}

// performLogin runs the device authorization flow and saves the API key.
func performLogin(cmd *cobra.Command, apiURLOverride string, openBrowser bool) (string, error) {
	apiURL := apiURLOverride
	if apiURL == "" {
		cfg := client.LoadConfig()
		apiURL = cfg.APIURL
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Requesting device code...")

	deviceResp, err := client.RequestDeviceCode(apiURL, Version)
	if err != nil {
		return "", fmt.Errorf("failed to start login: %w", err)
	}

	verificationURL := deviceResp.VerificationURI
	if verificationURL == "" {
		verificationURL = deviceResp.VerificationURIComplete
	}

	_, _ = fmt.Fprintf(out, "\nTo sign in, visit:\n  %s\n\n", verificationURL)
	_, _ = fmt.Fprintf(out, "And enter code: %s\n\n", deviceResp.UserCode)

	if openBrowser {
		if err := openBrowserURL(verificationURL); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Could not open browser: %v\n", err)
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Open the URL above manually.")
		}
	}

	_, _ = fmt.Fprintln(out, "Waiting for authorization...")
	timeout := time.Duration(deviceResp.ExpiresIn) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}

	apiKey, err := client.PollDeviceToken(apiURL, Version, deviceResp.DeviceCode, timeout)
	if err != nil {
		return "", err
	}

	if err := client.SaveAPIKey(apiKey); err != nil {
		return "", fmt.Errorf("failed to save API key: %w", err)
	}

	return apiKey, nil
}

func openBrowserURL(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("cmd", "/c", "start", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	return c.Start()
}
