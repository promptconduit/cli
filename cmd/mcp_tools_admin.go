package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/mcp"
)

// MCP tools for team billing, members and Slack alerts, plus your own synced
// spend, insights and coaching trends. They share code with `team billing`,
// `team members`, `team alerts` and `insights`.

var confirmProp = boolProp("Must be true. Confirms this destructive change, after the user has agreed to it.")

func adminMCPTools() []*mcp.Tool {
	return []*mcp.Tool{
		// ---- billing ----
		{
			Name:  "team_billing",
			Title: "Team billing status",
			Description: "The team's plan, seats used and available, subscription status, trial end, and which paid plans can be checked out. " +
				"Any member can read it.",
			InputSchema: objectSchema(schemaProps{"org": orgProp}),
			Annotations: readOnly("Team billing status"),
			Handler:     toolTeamBilling,
		},
		{
			Name:  "team_billing_checkout",
			Title: "Start a team subscription",
			Description: "Create a Stripe Checkout page for the Team or Growth plan and return its URL (owners and admins). " +
				"Give the URL to the user to open; payment details are entered only on that page, and nothing is charged until they finish. " +
				"Teams that already subscribe change plans with team_billing_portal.",
			InputSchema: objectSchema(schemaProps{
				"org":  orgProp,
				"tier": strProp("Plan to subscribe to.", paidTiers...),
			}, "tier"),
			Annotations: teamWrite("Start a team subscription", false, false),
			Handler:     toolTeamBillingCheckout,
		},
		{
			Name:  "team_billing_portal",
			Title: "Billing portal link",
			Description: "Return a Stripe billing portal URL where the user can change plan, update the card, see invoices or cancel (owners and admins). " +
				"Give the URL to the user to open.",
			InputSchema: objectSchema(schemaProps{"org": orgProp}),
			Annotations: teamWrite("Billing portal link", false, true),
			Handler:     toolTeamBillingPortal,
		},

		// ---- members ----
		{
			Name:        "team_members",
			Title:       "Team members",
			Description: "Team members with email, name, role and member id.",
			InputSchema: objectSchema(schemaProps{"org": orgProp}),
			Annotations: readOnly("Team members"),
			Handler:     toolTeamMembers,
		},
		{
			Name:  "team_set_member_role",
			Title: "Change a member's role",
			Description: "Change a member's role to owner, admin, member or viewer (owners and admins; only owners grant admin or owner). " +
				"A team always keeps at least one owner. Ask the user before demoting anyone.",
			InputSchema: objectSchema(schemaProps{
				"org":    orgProp,
				"member": strProp("Member email (or user id / member id from team_members)."),
				"role":   strProp("New role.", assignedRoles...),
			}, "member", "role"),
			Annotations: teamWrite("Change a member's role", true, true),
			Handler:     toolTeamSetMemberRole,
		},
		{
			Name:  "team_remove_member",
			Title: "Remove a team member",
			Description: "Remove a member from the team; they lose access to team data at once (owners and admins). " +
				"Only call this after the user has explicitly agreed, and pass confirm: true.",
			InputSchema: objectSchema(schemaProps{
				"org":     orgProp,
				"member":  strProp("Member email (or user id / member id from team_members)."),
				"confirm": confirmProp,
			}, "member", "confirm"),
			Annotations: teamWrite("Remove a team member", true, true),
			Handler:     toolTeamRemoveMember,
		},

		// ---- Slack alerts ----
		{
			Name:  "team_connect_slack",
			Title: "Send budget alerts to Slack",
			Description: "Post budget alerts (80% and 100% of a monthly budget) to a Slack channel through an incoming webhook (owners and admins). " +
				"A test message is posted first, so a bad URL fails right away. Replaces any webhook already connected.",
			InputSchema: objectSchema(schemaProps{
				"org":         orgProp,
				"webhook_url": strProp("Slack incoming webhook URL (https://hooks.slack.com/services/…)."),
			}, "webhook_url"),
			Annotations: teamWrite("Send budget alerts to Slack", false, true),
			Handler:     toolTeamConnectSlack,
		},
		{
			Name:  "team_disconnect_slack",
			Title: "Stop Slack budget alerts",
			Description: "Stop posting budget alerts to Slack; email alerts continue (owners and admins). " +
				"Only call this after the user has explicitly agreed, and pass confirm: true.",
			InputSchema: objectSchema(schemaProps{"org": orgProp, "confirm": confirmProp}, "confirm"),
			Annotations: teamWrite("Stop Slack budget alerts", true, true),
			Handler:     toolTeamDisconnectSlack,
		},

		// ---- personal, synced ----
		{
			Name:  "my_spend",
			Title: "Your synced AI spend",
			Description: "Your own AI coding spend across every machine synced to your account: total, change vs the previous period, " +
				"cache savings, and breakdowns by model, day and branch. Requires `promptconduit login`; for this machine only use cost_history.",
			InputSchema: objectSchema(schemaProps{"period": strProp("Time window (default 7d).", "7d", "30d", "90d")}),
			Annotations: readOnly("Your synced AI spend"),
			Handler:     toolMySpend,
		},
		{
			Name:  "my_insights",
			Title: "Your usage insights",
			Description: "Your synced usage summary: sessions, activity streaks, top tools and success rates, and error patterns. " +
				"Requires `promptconduit login`.",
			InputSchema: objectSchema(schemaProps{
				"period": strProp("Time window (default 7d).", "24h", "7d", "30d", "90d"),
				"repo":   strProp("Only this repo (e.g. acme/api)."),
			}),
			Annotations: readOnly("Your usage insights"),
			Handler:     toolMyInsights,
		},
		{
			Name:  "my_trends",
			Title: "Your agent coaching trends",
			Description: "How you work with AI agents over time (plan mode, subagents, interruptions, tool success, cache hit rate) and " +
				"coaching suggestions, each with a guide URL. Requires `promptconduit login`.",
			InputSchema: objectSchema(schemaProps{"period": strProp("Time window (default 30d).", "7d", "30d", "90d")}),
			Annotations: readOnly("Your agent coaching trends"),
			Handler:     toolMyTrends,
		},
	}
}

// orgArgs is the argument shape of the tools that only take a team.
type orgArgs struct {
	Org string `json:"org"`
}

// ---- billing ----

func toolTeamBilling(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a orgArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		return teamData("failed to get billing status", c.GetTeamBilling(o.ID), o)
	})
}

func toolTeamBillingCheckout(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org  string `json:"org"`
		Tier string `json:"tier"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if !oneOf(a.Tier, paidTiers) {
		return nil, fmt.Errorf("tier must be team or growth")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		out, err := teamData("failed to start checkout", c.CreateTeamCheckout(o.ID, a.Tier), o)
		return withNextStep(out, err,
			"Give the user this url to open. Payment details are entered on Stripe's page; nothing is charged until they finish checkout.")
	})
}

func toolTeamBillingPortal(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a orgArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		out, err := teamData("failed to open billing portal", c.CreateTeamBillingPortal(o.ID), o)
		return withNextStep(out, err, "Give the user this url to open. It expires shortly, so create a new one if needed.")
	})
}

func withNextStep(out interface{}, err error, next string) (interface{}, error) {
	if m, ok := out.(map[string]interface{}); ok {
		m["next_step"] = next
	}
	return out, err
}

// ---- members ----

func toolTeamMembers(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a orgArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		members, _, err := listMembers(c, o)
		if err != nil {
			return nil, err
		}
		rows := make([]interface{}, len(members))
		for i, m := range members {
			rows[i] = map[string]interface{}{"memberId": m.ID, "userId": m.UserID, "email": m.Email, "name": m.Name, "role": m.Role}
		}
		return withOrg(map[string]interface{}{"members": rows}, o), nil
	})
}

func toolTeamSetMemberRole(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org    string `json:"org"`
		Member string `json:"member"`
		Role   string `json:"role"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Member) == "" {
		return nil, fmt.Errorf("member is required")
	}
	if !oneOf(a.Role, assignedRoles) {
		return nil, fmt.Errorf("role must be one of owner, admin, member, viewer")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		return setMemberRole(c, o, a.Member, a.Role)
	})
}

func toolTeamRemoveMember(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org     string `json:"org"`
		Member  string `json:"member"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Member) == "" {
		return nil, fmt.Errorf("member is required")
	}
	if !a.Confirm {
		return nil, fmt.Errorf("removing a member needs confirm: true, after the user has agreed")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		return removeMember(c, o, a.Member)
	})
}

// ---- Slack alerts ----

func toolTeamConnectSlack(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org        string `json:"org"`
		WebhookURL string `json:"webhook_url"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.WebhookURL) == "" {
		return nil, fmt.Errorf("webhook_url is required")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		resp := c.ConnectSlackAlerts(o.ID, strings.TrimSpace(a.WebhookURL))
		if !resp.Success {
			return nil, teamAPIError("failed to connect Slack", resp)
		}
		return alertsSummary(o, resp.Data), nil
	})
}

func toolTeamDisconnectSlack(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org     string `json:"org"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if !a.Confirm {
		return nil, fmt.Errorf("disconnecting Slack needs confirm: true, after the user has agreed")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		resp := c.DisconnectSlackAlerts(o.ID)
		if !resp.Success {
			return nil, teamAPIError("failed to disconnect Slack", resp)
		}
		return alertsSummary(o, resp.Data), nil
	})
}

// ---- personal, synced ----

// meCall runs fn with a signed-in client.
func meCall(action string, fn func(c *client.Client) *client.APIResponse) (interface{}, error) {
	c, err := insightsClient()
	if err != nil {
		return nil, err
	}
	resp := fn(c)
	if !resp.Success {
		return nil, teamAPIError(action, resp)
	}
	return resp.Data, nil
}

type periodArgs struct {
	Period string `json:"period"`
}

func cloudPeriod(p, fallback string) (string, error) {
	if p == "" {
		return fallback, nil
	}
	return checkPeriod(p)
}

func toolMySpend(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a periodArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	period, err := cloudPeriod(a.Period, "7d")
	if err != nil {
		return nil, err
	}
	return meCall("failed to get your spend", func(c *client.Client) *client.APIResponse { return c.GetMyCost(period) })
}

func toolMyInsights(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Period string `json:"period"`
		Repo   string `json:"repo"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Period == "" {
		a.Period = "7d"
	}
	if !oneOf(a.Period, []string{"24h", "7d", "30d", "90d"}) {
		return nil, fmt.Errorf("period must be 24h, 7d, 30d or 90d, got %q", a.Period)
	}
	c, err := insightsClient()
	if err != nil {
		return nil, err
	}
	summary := c.GetInsights(a.Period, a.Repo)
	if !summary.Success {
		return nil, teamAPIError("failed to get insights", summary)
	}
	out := map[string]interface{}{"summary": summary.Data}
	// Tools and errors add detail; a failure there shouldn't hide the summary.
	if tools := c.GetInsightsTools(a.Period, a.Repo); tools.Success {
		out["tools"] = tools.Data
	} else {
		out["tools_error"] = teamAPIError("failed to get tool usage", tools).Error()
	}
	if errs := c.GetInsightsErrors(a.Period, a.Repo); errs.Success {
		out["errors"] = errs.Data
	} else {
		out["errors_error"] = teamAPIError("failed to get error patterns", errs).Error()
	}
	return out, nil
}

func toolMyTrends(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a periodArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	period, err := cloudPeriod(a.Period, "30d")
	if err != nil {
		return nil, err
	}
	return meCall("failed to get your trends", func(c *client.Client) *client.APIResponse { return c.GetMyTrends(period) })
}
