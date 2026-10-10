package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

var eventsFlushDryRun bool

var eventsFlushCmd = &cobra.Command{
	Use:          "flush",
	Short:        "Replay events queued after failed sends, now",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	Long: `Send everything waiting in the replay queue (outbox) right now.

Events whose send failed for a reason that may clear (no connection, rate
limiting, a server error) are queued and normally replayed in the background
after a later send succeeds — so the queue only drains while you keep
generating events. This command drains it on demand: it ignores the background
one-minute cooldown and keeps replaying until the queue is empty, the server
stops accepting events, or a pass makes no progress.

Only the queue for the currently configured API URL is replayed. Nothing is
sent in Free / local-only mode. 'promptconduit sync' runs the same drain at
the end of a full sync.

Examples:
  promptconduit events flush            # replay the queue now
  promptconduit events flush --dry-run  # just show how many are queued`,
	RunE: runEventsFlush,
}

func init() {
	eventsFlushCmd.Flags().BoolVar(&eventsFlushDryRun, "dry-run", false, "Only report how many events are queued; send nothing")
	eventsCmd.AddCommand(eventsFlushCmd)
}

func runEventsFlush(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return flushOutboxNow(ctx, cmd.OutOrStdout(), client.LoadConfig(), eventsFlushDryRun, false)
}

// flushOutboxNow drains the outbox for cfg's API URL, printing progress and a
// summary to w. With quiet set (the tail of `sync`) it prints nothing when the
// queue is empty or sending is off. It returns an error only when the queue
// could not be fully drained because of a failure, so `events flush` exits
// non-zero; callers that must not fail (sync) ignore it.
func flushOutboxNow(ctx context.Context, w io.Writer, cfg *client.Config, dryRun, quiet bool) error {
	if !cfg.EventLogEnabled() {
		if !quiet {
			_, _ = fmt.Fprintln(w, "Event log is disabled, so failed sends are not queued; nothing to flush.")
		}
		return nil
	}
	eventlog.SetEnabled(true)

	queued := eventlog.OutboxCount(cfg.APIURL)
	if queued == 0 {
		if !quiet {
			_, _ = fmt.Fprintf(w, "Replay queue is empty for %s.\n", cfg.APIURL)
		}
		return nil
	}
	if !cfg.ShouldSend() {
		if !quiet {
			_, _ = fmt.Fprintf(w, "%d event(s) queued for %s, but sending is off ", queued, cfg.APIURL)
			if cfg.LocalOnly {
				_, _ = fmt.Fprintln(w, "(local-only mode). Nothing was sent.")
			} else {
				_, _ = fmt.Fprintln(w, "(no API key). Nothing was sent.")
				_, _ = fmt.Fprintln(w, "  Log in with: promptconduit login")
			}
		}
		return nil
	}
	if dryRun {
		_, _ = fmt.Fprintf(w, "[dry-run] %d event(s) queued for replay to %s\n", queued, cfg.APIURL)
		return nil
	}

	_, _ = fmt.Fprintf(w, "Replaying %d queued event(s) to %s...\n", queued, cfg.APIURL)
	c := client.NewClient(cfg, Version)
	res, stopErr := c.DrainOutbox(ctx, func(pass int, st eventlog.FlushStats) {
		line := fmt.Sprintf("  pass %d: %d delivered · %d rejected", pass, st.Delivered, st.Rejected)
		if st.Skipped > 0 {
			line += fmt.Sprintf(" · %d failed (kept)", st.Skipped)
		}
		if st.Expired > 0 {
			line += fmt.Sprintf(" · %d expired", st.Expired)
		}
		_, _ = fmt.Fprintln(w, line)
	})

	if res.Busy {
		_, _ = fmt.Fprintln(w, "Another process is replaying the queue right now; try again in a moment.")
		return nil
	}
	summary := fmt.Sprintf("Flush complete: %d delivered · %d rejected", res.Delivered, res.Rejected)
	if res.Expired > 0 {
		summary += fmt.Sprintf(" · %d expired", res.Expired)
	}
	_, _ = fmt.Fprintf(w, "%s · %d remaining\n", summary, res.Remaining)

	switch {
	case res.Err != nil:
		return fmt.Errorf("could not update the replay queue: %w", res.Err)
	case res.Stopped && res.Remaining > 0:
		if stopErr != nil {
			return fmt.Errorf("stopped early, %d event(s) still queued: %w", res.Remaining, stopErr)
		}
		return fmt.Errorf("stopped early, %d event(s) still queued", res.Remaining)
	case res.Remaining > 0:
		_, _ = fmt.Fprintln(w, "  The rest failed on this attempt and stay queued; they'll be retried later.")
	}
	return nil
}
