package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/promptconduit/cli/internal/graph"
	"github.com/spf13/cobra"
)

var (
	graphAddr     string
	graphBackfill int
	graphNoOpen   bool
)

var graphCmd = &cobra.Command{
	Use:           "graph",
	Short:         "Watch your AI coding sessions as a live, breathing graph",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Serve a live Session Graph in your browser — one AI coding session
rendered as a growing tree: the session, each prompt→Stop turn (with its
tool activity and cost), and every subagent it spawned, branching off as
nested nodes. Running nodes pulse; worktrees get a badge. It updates in
place as your agent works, reading straight from the local event log.

100% local. Nothing is sent anywhere — the page reads
~/.promptconduit/events.jsonl on your machine. Works on the Free tier and
needs no editor: the same graph the Cursor/VS Code extension shows, in any
browser.

Examples:
  promptconduit graph                 # open the live graph in your browser
  promptconduit graph --port 4320     # pick the port
  promptconduit graph --no-open       # just print the URL (e.g. over SSH)
  promptconduit graph --backfill 5000 # seed fewer historical events`,
	RunE: runGraph,
}

func init() {
	graphCmd.Flags().StringVar(&graphAddr, "addr", "127.0.0.1:4320", "address to serve the graph on")
	graphCmd.Flags().IntVar(&graphBackfill, "backfill", 10000, "recent events to seed before going live (raise for very long sessions)")
	graphCmd.Flags().BoolVar(&graphNoOpen, "no-open", false, "don't open a browser, just print the URL")
}

func runGraph(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Reading events.jsonl is independent of the write gate, but if the log is
	// disabled there'll be no NEW events — warn so an empty graph isn't a mystery.
	if cfg := client.LoadConfig(); !cfg.EventLogEnabled() {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"Note: the local event log is disabled (PROMPTCONDUIT_EVENT_LOG=0) — showing existing history only.")
	}

	srv, err := graph.New(ctx, graph.Options{Addr: graphAddr, Backfill: graphBackfill})
	if err != nil {
		return fmt.Errorf("start graph server: %w", err)
	}

	url := "http://" + graphAddr
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Live session graph → %s\n", url)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Reading %s — Ctrl-C to stop.\n", eventlog.EventsJSONLPath())
	if !graphNoOpen {
		if err := openBrowserURL(url); err != nil {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Could not open a browser automatically — open the URL above.")
		}
	}

	// Ctrl-C / SIGTERM is the normal way to stop the server; treat it as a
	// clean exit so cobra doesn't print a "context canceled" banner.
	return srv.Run(ctx)
}
