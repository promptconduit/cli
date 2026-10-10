package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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
one-minute cooldown and tries every queued event once, stopping early if the
server stops accepting events (unreachable, rate limited, bad credentials).
Events that fail individually stay queued for a later retry.

Only the queue for the currently configured API URL is replayed. Nothing is
sent in Free / local-only mode. 'promptconduit sync' runs the same drain at
the end of a full sync.

Exits non-zero when queued events could not be sent because of a server or
credential failure, or when another process is replaying the queue.

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
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return flushOutboxNow(ctx, cmd.OutOrStdout(), client.LoadConfig(), eventsFlushDryRun, false)
}

// errFlushBusy: another process holds the outbox flush lock; nothing was sent.
var errFlushBusy = errors.New("another process is replaying the queue right now; nothing was sent, try again in a moment")

// flushProgressEvery: print a progress line every this many sends.
const flushProgressEvery = 100

// flushOutboxNow drains the outbox for cfg's API URL, printing progress and a
// summary to w. With quiet set (the tail of `sync`) it prints nothing when the
// queue is empty or sending is off. It returns an error when queued events
// could not be sent or the queue could not be updated, so `events flush` exits
// non-zero; callers that must not fail (sync) only report it.
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
	res, stopErr := c.DrainOutbox(ctx, func(st eventlog.FlushStats) {
		if st.Tried%flushProgressEvery == 0 {
			_, _ = fmt.Fprintf(w, "  %d/%d tried · %d delivered\n", st.Tried, queued, st.Delivered)
		}
	})

	if res.Busy {
		return errFlushBusy
	}
	if res.Err != nil {
		// The sends happened, but the queue file couldn't be rewritten, so
		// nothing was removed from it.
		return fmt.Errorf("sent %d queued event(s) (%d delivered) but couldn't update the replay queue: %w; "+
			"they stay queued and will be resent later (the server ignores duplicates)", res.Tried, res.Delivered, res.Err)
	}

	summary := fmt.Sprintf("Flush complete: %d delivered · %d rejected", res.Delivered, res.Rejected)
	if res.Skipped > 0 {
		summary += fmt.Sprintf(" · %d failed (kept)", res.Skipped)
	}
	if res.Expired > 0 {
		summary += fmt.Sprintf(" · %d expired", res.Expired)
	}
	_, _ = fmt.Fprintf(w, "%s · %d remaining\n", summary, res.Remaining)

	switch {
	case res.Stopped && res.Remaining > 0:
		if stopErr != nil {
			return fmt.Errorf("stopped early, %d event(s) still queued: %w", res.Remaining, stopErr)
		}
		return fmt.Errorf("stopped early, %d event(s) still queued", res.Remaining)
	case res.Remaining > 0:
		_, _ = fmt.Fprintln(w, "  Events that failed (or arrived during the flush) stay queued and will be retried later.")
	}
	return nil
}

// blankLineBefore writes a blank line before the first write, so an optional
// section (sync's queue drain) is separated only when it prints something.
type blankLineBefore struct {
	w       io.Writer
	started bool
}

func (b *blankLineBefore) Write(p []byte) (int, error) {
	if !b.started && len(p) > 0 {
		b.started = true
		if _, err := b.w.Write([]byte("\n")); err != nil {
			return 0, err
		}
	}
	return b.w.Write(p)
}
