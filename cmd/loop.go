package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/promptconduit/cli/internal/loopui"
	"github.com/spf13/cobra"
)

var (
	loopAddr     string
	loopBackfill int
	loopNoOpen   bool
)

var loopCmd = &cobra.Command{
	Use:           "loop",
	Short:         "See why each prompt cost what it did (The Loop, in your browser)",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Serve The Loop in your browser: each prompt broken into its model
round-trips, with the context the model re-read on every call, what that cost,
the tools each call ran, and what would have made it cheaper (fewer
round-trips, a fresh session or subagent, a model switch, cache expiry). Almost
all agent cost is the model re-reading the conversation, so a prompt costs
roughly context size × round-trips. It updates live as your agent works.

100% local. Nothing is sent anywhere: the page reads
~/.promptconduit/events.jsonl on your machine. Works on the Free tier and
needs no editor: the same view as the editor extension's Loop panel.

Examples:
  promptconduit loop                        # open The Loop in your browser
  promptconduit loop --addr 127.0.0.1:4321  # pick the address/port
  promptconduit loop --no-open              # just print the URL (e.g. over SSH)
  promptconduit loop --backfill 5000        # seed fewer historical events`,
	RunE: runLoop,
}

// graphCmd is the old name, kept as a hidden alias so scripts keep working.
var graphCmd = &cobra.Command{
	Use:           "graph",
	Short:         "Deprecated: use `promptconduit loop`",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "graph is now `promptconduit loop`")
		return runLoop(cmd, args)
	},
}

func init() {
	for _, c := range []*cobra.Command{loopCmd, graphCmd} {
		c.Flags().StringVar(&loopAddr, "addr", "127.0.0.1:4320", "address to serve The Loop on")
		c.Flags().IntVar(&loopBackfill, "backfill", 10000, "recent events to seed before going live (raise for very long sessions)")
		c.Flags().BoolVar(&loopNoOpen, "no-open", false, "don't open a browser, just print the URL")
	}
}

func runLoop(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Reading events.jsonl is independent of the write gate, but if the log is
	// disabled there'll be no NEW events. Warn so an empty page isn't a mystery.
	if cfg := client.LoadConfig(); !cfg.EventLogEnabled() {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"Note: the local event log is disabled (PROMPTCONDUIT_EVENT_LOG=0); showing existing history only.")
	}

	srv, err := loopui.New(ctx, loopui.Options{Addr: loopAddr, Backfill: loopBackfill})
	if err != nil {
		return fmt.Errorf("start loop server: %w", err)
	}

	url := "http://" + loopAddr
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "The Loop → %s\n", url)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Reading %s. Ctrl-C to stop.\n", eventlog.EventsJSONLPath())
	if !loopNoOpen {
		if err := openBrowserURL(url); err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Could not open a browser automatically. Open the URL above.")
		}
	}

	// Ctrl-C / SIGTERM is the normal way to stop the server; treat it as a
	// clean exit so cobra doesn't print a "context canceled" banner.
	return srv.Run(ctx)
}
