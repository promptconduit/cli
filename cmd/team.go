package cmd

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

// The team commands are the agent-first surface for the paid product: the
// same spend, savings, budget and client-tag data as the web team page, as
// text for people and --json for agents.

var (
	teamOrg    string
	teamPeriod string
	teamJSON   bool
	spendBy    string
	exportBy   string
	teamLimit  int
	teamOutput string

	budgetOrgWide bool
	budgetRepo    string
	budgetMember  string
	budgetMonthly float64

	tagClear bool

	createSlug      string
	createNoHistory bool
	inviteRole      string
	joinShare       bool
)

var teamCmd = &cobra.Command{
	Use:   "team",
	Short: "Team AI spend, savings, budgets and client tags",
	Long: `See and manage your team's AI coding spend from the terminal, or let your
agent do it. Every subcommand takes --json for machine-readable output.

The team is picked automatically when you belong to exactly one; otherwise
pass --org (id, slug or name) or set PROMPTCONDUIT_ORG.

Examples:
  promptconduit team                              # teams you belong to
  promptconduit team create "Acme Eng"            # start a team (you're the owner)
  promptconduit team invite dev@example.com       # prints an invite link to share
  promptconduit team join <invite-link>           # accept an invite
  promptconduit team spend --period 7d            # totals + top members, repos, models
  promptconduit team spend --by client --json     # spend per client for an agent
  promptconduit team savings                      # where the team could save
  promptconduit team budgets                      # budgets with month-to-date spend
  promptconduit team budgets set --repo acme/api --monthly 500
  promptconduit team tag acme/api "Acme Corp"     # attribute a repo's spend to a client
  promptconduit team export --by member -o spend.csv`,
	RunE: runTeamList,
}

var teamListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the teams you belong to",
	RunE:  runTeamList,
}

var teamCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a team with you as owner",
	Long: `Create a team with you as owner. Your last 30 days of usage is added to
the team unless you pass --no-history. Free for teams of up to 3.`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamCreate,
}

var teamInviteCmd = &cobra.Command{
	Use:   "invite <email>",
	Short: "Invite someone to the team (owners and admins)",
	Long: `Invite someone to the team and print the invite link to send them. They
accept it with 'promptconduit team join <link>' (or by opening the link).`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamInvite,
}

var teamJoinCmd = &cobra.Command{
	Use:   "join <invite-link|token>",
	Short: "Accept a team invite",
	Long: `Accept a team invite sent to your account's email. Your earlier usage stays
private unless you pass --share-history (adds your last 30 days to the team).`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamJoin,
}

var teamSpendCmd = &cobra.Command{
	Use:   "spend",
	Short: "Team spend totals and breakdowns",
	Long: `Team spend for a period with breakdowns by member, repo, client, model,
branch, pull request and day. Without --by, shows the top members, repos and
models; with --by, shows that one breakdown.`,
	RunE: runTeamSpend,
}

var teamSavingsCmd = &cobra.Command{
	Use:   "savings",
	Short: "Team savings report: cache savings, model mix, runaway sessions",
	RunE:  runTeamSavings,
}

var teamExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export team spend as CSV",
	Long:  `Export team spend as CSV grouped by client, repo, member, pr or day. Writes to stdout unless -o is given.`,
	RunE:  runTeamExport,
}

var teamBudgetsCmd = &cobra.Command{
	Use:   "budgets",
	Short: "List monthly budgets with month-to-date and projected spend",
	RunE:  runTeamBudgets,
}

var teamBudgetsSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Create or change a monthly budget (owners and admins)",
	Long: `Create or change the monthly budget for the whole team, one repo, or one
member. Setting a budget for a scope that already has one changes its amount.
Members are matched by email. Alerts fire at 80% and 100%.

Examples:
  promptconduit team budgets set --org-wide --monthly 2000
  promptconduit team budgets set --repo acme/api --monthly 500
  promptconduit team budgets set --member dev@example.com --monthly 150`,
	Args: cobra.NoArgs,
	RunE: runTeamBudgetsSet,
}

var teamBudgetsRmCmd = &cobra.Command{
	Use:   "rm <budget-id>",
	Short: "Remove a budget (owners and admins)",
	Args:  cobra.ExactArgs(1),
	RunE:  runTeamBudgetsRm,
}

var teamTagCmd = &cobra.Command{
	Use:   "tag <repo> [client]",
	Short: "Tag a repo with a client so its spend rolls up per client (owners and admins)",
	Long: `Tag a repo with a client so its spend shows up per client in 'team spend
--by client' and the CSV export. Use --clear to remove the tag.

Examples:
  promptconduit team tag acme/api "Acme Corp"
  promptconduit team tag acme/api --clear`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runTeamTag,
}

func init() {
	teamCmd.PersistentFlags().StringVar(&teamOrg, "org", "", "Team id, slug or name (default: your only team, or $PROMPTCONDUIT_ORG)")
	teamCmd.PersistentFlags().BoolVar(&teamJSON, "json", false, "Output JSON")

	for _, c := range []*cobra.Command{teamSpendCmd, teamSavingsCmd, teamExportCmd} {
		c.Flags().StringVarP(&teamPeriod, "period", "p", "30d", "Time period (7d, 30d, 90d)")
	}
	teamSpendCmd.Flags().StringVar(&spendBy, "by", "", "Single breakdown: member, repo, client, model, branch, pr, day")
	teamSpendCmd.Flags().IntVarP(&teamLimit, "limit", "l", 5, "Rows per breakdown (0 = all)")
	teamExportCmd.Flags().StringVar(&exportBy, "by", "repo", "Group by: client, repo, member, pr, day")
	teamExportCmd.Flags().StringVarP(&teamOutput, "output", "o", "", "Write CSV to this file instead of stdout")

	teamBudgetsSetCmd.Flags().BoolVar(&budgetOrgWide, "org-wide", false, "Budget for the whole team")
	teamBudgetsSetCmd.Flags().StringVar(&budgetRepo, "repo", "", "Budget for one repo")
	teamBudgetsSetCmd.Flags().StringVar(&budgetMember, "member", "", "Budget for one member (email)")
	teamBudgetsSetCmd.Flags().Float64Var(&budgetMonthly, "monthly", 0, "Monthly budget in USD")
	_ = teamBudgetsSetCmd.MarkFlagRequired("monthly")

	teamTagCmd.Flags().BoolVar(&tagClear, "clear", false, "Remove the repo's client tag")

	teamCreateCmd.Flags().StringVar(&createSlug, "slug", "", "URL slug (default: derived from the name)")
	teamCreateCmd.Flags().BoolVar(&createNoHistory, "no-history", false, "Don't add your last 30 days of usage to the team")
	teamInviteCmd.Flags().StringVar(&inviteRole, "role", "member", "Role: member, admin, viewer")
	teamJoinCmd.Flags().BoolVar(&joinShare, "share-history", false, "Add your last 30 days of usage to the team")

	teamBudgetsCmd.AddCommand(teamBudgetsSetCmd, teamBudgetsRmCmd)
	teamCmd.AddCommand(teamListCmd, teamCreateCmd, teamInviteCmd, teamJoinCmd,
		teamSpendCmd, teamSavingsCmd, teamExportCmd, teamBudgetsCmd, teamTagCmd)

	// A failed API call is not a usage mistake; print the error alone so
	// agents (and people) see the one line that matters.
	silenceUsage(teamCmd)
}

func silenceUsage(c *cobra.Command) {
	c.SilenceUsage = true
	for _, sub := range c.Commands() {
		silenceUsage(sub)
	}
}

// ============================================================================
// Org resolution and errors
// ============================================================================

type teamOrgInfo struct {
	ID   string
	Name string
	Slug string
	Role string
	Plan string
}

func newTeamClient() (*client.Client, error) {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return nil, fmt.Errorf("not signed in. Run: promptconduit login")
	}
	return client.NewClient(cfg, Version), nil
}

// teamAPIError turns a failed reply into one line an agent or person can act
// on: the problem detail from the API plus a next step where there is one.
func teamAPIError(action string, resp *client.APIResponse) error {
	msg := resp.Error
	if resp.Data != nil {
		if d, ok := resp.Data["detail"].(string); ok && d != "" {
			msg = d
		} else if t, ok := resp.Data["title"].(string); ok && t != "" {
			msg = t
		}
	}
	if resp.StatusCode == 401 {
		msg += ". Run: promptconduit login"
	}
	return fmt.Errorf("%s: %s", action, msg)
}

func listTeamOrgs(c *client.Client) ([]teamOrgInfo, error) {
	resp := c.ListOrganizations()
	if !resp.Success {
		return nil, teamAPIError("failed to list teams", resp)
	}
	raw, _ := resp.Data["organizations"].([]interface{})
	orgs := make([]teamOrgInfo, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		orgs = append(orgs, teamOrgInfo{
			ID:   str(m["id"]),
			Name: str(m["name"]),
			Slug: str(m["slug"]),
			Role: str(m["role"]),
			Plan: str(m["plan"]),
		})
	}
	return orgs, nil
}

// pickTeamOrg chooses the org a command acts on: the one matching want (id,
// slug, or case-insensitive name), or the only org when want is empty.
func pickTeamOrg(orgs []teamOrgInfo, want string) (teamOrgInfo, error) {
	if want != "" {
		for _, o := range orgs {
			if o.ID == want || o.Slug == want || strings.EqualFold(o.Name, want) {
				return o, nil
			}
		}
		return teamOrgInfo{}, fmt.Errorf("no team matches %q. Your teams: %s", want, describeOrgs(orgs))
	}
	switch len(orgs) {
	case 0:
		return teamOrgInfo{}, fmt.Errorf("you're not a member of any team yet. Create one: promptconduit team create <name>")
	case 1:
		return orgs[0], nil
	default:
		return teamOrgInfo{}, fmt.Errorf("you belong to %d teams; pass --org or set PROMPTCONDUIT_ORG. Your teams: %s", len(orgs), describeOrgs(orgs))
	}
}

func describeOrgs(orgs []teamOrgInfo) string {
	if len(orgs) == 0 {
		return "(none)"
	}
	parts := make([]string, len(orgs))
	for i, o := range orgs {
		parts[i] = fmt.Sprintf("%s (%s)", o.Name, o.Slug)
	}
	return strings.Join(parts, ", ")
}

func resolveTeamOrg(c *client.Client) (teamOrgInfo, error) {
	orgs, err := listTeamOrgs(c)
	if err != nil {
		return teamOrgInfo{}, err
	}
	want := teamOrg
	if want == "" {
		want = os.Getenv("PROMPTCONDUIT_ORG")
	}
	return pickTeamOrg(orgs, want)
}

func (o teamOrgInfo) jsonRef() map[string]interface{} {
	return map[string]interface{}{"id": o.ID, "name": o.Name, "slug": o.Slug}
}

// withOrg adds which team the data is for, so --json output is self-describing.
func withOrg(data map[string]interface{}, org teamOrgInfo) map[string]interface{} {
	if data == nil {
		data = map[string]interface{}{}
	}
	data["organization"] = org.jsonRef()
	return data
}

// ============================================================================
// list
// ============================================================================

func runTeamList(cmd *cobra.Command, args []string) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	orgs, err := listTeamOrgs(c)
	if err != nil {
		return err
	}
	if teamJSON {
		out := make([]interface{}, len(orgs))
		for i, o := range orgs {
			out[i] = map[string]interface{}{"id": o.ID, "name": o.Name, "slug": o.Slug, "role": o.Role, "plan": o.Plan}
		}
		return outputJSON(map[string]interface{}{"organizations": out})
	}
	if len(orgs) == 0 {
		fmt.Println("You're not a member of any team yet.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSLUG\tROLE\tPLAN\tID")
	for _, o := range orgs {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", o.Name, o.Slug, o.Role, o.Plan, o.ID)
	}
	return w.Flush()
}

// ============================================================================
// create / invite / join
// ============================================================================

func runTeamCreate(cmd *cobra.Command, args []string) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	resp := c.CreateOrganization(strings.TrimSpace(args[0]), createSlug, !createNoHistory)
	if !resp.Success {
		return teamAPIError("failed to create team", resp)
	}
	if teamJSON {
		return outputJSON(resp.Data)
	}
	org, _ := resp.Data["organization"].(map[string]interface{})
	fmt.Printf("Created %s (%s). You're the owner.\n", str(org["name"]), str(org["slug"]))
	if n := num(resp.Data["attachedEvents"]); n > 0 {
		fmt.Printf("Added %s of your recent events to the team.\n", count(n))
	}
	fmt.Println("Invite people: promptconduit team invite <email>")
	return nil
}

func runTeamInvite(cmd *cobra.Command, args []string) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	resp := c.InviteMember(org.ID, args[0], inviteRole)
	if !resp.Success {
		return teamAPIError("failed to invite", resp)
	}
	if teamJSON {
		return outputJSON(withOrg(resp.Data, org))
	}
	inv, _ := resp.Data["invite"].(map[string]interface{})
	fmt.Printf("Invited %s to %s as %s.\n", str(inv["email"]), org.Name, str(inv["role"]))
	fmt.Printf("Send them this link: %s\n", str(inv["inviteUrl"]))
	fmt.Println("They can also accept from their terminal: promptconduit team join <link>")
	return nil
}

// inviteToken accepts a full invite link (…/invite/<token>) or a bare token.
func inviteToken(arg string) string {
	arg = strings.TrimSpace(arg)
	if i := strings.LastIndex(arg, "/invite/"); i >= 0 {
		arg = arg[i+len("/invite/"):]
	}
	if i := strings.IndexAny(arg, "?#/"); i >= 0 {
		arg = arg[:i]
	}
	return arg
}

func runTeamJoin(cmd *cobra.Command, args []string) error {
	token := inviteToken(args[0])
	if token == "" {
		return fmt.Errorf("pass an invite link or token")
	}
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	resp := c.AcceptInvite(token)
	if !resp.Success {
		return teamAPIError("failed to join team", resp)
	}
	org, _ := resp.Data["organization"].(map[string]interface{})
	if joinShare {
		share := c.AttachHistory(str(org["id"]))
		if !share.Success {
			return teamAPIError("joined, but failed to share history", share)
		}
		resp.Data["attachedEvents"] = share.Data["attachedEvents"]
	}
	if teamJSON {
		return outputJSON(resp.Data)
	}
	fmt.Printf("Joined %s.\n", str(org["name"]))
	if joinShare {
		fmt.Printf("Shared %s of your recent events with the team.\n", count(num(resp.Data["attachedEvents"])))
	}
	return nil
}

// ============================================================================
// spend
// ============================================================================

// spendBreakdowns maps --by to the cost response's list key.
var spendBreakdowns = map[string]string{
	"member": "byMember",
	"repo":   "byRepo",
	"client": "byClient",
	"model":  "byModel",
	"branch": "byBranch",
	"pr":     "byPR",
	"day":    "byDay",
}

func runTeamSpend(cmd *cobra.Command, args []string) error {
	if spendBy != "" {
		if _, ok := spendBreakdowns[spendBy]; !ok {
			return fmt.Errorf("--by must be one of member, repo, client, model, branch, pr, day")
		}
	}
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	resp := c.GetTeamCost(org.ID, teamPeriod)
	if !resp.Success {
		return teamAPIError("failed to get team spend", resp)
	}
	if teamJSON {
		return outputJSON(withOrg(resp.Data, org))
	}
	return writeTeamSpend(os.Stdout, org, resp.Data, spendBy, teamLimit)
}

func writeTeamSpend(out io.Writer, org teamOrgInfo, data map[string]interface{}, by string, limit int) error {
	totals, _ := data["totals"].(map[string]interface{})
	cache, _ := data["cache"].(map[string]interface{})

	_, _ = fmt.Fprintf(out, "%s · last %s\n\n", org.Name, str(data["period"]))
	_, _ = fmt.Fprintf(out, "  Spend        %s\n", usd(num(totals["usd"])))
	_, _ = fmt.Fprintf(out, "  Activity     %s runs · %s sessions · %s members\n",
		count(num(totals["runs"])), count(num(totals["sessions"])), count(num(totals["members"])))
	if s := num(cache["savingsUsd"]); s > 0 {
		_, _ = fmt.Fprintf(out, "  Cache saved  %s (%.0f%% hit rate)\n", usd(s), num(cache["hitRate"])*100)
	}

	sections := []string{"member", "repo", "model"}
	if by != "" {
		sections = []string{by}
	}
	for _, s := range sections {
		rows, _ := data[spendBreakdowns[s]].([]interface{})
		_, _ = fmt.Fprintf(out, "\nBy %s\n", s)
		if len(rows) == 0 {
			_, _ = fmt.Fprintln(out, "  (no spend)")
			continue
		}
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for i, r := range rows {
			if limit > 0 && i >= limit {
				_, _ = fmt.Fprintf(w, "  … %d more (use --limit 0)\n", len(rows)-limit)
				break
			}
			m, _ := r.(map[string]interface{})
			label, detail := spendRowLabel(s, m)
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\n", label, usd(num(m["usd"])), detail)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// spendRowLabel names a breakdown row and summarizes its counts.
func spendRowLabel(by string, m map[string]interface{}) (string, string) {
	sessions := plural(num(m["sessions"]), "session")
	switch by {
	case "member":
		name := str(m["name"])
		if name == "" {
			name = str(m["email"])
		} else if e := str(m["email"]); e != "" {
			name += " <" + e + ">"
		}
		return name, sessions
	case "repo":
		label := orUnknown(str(m["repoName"]), "(no repo)")
		if cl := str(m["client"]); cl != "" {
			label += " [" + cl + "]"
		}
		return label, sessions
	case "client":
		return orUnknown(str(m["client"]), "(untagged)"), plural(num(m["repos"]), "repo") + " · " + sessions
	case "model":
		return orUnknown(str(m["model"]), "(unknown)"), plural(num(m["runs"]), "run")
	case "branch":
		return str(m["repoName"]) + "@" + str(m["branch"]), sessions
	case "pr":
		return fmt.Sprintf("%s#%s", str(m["repoName"]), str(m["prNumber"])), sessions
	default: // day
		return str(m["day"]), ""
	}
}

// ============================================================================
// savings
// ============================================================================

func runTeamSavings(cmd *cobra.Command, args []string) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	resp := c.GetTeamSavings(org.ID, teamPeriod)
	if !resp.Success {
		return teamAPIError("failed to get team savings", resp)
	}
	if teamJSON {
		return outputJSON(withOrg(resp.Data, org))
	}
	writeTeamSavings(os.Stdout, org, resp.Data)
	return nil
}

func writeTeamSavings(out io.Writer, org teamOrgInfo, data map[string]interface{}) {
	spend, _ := data["spend"].(map[string]interface{})
	cache, _ := data["cache"].(map[string]interface{})
	mix, _ := data["modelMix"].(map[string]interface{})
	runaway, _ := data["runaway"].(map[string]interface{})
	opp, _ := data["opportunity"].(map[string]interface{})

	_, _ = fmt.Fprintf(out, "%s savings · last %s\n\n", org.Name, str(data["period"]))
	_, _ = fmt.Fprintf(out, "  Spend            %s  (≈ %s/month)\n", usd(num(spend["usd"])), usd(num(spend["monthlyUsd"])))
	_, _ = fmt.Fprintf(out, "  Cache saved      %s  (≈ %s/month)\n", usd(num(cache["savingsUsd"])), usd(num(cache["monthlyUsd"])))
	_, _ = fmt.Fprintf(out, "  Model mix        %s/month if %s light premium-model turns ran on Sonnet\n",
		usd(num(mix["monthlyUsd"])), count(num(mix["turns"])))
	_, _ = fmt.Fprintf(out, "  Runaway sessions %s costing %s (≥ %s each, typical %s)\n",
		count(num(runaway["count"])), usd(num(runaway["usd"])), usd(num(runaway["thresholdUsd"])), usd(num(runaway["typicalSessionUsd"])))

	if top, _ := runaway["top"].([]interface{}); len(top) > 0 {
		_, _ = fmt.Fprintln(out)
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, r := range top {
			m, _ := r.(map[string]interface{})
			who := str(m["name"])
			if who == "" {
				who = str(m["email"])
			}
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", usd(num(m["usd"])), who, orUnknown(str(m["repoName"]), "(no repo)"), str(m["sessionId"]))
		}
		_ = w.Flush()
	}
	_, _ = fmt.Fprintf(out, "\n  Opportunity      %s/month\n", usd(num(opp["monthlyUsd"])))
}

// ============================================================================
// export
// ============================================================================

func runTeamExport(cmd *cobra.Command, args []string) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	csv, err := c.ExportTeamCost(org.ID, teamPeriod, exportBy)
	if err != nil {
		return fmt.Errorf("failed to export team spend: %w", err)
	}
	if teamOutput == "" {
		_, err = fmt.Fprint(os.Stdout, csv)
		return err
	}
	if err := os.WriteFile(teamOutput, []byte(csv), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", teamOutput, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "Wrote %s\n", teamOutput)
	return nil
}

// ============================================================================
// budgets
// ============================================================================

func runTeamBudgets(cmd *cobra.Command, args []string) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	resp := c.ListBudgets(org.ID)
	if !resp.Success {
		return teamAPIError("failed to list budgets", resp)
	}
	return outputBudgets(org, resp.Data)
}

func outputBudgets(org teamOrgInfo, data map[string]interface{}) error {
	if teamJSON {
		return outputJSON(withOrg(data, org))
	}
	return writeBudgets(os.Stdout, org, data)
}

func writeBudgets(out io.Writer, org teamOrgInfo, data map[string]interface{}) error {
	budgets, _ := data["budgets"].([]interface{})
	slack := "off"
	if b, _ := data["slackConnected"].(bool); b {
		slack = "on"
	}
	_, _ = fmt.Fprintf(out, "%s monthly budgets (Slack alerts: %s)\n\n", org.Name, slack)
	if len(budgets) == 0 {
		_, _ = fmt.Fprintln(out, "  No budgets yet. Add one: promptconduit team budgets set --org-wide --monthly 1000")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  SCOPE\tTARGET\tSPENT\tBUDGET\tUSED\tPROJECTED\tID")
	for _, b := range budgets {
		m, _ := b.(map[string]interface{})
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%.0f%%\t%s\t%s\n",
			str(m["scope"]), orUnknown(str(m["label"]), str(m["scopeValue"])),
			usd(num(m["spendUsd"])), usd(num(m["monthlyUsd"])), num(m["pct"]),
			usd(num(m["projectedUsd"])), str(m["id"]))
	}
	return w.Flush()
}

func runTeamBudgetsSet(cmd *cobra.Command, args []string) error {
	scope, value, err := budgetScope(budgetOrgWide, budgetRepo, budgetMember)
	if err != nil {
		return err
	}
	if budgetMonthly <= 0 {
		return fmt.Errorf("--monthly must be greater than 0")
	}
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	if scope == "member" {
		if value, err = memberUserID(c, org, value); err != nil {
			return err
		}
	}
	resp := c.SetBudget(org.ID, scope, value, budgetMonthly)
	if !resp.Success {
		return teamAPIError("failed to set budget", resp)
	}
	return outputBudgets(org, resp.Data)
}

// budgetScope validates that exactly one of --org-wide, --repo, --member is set.
func budgetScope(orgWide bool, repo, member string) (string, string, error) {
	n := 0
	scope, value := "", ""
	if orgWide {
		n++
		scope = "org"
	}
	if repo != "" {
		n++
		scope, value = "repo", repo
	}
	if member != "" {
		n++
		scope, value = "member", member
	}
	if n != 1 {
		return "", "", fmt.Errorf("pass exactly one of --org-wide, --repo <name>, --member <email>")
	}
	return scope, value, nil
}

// memberUserID finds a member's user id by email (or accepts a user id as-is).
func memberUserID(c *client.Client, org teamOrgInfo, who string) (string, error) {
	resp := c.ListOrgMembers(org.ID)
	if !resp.Success {
		return "", teamAPIError("failed to list members", resp)
	}
	members, _ := resp.Data["members"].([]interface{})
	for _, r := range members {
		m, _ := r.(map[string]interface{})
		u, _ := m["user"].(map[string]interface{})
		if strings.EqualFold(str(u["email"]), who) || str(u["id"]) == who {
			return str(u["id"]), nil
		}
	}
	return "", fmt.Errorf("%s is not a member of %s", who, org.Name)
}

func runTeamBudgetsRm(cmd *cobra.Command, args []string) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	resp := c.DeleteBudget(org.ID, args[0])
	if !resp.Success {
		return teamAPIError("failed to remove budget", resp)
	}
	return outputBudgets(org, resp.Data)
}

// ============================================================================
// tag
// ============================================================================

func runTeamTag(cmd *cobra.Command, args []string) error {
	repo := args[0]
	clientName := ""
	if len(args) == 2 {
		clientName = strings.TrimSpace(args[1])
	}
	if tagClear && clientName != "" {
		return fmt.Errorf("pass a client or --clear, not both")
	}
	if !tagClear && clientName == "" {
		return fmt.Errorf("pass a client name, or --clear to remove the tag")
	}
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	resp := c.SetRepoClient(org.ID, repo, clientName)
	if !resp.Success {
		return teamAPIError("failed to tag repo", resp)
	}
	if teamJSON {
		return outputJSON(withOrg(resp.Data, org))
	}
	if clientName == "" {
		fmt.Printf("Cleared the client tag on %s\n", repo)
	} else {
		fmt.Printf("Tagged %s → %s\n", repo, clientName)
	}
	return nil
}

// ============================================================================
// Formatting helpers
// ============================================================================

func str(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == math.Trunc(t) {
			return fmt.Sprintf("%.0f", t)
		}
		return fmt.Sprintf("%g", t)
	default:
		return fmt.Sprint(t)
	}
}

func num(v interface{}) float64 {
	f, _ := v.(float64)
	return f
}

func orUnknown(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// usd formats dollars with thousands separators: 12345.6 → $12,345.60.
func usd(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	cents := int64(math.Round(v * 100))
	return fmt.Sprintf("%s$%s.%02d", sign, groupThousands(cents/100), cents%100)
}

// count formats a whole number with thousands separators.
func count(v float64) string {
	return groupThousands(int64(math.Round(v)))
}

func plural(v float64, noun string) string {
	if math.Round(v) == 1 {
		return "1 " + noun
	}
	return count(v) + " " + noun + "s"
}

func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
