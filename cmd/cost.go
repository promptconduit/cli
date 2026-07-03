package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/cost"
	"github.com/spf13/cobra"
)

var (
	costCwd  string
	costJSON bool
	costDays int
)

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Real-time AI token cost for your sessions (100% local)",
	Long: `Compute what your AI coding sessions cost, in real time, entirely on
your machine. The cost feature reads local transcripts/hook payloads, prices
each turn against a bundled rate table, and never sends any of your data to the
PromptConduit platform or anywhere else.

Run with no subcommand to see the current session's cost.

The live per-request feed the editor extension renders comes from the cost
enrichment on ~/.promptconduit/events.jsonl (the old 'cost watch' stream was
retired with the v2 envelope).

Subcommands:
  (none)          Show the current session's cost summary
  session         Same as running 'cost' with no subcommand
  history         Aggregate cost over the last N days
  refresh-pricing Fetch the latest public model-price table (opt-in)

Cost tracking is part of the standard setup: 'promptconduit install cursor'
wires the hooks that capture Cursor's exact token usage, and Claude Code cost
comes from its local transcripts automatically. Models not in the rate table are
reported with exact tokens but flagged unpriced rather than guessed.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostSession, // bare 'promptconduit cost' shows the current session
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

// litellmPricingURL is the public, maintained model-price table the optional
// refresh fetches. It contains no user data; the request is a plain GET.
// NOTE: the file lives at the repo ROOT, not under litellm/ (the latter 404s).
const litellmPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

var costRefreshPricingCmd = &cobra.Command{
	Use:   "refresh-pricing",
	Short: "Fetch the latest public model-price table and cache it locally (opt-in)",
	Long: `Fetch LiteLLM's public model_prices_and_context_window.json and cache it at
~/.config/promptconduit/cost/pricing.json. The curated built-in rates always
take precedence; the cache only adds coverage for models the built-in table
doesn't include.

This is the only command that touches the network, it is never run
automatically, and it sends none of your data — it's a plain GET of a public
file.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostRefreshPricing,
}

func init() {
	// Bare `cost` and `cost session` share behavior: human-readable by default,
	// --json for scripts. They bind the same package vars (only one runs).
	for _, c := range []*cobra.Command{costCmd, costSessionCmd} {
		c.Flags().StringVar(&costCwd, "cwd", "", "workspace directory to scope to (default: current directory)")
		c.Flags().BoolVar(&costJSON, "json", false, "emit JSON instead of a human-readable summary")
	}

	costHistoryCmd.Flags().IntVar(&costDays, "days", 7, "number of days to include")
	costHistoryCmd.Flags().BoolVar(&costJSON, "json", false, "emit JSON instead of a table")

	costCmd.AddCommand(costSessionCmd)
	costCmd.AddCommand(costHistoryCmd)
	costCmd.AddCommand(costRefreshPricingCmd)
}

func runCostRefreshPricing(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Fetching public price table (no data sent): %s\n", litellmPricingURL)

	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, litellmPricingURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch pricing: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch pricing: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return fmt.Errorf("read pricing: %w", err)
	}

	// Validate it parses before caching, so a bad fetch can't poison the table.
	n, err := cost.ValidatePricingData(data)
	if err != nil {
		return fmt.Errorf("fetched pricing did not parse: %w", err)
	}

	path := cost.CachedPricingPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Cached %d model prices to %s\n", n, path)
	_, _ = fmt.Fprintln(out, "Built-in curated rates still take precedence; cached rates fill gaps.")
	return nil
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

func runCostSession(cmd *cobra.Command, args []string) error {
	table, err := cost.LoadPriceTable()
	if err != nil {
		return fmt.Errorf("load pricing table: %w", err)
	}
	cwd := resolveCwd()
	dirs, err := cost.ResolveDirs(cwd, false)
	if err != nil {
		return fmt.Errorf("resolve transcript directories: %w", err)
	}

	// We read the summary directly rather than stream it, so the watcher's emit
	// sink is discarded.
	w := cost.NewWatcher(table, nil, io.Discard, false)
	w.SeedNewest(dirs)
	w.SeedCursorFeeds(cost.CursorFeedPaths(cwd, false))

	summary, ok := w.LatestSummary()
	out := cmd.OutOrStdout()
	if !ok {
		_, _ = fmt.Fprintln(out, "No AI session cost recorded yet for this workspace.")
		_, _ = fmt.Fprintln(out, "Use Claude Code here, or run `promptconduit install cursor` and use Cursor, then try again.")
		return nil
	}
	if costJSON {
		data, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, string(data))
		return nil
	}
	renderSessionHuman(out, summary)
	return nil
}

// renderSessionHuman prints a friendly one-screen cost summary.
func renderSessionHuman(out io.Writer, s cost.SessionSummary) {
	_, _ = fmt.Fprintf(out, "AI session cost — %s  (%s)\n", s.Tool, s.Source)
	_, _ = fmt.Fprintf(out, "  Total: $%.4f %s\n", s.Totals.CostTotal, s.Totals.Currency)
	for _, m := range s.ByModel {
		costStr := fmt.Sprintf("$%.4f", m.CostTotal)
		if !m.ModelPriced {
			costStr = "unpriced"
		}
		_, _ = fmt.Fprintf(out, "    %-22s %-10s  in %d · out %d · cache-read %d · cache-write %d\n",
			m.Model, costStr, m.Tokens.Input, m.Tokens.Output, m.Tokens.CacheRead, m.Tokens.CacheWrite)
	}
	if s.Tools.Total > 0 {
		_, _ = fmt.Fprintf(out, "  Tools: %d call(s)%s\n", s.Tools.Total, formatToolBreakdown(s.Tools.ByName))
	}
	renderSignalsHuman(out, s.Signals)
}

// renderSignalsHuman prints the derived cost-reduction signals as percentages.
// Numbers only — no prompt content. Skipped entirely for an unpriced session,
// where the cost-share signal would be a meaningless 0.
func renderSignalsHuman(out io.Writer, sig cost.Signals) {
	_, _ = fmt.Fprintf(out, "  Signals: cache-hit %.0f%% · input-share %.0f%% · tier %s\n",
		sig.CacheHitRate*100, sig.InputTokenShare*100, sig.Tier)
	if sig.ModelPriced {
		_, _ = fmt.Fprintf(out, "           cache-miss cost share %.0f%%\n", sig.CacheMissCostShare*100)
	}
}

// formatToolBreakdown renders a per-tool count like "  (Read ×3, Bash ×1)",
// sorted by count desc then name. Names only — no inputs are ever shown.
// Returns "" when there's no breakdown.
func formatToolBreakdown(byName map[string]int) string {
	if len(byName) == 0 {
		return ""
	}
	type tc struct {
		name  string
		count int
	}
	tcs := make([]tc, 0, len(byName))
	for n, c := range byName {
		tcs = append(tcs, tc{n, c})
	}
	sort.Slice(tcs, func(i, j int) bool {
		if tcs[i].count != tcs[j].count {
			return tcs[i].count > tcs[j].count
		}
		return tcs[i].name < tcs[j].name
	})
	parts := make([]string, len(tcs))
	for i, t := range tcs {
		parts[i] = fmt.Sprintf("%s ×%d", t.name, t.count)
	}
	return "  (" + strings.Join(parts, ", ") + ")"
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
		_, _ = fmt.Fprint(out, "[")
		for i, dt := range days {
			if i > 0 {
				_, _ = fmt.Fprint(out, ",")
			}
			_, _ = fmt.Fprintf(out, `{"day":%q,"cost_total":%.6f,"input":%d,"output":%d,"currency":%q}`,
				dt.day, dt.cost, dt.in, dt.out, cost.Currency)
		}
		_, _ = fmt.Fprintln(out, "]")
		return nil
	}

	if len(days) == 0 {
		_, _ = fmt.Fprintln(out, "No cost history yet. Run `promptconduit cost watch` during a session.")
		return nil
	}
	_, _ = fmt.Fprintf(out, "%-12s %12s %12s %12s\n", "DAY", "COST (USD)", "INPUT", "OUTPUT")
	var total float64
	for _, dt := range days {
		_, _ = fmt.Fprintf(out, "%-12s %12.4f %12d %12d\n", dt.day, dt.cost, dt.in, dt.out)
		total += dt.cost
	}
	_, _ = fmt.Fprintf(out, "%-12s %12.4f\n", "TOTAL", total)
	return nil
}
