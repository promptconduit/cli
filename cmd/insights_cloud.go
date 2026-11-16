package cmd

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

// Personal cloud views: the same spend and coaching trends as the web
// Overview and Trends pages, from the events your CLI has synced.

var insightsJSON bool

var insightsSpendCmd = &cobra.Command{
	Use:   "spend",
	Short: "Your synced AI spend: total, change, cache savings, models",
	Long: `Your own AI coding spend across every machine that syncs to your account
(the web Overview page). For this machine only, without an account, use
'promptconduit cost history'. Periods: 7d, 30d, 90d.`,
	Args: cobra.NoArgs,
	RunE: runInsightsSpend,
}

var insightsTrendsCmd = &cobra.Command{
	Use:   "trends",
	Short: "Your agent coaching trends and the top suggestions",
	Long: `How you work with your AI agents over time (the web Trends page): plan mode,
subagents, interruptions, tool success, cache hit rate, and concrete coaching
suggestions with a guide for each. Periods: 7d, 30d, 90d.`,
	Args: cobra.NoArgs,
	RunE: runInsightsTrends,
}

func init() {
	insightsCmd.PersistentFlags().BoolVar(&insightsJSON, "json", false, "Output JSON (same as --format json)")
	insightsCmd.AddCommand(insightsSpendCmd, insightsTrendsCmd)
}

func insightsWantJSON() bool {
	return insightsJSON || insightsFormat == "json"
}

// insightsCloudPeriod maps the insights --period (default 7d, also 24h) onto
// the 7d|30d|90d windows the spend and trends endpoints take.
func insightsCloudPeriod(p string) (string, error) {
	switch p {
	case "", "24h", "7d":
		return "7d", nil
	case "30d", "90d":
		return p, nil
	}
	return "", fmt.Errorf("--period must be 7d, 30d or 90d")
}

func insightsClient() (*client.Client, error) {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return nil, fmt.Errorf("not signed in. Run: promptconduit login")
	}
	return client.NewClient(cfg, Version), nil
}

func runInsightsSpend(cmd *cobra.Command, args []string) error {
	period, err := insightsCloudPeriod(insightsPeriod)
	if err != nil {
		return err
	}
	c, err := insightsClient()
	if err != nil {
		return err
	}
	resp := c.GetMyCost(period)
	if !resp.Success {
		return teamAPIError("failed to get your spend", resp)
	}
	if insightsWantJSON() {
		return outputJSON(resp.Data)
	}
	return writeMySpend(os.Stdout, resp.Data)
}

func writeMySpend(out io.Writer, data map[string]interface{}) error {
	totals, _ := data["totals"].(map[string]interface{})
	cache, _ := data["cache"].(map[string]interface{})
	_, _ = fmt.Fprintf(out, "Your AI spend · last %s\n\n", jsonStr(data["period"]))
	line := fmt.Sprintf("  Spend        %s", fmtUSD(jsonNum(totals["usd"])))
	if ch, ok := data["change"].(float64); ok && ch != 0 {
		line += fmt.Sprintf("  (%+.0f%% vs previous period)", ch*100)
	}
	_, _ = fmt.Fprintln(out, line)
	_, _ = fmt.Fprintf(out, "  Activity     %s runs · %s sessions\n", fmtCount(jsonNum(totals["runs"])), fmtCount(jsonNum(totals["sessions"])))
	if s := jsonNum(cache["savingsUsd"]); s > 0 {
		_, _ = fmt.Fprintf(out, "  Cache saved  %s (%.0f%% hit rate)\n", fmtUSD(s), jsonNum(cache["hitRate"])*100)
	}
	models, _ := data["byModel"].([]interface{})
	if len(models) == 0 {
		_, _ = fmt.Fprintln(out, "\n  No synced spend in this period. Events sync when you're signed in and not in local-only mode.")
		return nil
	}
	_, _ = fmt.Fprintln(out, "\nBy model")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range models {
		m, _ := r.(map[string]interface{})
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", orUnknown(jsonStr(m["model"]), "(unknown)"), fmtUSD(jsonNum(m["usd"])), fmtPlural(jsonNum(m["runs"]), "run"))
	}
	return w.Flush()
}

func runInsightsTrends(cmd *cobra.Command, args []string) error {
	p := insightsPeriod
	if !cmd.Flags().Changed("period") {
		p = "30d" // the Trends page default; short windows are noisy
	}
	period, err := insightsCloudPeriod(p)
	if err != nil {
		return err
	}
	c, err := insightsClient()
	if err != nil {
		return err
	}
	resp := c.GetMyTrends(period)
	if !resp.Success {
		return teamAPIError("failed to get your trends", resp)
	}
	if insightsWantJSON() {
		return outputJSON(resp.Data)
	}
	writeMyTrends(os.Stdout, resp.Data)
	return nil
}

func writeMyTrends(out io.Writer, data map[string]interface{}) {
	p, _ := data["period"].(map[string]interface{})
	m, _ := data["metrics"].(map[string]interface{})
	intr, _ := m["interruptions"].(map[string]interface{})
	_, _ = fmt.Fprintf(out, "Your agent trends · last %s days\n\n", jsonStr(p["days"]))
	_, _ = fmt.Fprintf(out, "  Prompts             %s\n", fmtCount(jsonNum(m["prompts"])))
	_, _ = fmt.Fprintf(out, "  Plan mode           %.0f%% of prompts\n", jsonNum(m["plan_mode_adoption_rate"])*100)
	_, _ = fmt.Fprintf(out, "  Interruptions       %.0f%% of prompts\n", jsonNum(intr["rate"])*100)
	_, _ = fmt.Fprintf(out, "  Tool success        %.0f%%\n", jsonNum(m["tool_success_rate"])*100)
	_, _ = fmt.Fprintf(out, "  Slash commands      %.0f%% of prompts\n", jsonNum(m["slash_command_adoption_rate"])*100)

	insights, _ := data["insights"].([]interface{})
	if len(insights) == 0 {
		_, _ = fmt.Fprintln(out, "\n  No coaching suggestions for this period.")
		return
	}
	_, _ = fmt.Fprintln(out, "\nSuggestions")
	for _, r := range insights {
		in, _ := r.(map[string]interface{})
		_, _ = fmt.Fprintf(out, "  • %s: %s\n", jsonStr(in["title"]), jsonStr(in["detail"]))
		if u := jsonStr(in["article_url"]); u != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", u)
		}
	}
}
