package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/promptconduit/cli/internal/cost"
	"github.com/spf13/cobra"
)

var (
	costCwd  string
	costAll  bool
	costJSON bool
	costDays int
)

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Real-time AI token cost for your sessions (100% local)",
	Long: `Compute what your AI coding sessions cost, in real time, entirely on
your machine. The cost feature reads local transcripts, prices each turn against
a bundled rate table, and never sends any of your data to the PromptConduit
platform or anywhere else.

Subcommands:
  watch     Stream cost events as your session runs (the editor extension's feed)
  session   Print the current session's cost summary
  history   Aggregate cost over the last N days

Today this prices Claude Code sessions with exact token counts from the
transcript. Cursor's native agent (estimate + reconcile) lands in a later
milestone.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var costWatchCmd = &cobra.Command{
	Use:           "watch",
	Short:         "Stream real-time cost events (newline-delimited JSON)",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostWatch,
}

var costSessionCmd = &cobra.Command{
	Use:           "session",
	Short:         "Print the current session's cost summary",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostSession,
}

var costHistoryCmd = &cobra.Command{
	Use:           "history",
	Short:         "Aggregate cost over recent days from the local store",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostHistory,
}

func init() {
	costWatchCmd.Flags().StringVar(&costCwd, "cwd", "", "workspace directory to scope to (default: current directory)")
	costWatchCmd.Flags().BoolVar(&costAll, "all", false, "watch every Claude Code project, not just the current workspace")
	costWatchCmd.Flags().BoolVar(&costJSON, "json", true, "emit newline-delimited JSON (the only format today)")

	costSessionCmd.Flags().StringVar(&costCwd, "cwd", "", "workspace directory to scope to (default: current directory)")
	costSessionCmd.Flags().BoolVar(&costJSON, "json", true, "emit JSON (the only format today)")

	costHistoryCmd.Flags().IntVar(&costDays, "days", 7, "number of days to include")
	costHistoryCmd.Flags().BoolVar(&costJSON, "json", false, "emit JSON instead of a table")

	costCmd.AddCommand(costWatchCmd)
	costCmd.AddCommand(costSessionCmd)
	costCmd.AddCommand(costHistoryCmd)
}

// resolveCwd returns the explicit --cwd or the process working directory.
func resolveCwd() string {
	if costCwd != "" {
		return costCwd
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

func runCostWatch(cmd *cobra.Command, args []string) error {
	table, err := cost.LoadBundledPriceTable()
	if err != nil {
		return fmt.Errorf("load pricing table: %w", err)
	}

	dirs, err := cost.ResolveDirs(resolveCwd(), costAll)
	if err != nil {
		return fmt.Errorf("resolve transcript directories: %w", err)
	}

	// Persist to the local store best-effort; a store failure must never stop
	// the live stream the extension depends on.
	store, err := cost.OpenStore()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "cost: local store unavailable (%v); continuing without history\n", err)
		store = nil
	}
	if store != nil {
		defer store.Close()
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	scope := "current workspace"
	if costAll {
		scope = "all Claude Code projects"
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "cost: watching %s — local only, Ctrl-C to stop.\n", scope)

	w := cost.NewWatcher(table, store, cmd.OutOrStdout(), true)
	return w.Run(ctx, dirs)
}

func runCostSession(cmd *cobra.Command, args []string) error {
	table, err := cost.LoadBundledPriceTable()
	if err != nil {
		return fmt.Errorf("load pricing table: %w", err)
	}
	dirs, err := cost.ResolveDirs(resolveCwd(), false)
	if err != nil {
		return fmt.Errorf("resolve transcript directories: %w", err)
	}
	w := cost.NewWatcher(table, nil, cmd.OutOrStdout(), false)
	w.SeedNewest(dirs)
	return nil
}

func runCostHistory(cmd *cobra.Command, args []string) error {
	events, err := cost.ReadEvents()
	if err != nil {
		return fmt.Errorf("read cost history: %w", err)
	}

	// Group by calendar day (the YYYY-MM-DD prefix of the RFC3339 timestamp).
	type dayTotal struct {
		day  string
		cost float64
		in   int64
		out  int64
	}
	byDay := map[string]*dayTotal{}
	for _, e := range events {
		day := e.Timestamp
		if len(day) >= 10 {
			day = day[:10]
		}
		dt := byDay[day]
		if dt == nil {
			dt = &dayTotal{day: day}
			byDay[day] = dt
		}
		dt.cost += e.Cost.Total
		dt.in += e.Tokens.Input
		dt.out += e.Tokens.Output
	}

	days := make([]*dayTotal, 0, len(byDay))
	for _, dt := range byDay {
		days = append(days, dt)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].day > days[j].day })
	if costDays > 0 && len(days) > costDays {
		days = days[:costDays]
	}

	out := cmd.OutOrStdout()
	if costJSON {
		fmt.Fprint(out, "[")
		for i, dt := range days {
			if i > 0 {
				fmt.Fprint(out, ",")
			}
			fmt.Fprintf(out, `{"day":%q,"cost_total":%.6f,"input":%d,"output":%d,"currency":%q}`,
				dt.day, dt.cost, dt.in, dt.out, cost.Currency)
		}
		fmt.Fprintln(out, "]")
		return nil
	}

	if len(days) == 0 {
		fmt.Fprintln(out, "No cost history yet. Run `promptconduit cost watch` during a session.")
		return nil
	}
	fmt.Fprintf(out, "%-12s %12s %12s %12s\n", "DAY", "COST (USD)", "INPUT", "OUTPUT")
	var total float64
	for _, dt := range days {
		fmt.Fprintf(out, "%-12s %12.4f %12d %12d\n", dt.day, dt.cost, dt.in, dt.out)
		total += dt.cost
	}
	fmt.Fprintf(out, "%-12s %12.4f\n", "TOTAL", total)
	return nil
}
