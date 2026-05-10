package cmd

import (
	"fmt"
	"os"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/correlation"
	"github.com/spf13/cobra"
)

var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Debugging utilities",
}

var debugTraceCmd = &cobra.Command{
	Use:   "trace <session_id>",
	Short: "Print the correlation trace tree for a session",
	Long: `Print the locally-stored trace ID and recorded parent spans for a session.

Useful for support and self-debugging when correlation IDs look wrong.
Reads from ~/.config/promptconduit/traces/<session_id>.{json,spans.json}.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID := args[0]
		store := correlation.NewStore(client.ConfigDir())

		rec, err := store.LoadTrace(sessionID)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("no trace record for session %s", sessionID)
			}
			return fmt.Errorf("read trace: %w", err)
		}

		cmd.Printf("Session:      %s\n", rec.SessionID)
		cmd.Printf("Trace ID:     %s\n", rec.TraceID)
		cmd.Printf("Created:      %s\n", rec.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
		cmd.Printf("Last seen:    %s\n", rec.LastSeenAt.Format("2006-01-02T15:04:05Z07:00"))

		spans, err := store.LoadSpans(sessionID)
		if err != nil {
			return fmt.Errorf("read spans: %w", err)
		}

		cmd.Println()
		cmd.Println("Recorded parent spans:")
		if spans.RootSpan != "" {
			cmd.Printf("  root_span:          %s\n", spans.RootSpan)
		}
		if spans.LastPromptSubmit != "" {
			cmd.Printf("  last_prompt_submit: %s\n", spans.LastPromptSubmit)
		}
		printSpanMap(cmd, "tool_uses", spans.ToolUses)
		printSpanMap(cmd, "subagents", spans.Subagents)
		printSpanMap(cmd, "tasks", spans.Tasks)
		printSpanMap(cmd, "elicitations", spans.Elicitations)
		printSpanMap(cmd, "context_compacts", spans.ContextCompacts)

		return nil
	},
}

func printSpanMap(cmd *cobra.Command, label string, m map[string]string) {
	if len(m) == 0 {
		return
	}
	cmd.Printf("  %s:\n", label)
	for k, v := range m {
		cmd.Printf("    %s -> %s\n", k, v)
	}
}

func init() {
	debugCmd.AddCommand(debugTraceCmd)
}
