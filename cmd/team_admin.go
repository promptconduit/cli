package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

// Team administration: billing, members and Slack budget alerts. Same API
// routes as the web team page, so nobody has to open the dashboard to run a
// team. Payment details are only ever entered on Stripe's hosted pages: the
// CLI prints (or opens) the URL.

var (
	billingTier   string
	billingOpen   bool
	memberYes     bool
	slackOff      bool
	slackYes      bool
	paidTiers     = []string{"team", "growth"}
	assignedRoles = []string{"owner", "admin", "member", "viewer"}
)

var teamBillingCmd = &cobra.Command{
	Use:   "billing",
	Short: "Team plan, seats and subscription status",
	Long: `Show the team's plan, seats and subscription status. Owners and admins can
start a subscription or open the billing portal; both print a secure Stripe
page to open in a browser (the CLI never handles card details).

Examples:
  promptconduit team billing
  promptconduit team billing checkout --tier team --open
  promptconduit team billing portal --open`,
	Args: cobra.NoArgs,
	RunE: runTeamBilling,
}

var teamBillingCheckoutCmd = &cobra.Command{
	Use:   "checkout",
	Short: "Start a Team or Growth subscription (owners and admins)",
	Long: `Create a Stripe Checkout page for a paid plan and print its URL. Nothing is
charged until you finish checkout in the browser. Teams that already have a
subscription change plans in the billing portal instead.`,
	Args: cobra.NoArgs,
	RunE: runTeamBillingCheckout,
}

var teamBillingPortalCmd = &cobra.Command{
	Use:   "portal",
	Short: "Open the billing portal: change plan, update card, cancel (owners and admins)",
	Args:  cobra.NoArgs,
	RunE:  runTeamBillingPortal,
}

var teamMembersCmd = &cobra.Command{
	Use:   "members",
	Short: "List team members and their roles",
	Long: `List team members with their roles. Owners and admins can change roles and
remove members. Members are matched by email (or user/member id).

Examples:
  promptconduit team members
  promptconduit team members role dev@example.com admin
  promptconduit team members rm dev@example.com --yes`,
	Args: cobra.NoArgs,
	RunE: runTeamMembers,
}

var teamMembersRoleCmd = &cobra.Command{
	Use:   "role <email> <owner|admin|member|viewer>",
	Short: "Change a member's role (owners and admins; only owners grant admin or owner)",
	Args:  cobra.ExactArgs(2),
	RunE:  runTeamMembersRole,
}

var teamMembersRmCmd = &cobra.Command{
	Use:   "rm <email>",
	Short: "Remove a member from the team (owners and admins; needs --yes)",
	Long: `Remove a member from the team. They lose access to the team's data at once;
their own usage history stays theirs. Pass --yes to confirm.`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamMembersRm,
}

var teamAlertsCmd = &cobra.Command{
	Use:   "alerts",
	Short: "Budget alert channels (email always; Slack optional)",
	Long: `Show where budget alerts go. Alerts fire at 80% and 100% of a monthly
budget, by email to owners and admins and, when connected, to a Slack channel.

Examples:
  promptconduit team alerts
  promptconduit team alerts slack https://hooks.slack.com/services/T000/B000/XXXX
  promptconduit team alerts slack --disconnect --yes`,
	Args: cobra.NoArgs,
	RunE: runTeamAlerts,
}

var teamAlertsSlackCmd = &cobra.Command{
	Use:   "slack [incoming-webhook-url]",
	Short: "Send budget alerts to a Slack channel, or stop with --disconnect (owners and admins)",
	Long: `Connect a Slack incoming webhook (https://hooks.slack.com/services/…) for
budget alerts. A test message is posted first, so a bad URL fails now rather
than on the day a budget runs over. Pass - to read the URL from stdin and keep
it out of your shell history. --disconnect --yes stops Slack alerts.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTeamAlertsSlack,
}

func init() {
	teamBillingCheckoutCmd.Flags().StringVar(&billingTier, "tier", "", "Plan: team or growth")
	_ = teamBillingCheckoutCmd.MarkFlagRequired("tier")
	for _, c := range []*cobra.Command{teamBillingCheckoutCmd, teamBillingPortalCmd} {
		c.Flags().BoolVar(&billingOpen, "open", false, "Also open the page in your browser")
	}
	teamMembersRmCmd.Flags().BoolVar(&memberYes, "yes", false, "Confirm removing the member")
	teamAlertsSlackCmd.Flags().BoolVar(&slackOff, "disconnect", false, "Stop sending budget alerts to Slack")
	teamAlertsSlackCmd.Flags().BoolVar(&slackYes, "yes", false, "Confirm --disconnect")

	teamBillingCmd.AddCommand(teamBillingCheckoutCmd, teamBillingPortalCmd)
	teamMembersCmd.AddCommand(teamMembersRoleCmd, teamMembersRmCmd)
	teamAlertsCmd.AddCommand(teamAlertsSlackCmd)
	teamCmd.AddCommand(teamBillingCmd, teamMembersCmd, teamAlertsCmd)
	for _, c := range []*cobra.Command{teamBillingCmd, teamMembersCmd, teamAlertsCmd} {
		silenceUsage(c)
	}
}

// teamRun resolves the client and org for a team subcommand.
func teamRun(fn func(c *client.Client, org teamOrgInfo) error) error {
	c, err := newTeamClient()
	if err != nil {
		return err
	}
	org, err := resolveTeamOrg(c)
	if err != nil {
		return err
	}
	return fn(c, org)
}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// ============================================================================
// billing
// ============================================================================

func runTeamBilling(cmd *cobra.Command, args []string) error {
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		resp := c.GetTeamBilling(org.ID)
		if !resp.Success {
			return teamAPIError("failed to get billing status", resp)
		}
		if teamJSON {
			return outputJSON(withOrg(resp.Data, org))
		}
		writeTeamBilling(os.Stdout, org, resp.Data)
		return nil
	})
}

func writeTeamBilling(out io.Writer, org teamOrgInfo, data map[string]interface{}) {
	b, _ := data["billing"].(map[string]interface{})
	_, _ = fmt.Fprintf(out, "%s billing\n\n", org.Name)
	_, _ = fmt.Fprintf(out, "  Plan         %s\n", orUnknown(jsonStr(b["plan"]), "free"))
	if s := jsonStr(b["subscriptionStatus"]); s != "" {
		_, _ = fmt.Fprintf(out, "  Status       %s\n", s)
	}
	if t := jsonStr(b["trialEndsAt"]); t != "" && jsonStr(b["subscriptionStatus"]) == "trialing" {
		_, _ = fmt.Fprintf(out, "  Trial ends   %s\n", t)
	}
	if cancel, _ := b["cancelAtPeriodEnd"].(bool); cancel {
		_, _ = fmt.Fprintln(out, "  Renewal      cancels at the end of the period")
	}
	_, _ = fmt.Fprintf(out, "  Seats        %s of %s used\n", fmtCount(jsonNum(b["activeMembers"])), fmtCount(jsonNum(b["seats"])))
	if active, _ := b["active"].(bool); !active {
		_, _ = fmt.Fprintln(out, "\n  Team features are off: the team has more members than its plan covers.")
	}
	if org.Role == "owner" || org.Role == "admin" {
		if jsonStr(b["plan"]) == "free" || jsonStr(b["plan"]) == "" {
			_, _ = fmt.Fprintln(out, "\nUpgrade: promptconduit team billing checkout --tier team --open")
		} else {
			_, _ = fmt.Fprintln(out, "\nChange plan or card: promptconduit team billing portal --open")
		}
	}
}

func runTeamBillingCheckout(cmd *cobra.Command, args []string) error {
	if !oneOf(billingTier, paidTiers) {
		return fmt.Errorf("--tier must be team or growth")
	}
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		return outputBillingURL(org, c.CreateTeamCheckout(org.ID, billingTier), "failed to start checkout",
			"Finish checkout in your browser; nothing is charged until you do:")
	})
}

func runTeamBillingPortal(cmd *cobra.Command, args []string) error {
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		return outputBillingURL(org, c.CreateTeamBillingPortal(org.ID), "failed to open billing portal",
			"Manage the plan, card and invoices here:")
	})
}

func outputBillingURL(org teamOrgInfo, resp *client.APIResponse, action, lead string) error {
	if !resp.Success {
		return teamAPIError(action, resp)
	}
	u := jsonStr(resp.Data["url"])
	if teamJSON {
		return outputJSON(withOrg(resp.Data, org))
	}
	fmt.Println(lead)
	fmt.Println(u)
	if billingOpen && u != "" {
		if err := openBrowserURL(u); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Couldn't open a browser (%v); open the link above.\n", err)
		}
	}
	return nil
}

// ============================================================================
// members
// ============================================================================

// teamMember is one row of the members list.
type teamMember struct {
	ID     string // membership id (what PATCH/DELETE take)
	UserID string
	Email  string
	Name   string
	Role   string
}

func listMembers(c *client.Client, org teamOrgInfo) ([]teamMember, map[string]interface{}, error) {
	resp := c.ListOrgMembers(org.ID)
	if !resp.Success {
		return nil, nil, teamAPIError("failed to list members", resp)
	}
	raw, _ := resp.Data["members"].([]interface{})
	out := make([]teamMember, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]interface{})
		u, _ := m["user"].(map[string]interface{})
		out = append(out, teamMember{
			ID:     jsonStr(m["id"]),
			UserID: jsonStr(u["id"]),
			Email:  jsonStr(u["email"]),
			Name:   jsonStr(u["name"]),
			Role:   jsonStr(m["role"]),
		})
	}
	return out, resp.Data, nil
}

// findMember matches an email (case-insensitive), user id or membership id.
func findMember(members []teamMember, org teamOrgInfo, who string) (teamMember, error) {
	who = strings.TrimSpace(who)
	for _, m := range members {
		if strings.EqualFold(m.Email, who) || m.UserID == who || m.ID == who {
			return m, nil
		}
	}
	return teamMember{}, fmt.Errorf("%s is not a member of %s. See: promptconduit team members", who, org.Name)
}

func runTeamMembers(cmd *cobra.Command, args []string) error {
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		members, data, err := listMembers(c, org)
		if err != nil {
			return err
		}
		if teamJSON {
			return outputJSON(withOrg(data, org))
		}
		return writeMembers(os.Stdout, org, members)
	})
}

func writeMembers(out io.Writer, org teamOrgInfo, members []teamMember) error {
	_, _ = fmt.Fprintf(out, "%s members (%d)\n\n", org.Name, len(members))
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  EMAIL\tNAME\tROLE\tMEMBER ID")
	for _, m := range members {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", m.Email, orUnknown(m.Name, "-"), m.Role, m.ID)
	}
	return w.Flush()
}

func runTeamMembersRole(cmd *cobra.Command, args []string) error {
	role := strings.ToLower(strings.TrimSpace(args[1]))
	if !oneOf(role, assignedRoles) {
		return fmt.Errorf("role must be one of owner, admin, member, viewer")
	}
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		res, err := setMemberRole(c, org, args[0], role)
		if err != nil {
			return err
		}
		if teamJSON {
			return outputJSON(res)
		}
		fmt.Printf("%s is now %s in %s.\n", jsonStr(res["email"]), role, org.Name)
		return nil
	})
}

// setMemberRole is shared by the CLI and the MCP tool.
func setMemberRole(c *client.Client, org teamOrgInfo, who, role string) (map[string]interface{}, error) {
	members, _, err := listMembers(c, org)
	if err != nil {
		return nil, err
	}
	m, err := findMember(members, org, who)
	if err != nil {
		return nil, err
	}
	resp := c.UpdateMemberRole(org.ID, m.ID, role)
	if !resp.Success {
		return nil, teamAPIError("failed to change role", resp)
	}
	return withOrg(map[string]interface{}{
		"memberId": m.ID, "email": m.Email, "previousRole": m.Role, "role": role,
	}, org), nil
}

func runTeamMembersRm(cmd *cobra.Command, args []string) error {
	if !memberYes {
		return fmt.Errorf("removing %s takes away their access to the team. Re-run with --yes to confirm", args[0])
	}
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		res, err := removeMember(c, org, args[0])
		if err != nil {
			return err
		}
		if teamJSON {
			return outputJSON(res)
		}
		fmt.Printf("Removed %s from %s.\n", jsonStr(res["email"]), org.Name)
		return nil
	})
}

// removeMember is shared by the CLI and the MCP tool.
func removeMember(c *client.Client, org teamOrgInfo, who string) (map[string]interface{}, error) {
	members, _, err := listMembers(c, org)
	if err != nil {
		return nil, err
	}
	m, err := findMember(members, org, who)
	if err != nil {
		return nil, err
	}
	resp := c.RemoveMember(org.ID, m.ID)
	if !resp.Success {
		return nil, teamAPIError("failed to remove member", resp)
	}
	return withOrg(map[string]interface{}{"removed": true, "memberId": m.ID, "email": m.Email, "role": m.Role}, org), nil
}

// ============================================================================
// alerts
// ============================================================================

func runTeamAlerts(cmd *cobra.Command, args []string) error {
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		resp := c.ListBudgets(org.ID)
		if !resp.Success {
			return teamAPIError("failed to get alert settings", resp)
		}
		return outputAlerts(org, resp.Data, "")
	})
}

// alertsSummary is the alert-settings view of a budgets reply.
func alertsSummary(org teamOrgInfo, data map[string]interface{}) map[string]interface{} {
	slack, _ := data["slackConnected"].(bool)
	return withOrg(map[string]interface{}{
		"email":          true,
		"slackConnected": slack,
		"thresholds":     data["thresholds"],
		"canManage":      data["canManage"],
	}, org)
}

func outputAlerts(org teamOrgInfo, data map[string]interface{}, note string) error {
	sum := alertsSummary(org, data)
	if teamJSON {
		return outputJSON(sum)
	}
	if note != "" {
		fmt.Println(note)
	}
	slack := "not connected (promptconduit team alerts slack <webhook-url>)"
	if sum["slackConnected"] == true {
		slack = "connected"
	}
	fmt.Printf("%s budget alerts\n\n", org.Name)
	fmt.Println("  Email   owners and admins")
	fmt.Printf("  Slack   %s\n", slack)
	return nil
}

func runTeamAlertsSlack(cmd *cobra.Command, args []string) error {
	switch {
	case slackOff && len(args) > 0:
		return fmt.Errorf("pass a webhook URL or --disconnect, not both")
	case !slackOff && len(args) == 0:
		return fmt.Errorf("pass a Slack incoming webhook URL, or --disconnect --yes to stop Slack alerts")
	case slackOff && !slackYes:
		return fmt.Errorf("--disconnect stops budget alerts going to Slack. Re-run with --yes to confirm")
	}
	return teamRun(func(c *client.Client, org teamOrgInfo) error {
		if slackOff {
			resp := c.DisconnectSlackAlerts(org.ID)
			if !resp.Success {
				return teamAPIError("failed to disconnect Slack", resp)
			}
			return outputAlerts(org, resp.Data, "Slack budget alerts disconnected.")
		}
		webhook, err := slackWebhookArg(args[0], os.Stdin)
		if err != nil {
			return err
		}
		resp := c.ConnectSlackAlerts(org.ID, webhook)
		if !resp.Success {
			return teamAPIError("failed to connect Slack", resp)
		}
		return outputAlerts(org, resp.Data, "Slack connected; a test message was posted to the channel.")
	})
}

// slackWebhookArg returns the webhook URL; "-" reads it from stdin so the
// secret URL stays out of shell history.
func slackWebhookArg(arg string, stdin io.Reader) (string, error) {
	if arg != "-" {
		return strings.TrimSpace(arg), nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		return "", fmt.Errorf("failed to read the webhook URL from stdin: %w", err)
	}
	if u := strings.TrimSpace(string(b)); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("no webhook URL on stdin")
}
