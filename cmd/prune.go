package cmd

import (
	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

var pruneDryRun bool

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Trim local hook history older than the retention window",
	Long: `Remove locally-captured events older than the configured retention window
from ~/.promptconduit/events.jsonl (and the hook-events trace).

Events within the retention window are ALWAYS kept — prune can only ever remove
already-expired data. The window defaults to 30 days and is configurable:

  promptconduit config set --event-log-retention-days=90   # keep 90 days
  promptconduit config set --event-log-retention-days=-1   # keep forever

The CLI also prunes automatically in the background once the log grows past its
size ceiling; this command lets you reclaim space on demand.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runPrune,
}

func init() {
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "Report what would be removed without changing any files")
}

func runPrune(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	days := cfg.RetentionDays()

	if days == 0 {
		cmd.Println("Retention: keep forever — local hook history is never pruned.")
		cmd.Println("Set a window with: promptconduit config set --event-log-retention-days=30")
		return nil
	}

	stats := eventlog.Stats(days)
	cmd.Printf("Retention window: %d days\n", days)
	cmd.Printf("Event log:        %s (%s)\n", eventlog.EventsJSONLPath(), humanBytes(int(stats.EventsBytes)))
	cmd.Printf("Events:           %d total, %d older than %d days\n", stats.EventsTotal, stats.EventsExpired, days)
	if stats.HookTotal > 0 {
		cmd.Printf("Hook-events:      %d total, %d older than %d days\n", stats.HookTotal, stats.HookExpired, days)
	}

	expired := stats.EventsExpired + stats.HookExpired
	if expired == 0 {
		cmd.Println("\nNothing to prune — all local history is within the retention window.")
		return nil
	}

	if pruneDryRun {
		cmd.Printf("\nDry run: would remove %d expired record(s). Re-run without --dry-run to apply.\n", expired)
		return nil
	}

	removed := eventlog.PruneExpired(days)
	cmd.Printf("\nPruned %d expired record(s) older than %d days.\n", removed, days)
	return nil
}
