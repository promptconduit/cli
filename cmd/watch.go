package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/outbound"
	"github.com/spf13/cobra"
)

var (
	watchVerbose bool
	watchLines   int
)

var watchCmd = &cobra.Command{
	Use:           "watch",
	Short:         "Tail outbound HTTP traffic from the CLI in real time",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Stream every HTTP request the CLI makes to the platform — hook
envelope sends, transcript syncs, insights queries, skills traffic, the
whole lot — to your terminal as it happens.

Each request appears as a one-line summary by default:

  15:30:42  POST  /v1/events/raw  3.2KB  → 200 (87ms)

Use --verbose to also print the request body (and response body, when
the server returned one) pretty-printed beneath the summary. Useful for
checking what your hook is actually sending when events aren't showing
up on the dashboard.

The underlying file lives at ~/.config/promptconduit/outbound.ndjson
(mode 0600). Authorization, Cookie, and similar credential headers are
redacted before write; request bodies are capped at 64KB per row and
the file rotates to outbound.ndjson.1 at 50MB.

Examples:
  promptconduit watch                 # tail live traffic
  promptconduit watch --verbose       # include full bodies
  promptconduit watch --lines 20      # backfill the last 20 entries`,
	RunE: runWatch,
}

func init() {
	watchCmd.Flags().BoolVarP(&watchVerbose, "verbose", "v", false, "include full request/response bodies under each summary line")
	watchCmd.Flags().IntVar(&watchLines, "lines", 0, "backfill the last N entries before going live")
}

func runWatch(cmd *cobra.Command, args []string) error {
	path := filepath.Join(client.ConfigDir(), outbound.MirrorFileName)
	color := outbound.IsTerminal(os.Stdout)

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(cmd.ErrOrStderr(), "Watching %s — Ctrl-C to stop.\n", path)

	lines := outbound.Tail(ctx, path, watchLines)
	for raw := range lines {
		entry, err := outbound.ParseLine(raw)
		if err != nil {
			// Best-effort: show the unparseable raw line so the user
			// can still see something went through.
			fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			continue
		}
		fmt.Fprintln(cmd.OutOrStdout(), outbound.RenderSummary(entry, watchVerbose, color))
	}

	// Ctrl-C / SIGTERM is the normal way to leave watch; treat as a
	// successful exit rather than an error so cobra doesn't print a
	// "Error: context canceled" banner.
	return nil
}
