package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

var feedbackCategory string

var feedbackCmd = &cobra.Command{
	Use:          "feedback [message]",
	Short:        "Send feedback to the PromptConduit team",
	SilenceUsage: true, // a runtime error shouldn't dump usage, but DO print it
	Long: `Send a note straight to the PromptConduit team — an idea, a bug, or
just what you think. Pass the message as an argument or pipe it on stdin.

If you're logged in (an API key is configured) the feedback is tied to your
account so we can follow up; otherwise it's sent anonymously.

Examples:
  promptconduit feedback "the graph view is great, add filtering"
  promptconduit feedback --category bug "sync failed on a large repo"
  echo "loving the cache savings stat" | promptconduit feedback`,
	RunE: runFeedback,
}

func init() {
	feedbackCmd.Flags().StringVar(&feedbackCategory, "category", "", "optional: bug | idea | praise | other")
}

func runFeedback(cmd *cobra.Command, args []string) error {
	message := strings.TrimSpace(strings.Join(args, " "))
	if message == "" {
		// Fall back to a piped message (`echo "..." | promptconduit feedback`).
		// Guard on ModeNamedPipe specifically so we never block reading a TTY or
		// an inherited open stdin that will never send EOF.
		if info, _ := os.Stdin.Stat(); info != nil && info.Mode()&os.ModeNamedPipe != 0 {
			piped, _ := io.ReadAll(os.Stdin)
			message = strings.TrimSpace(string(piped))
		}
	}
	if message == "" {
		return fmt.Errorf("no message — pass it as an argument or pipe it on stdin\n  promptconduit feedback \"your note\"")
	}
	if len(message) > 4000 {
		return fmt.Errorf("message is too long (%d chars, max 4000)", len(message))
	}

	cfg := client.LoadConfig()

	payload := map[string]any{
		"message": message,
		"source":  "cli",
		"context": map[string]string{
			"cli_version": Version,
			"os":          runtime.GOOS,
			"arch":        runtime.GOARCH,
		},
	}
	if feedbackCategory != "" {
		payload["category"] = feedbackCategory
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode feedback: %w", err)
	}

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.APIURL+"/v1/feedback", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("PromptConduit-CLI/%s", Version))
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("send feedback: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("feedback failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Thanks — your feedback landed. We read every note.")
	return nil
}
