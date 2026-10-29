package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/cost"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/promptconduit/cli/internal/mcp"
	"github.com/promptconduit/cli/internal/sessions"
)

// The MCP tools reuse the same code paths as `promptconduit cost`,
// `sessions` and `team`, so the agent and the terminal always agree.

// ============================================================================
// Schema and argument helpers
// ============================================================================

type schemaProps map[string]interface{}

func objectSchema(props schemaProps, required ...string) map[string]interface{} {
	s := map[string]interface{}{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string, enum ...string) map[string]interface{} {
	p := map[string]interface{}{"type": "string", "description": desc}
	if len(enum) > 0 {
		p["enum"] = enum
	}
	return p
}

func intProp(desc string, min, max int) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

func boolProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

var (
	orgProp    = strProp("Team id, slug or name. Optional when you belong to one team.")
	periodProp = strProp("Time window (default 30d).", "7d", "30d", "90d")
)

func readOnly(title string) *mcp.Annotations {
	return &mcp.Annotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

// teamWrite marks a tool that changes shared team settings.
func teamWrite(title string, destructive, idempotent bool) *mcp.Annotations {
	return &mcp.Annotations{Title: title, DestructiveHint: destructive, IdempotentHint: idempotent}
}

func decodeArgs(raw json.RawMessage, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func checkPeriod(p string) (string, error) {
	switch p {
	case "":
		return "30d", nil
	case "7d", "30d", "90d":
		return p, nil
	}
	return "", fmt.Errorf("period must be 7d, 30d or 90d, got %q", p)
}

// teamCall resolves the client and org, then runs fn.
func teamCall(org string, fn func(c *client.Client, o teamOrgInfo) (interface{}, error)) (interface{}, error) {
	c, err := newTeamClient()
	if err != nil {
		return nil, err
	}
	o, err := resolveTeamOrgFor(c, org)
	if err != nil {
		return nil, err
	}
	return fn(c, o)
}

// teamData returns a successful reply's data tagged with its org.
func teamData(action string, resp *client.APIResponse, o teamOrgInfo) (interface{}, error) {
	if !resp.Success {
		return nil, teamAPIError(action, resp)
	}
	return withOrg(resp.Data, o), nil
}

// ============================================================================
// Tools
// ============================================================================

func mcpTools() []*mcp.Tool {
	return append([]*mcp.Tool{
		// ---- local, Free tier ----
		{
			Name:  "session_cost",
			Title: "Current session cost",
			Description: "What the newest AI coding session in a workspace has cost so far: total USD, tokens, and a per-model breakdown, " +
				"priced locally from Claude Code transcripts (subagents included) and Cursor hook data. Models missing from the rate table are listed in unpriced_models. " +
				"No account needed; nothing leaves the machine.",
			InputSchema: objectSchema(schemaProps{
				"cwd": strProp("Absolute workspace directory. Default: the directory the agent runs in, falling back to the newest session on the machine."),
			}),
			Annotations: readOnly("Current session cost"),
			Handler:     toolSessionCost,
		},
		{
			Name:  "cost_history",
			Title: "Daily AI cost",
			Description: "Daily AI coding cost on this machine over recent days (USD, requests, tokens), from the local event log, subagent spend included. " +
				"Requests on models missing from the rate table are counted in unpriced_models. No account needed.",
			InputSchema: objectSchema(schemaProps{
				"days": intProp("Number of days including today (default 7).", 1, 90),
			}),
			Annotations: readOnly("Daily AI cost"),
			Handler:     toolCostHistory,
		},
		{
			Name:  "recent_sessions",
			Title: "Recent sessions",
			Description: "Recent AI coding sessions on this machine, newest first, with their directory, branch, last prompt and session id " +
				"(resume one with `claude --resume <id>` from its directory). No account needed.",
			InputSchema: objectSchema(schemaProps{
				"within_hours":    intProp("Only sessions active in this many hours (default 24).", 1, 720),
				"cwd":             strProp("Only sessions under this directory."),
				"include_running": boolProp("Include sessions that still have a live process (default true)."),
			}),
			Annotations: readOnly("Recent sessions"),
			Handler:     toolRecentSessions,
		},
		{
			Name:        "setup_status",
			Title:       "PromptConduit setup status",
			Description: "Which AI tools have PromptConduit hooks and the MCP server installed, whether you are signed in, and whether events sync or stay local.",
			InputSchema: objectSchema(schemaProps{}),
			Annotations: readOnly("Setup status"),
			Handler:     toolSetupStatus,
		},

		// ---- team, reads ----
		{
			Name:        "team_list",
			Title:       "Your teams",
			Description: "The PromptConduit teams you belong to, with your role and plan. Requires `promptconduit login`.",
			InputSchema: objectSchema(schemaProps{}),
			Annotations: readOnly("Your teams"),
			Handler:     toolTeamList,
		},
		{
			Name:  "team_spend",
			Title: "Team AI spend",
			Description: "Team AI coding spend for a period: totals, cache savings and breakdowns by member, repo, client, model, branch, pull request and day. " +
				"Pass breakdown to get just one list. Lists are sorted by spend (days newest first) and capped at limit.",
			InputSchema: objectSchema(schemaProps{
				"org":       orgProp,
				"period":    periodProp,
				"breakdown": strProp("Return only this breakdown.", "member", "repo", "client", "model", "branch", "pr", "day"),
				"limit":     intProp("Max rows per breakdown (default 10).", 1, 500),
			}),
			Annotations: readOnly("Team AI spend"),
			Handler:     toolTeamSpend,
		},
		{
			Name:  "team_savings",
			Title: "Team savings report",
			Description: "Where the team could spend less: cache savings already earned, the monthly saving from running light premium-model turns on Sonnet, " +
				"and runaway sessions that cost far more than typical.",
			InputSchema: objectSchema(schemaProps{"org": orgProp, "period": periodProp}),
			Annotations: readOnly("Team savings report"),
			Handler:     toolTeamSavings,
		},
		{
			Name:  "team_budgets",
			Title: "Team budgets",
			Description: "Monthly AI budgets (whole team, per repo, per member) with month-to-date spend, percent used and projected month-end spend. " +
				"Alerts fire at 80% and 100%.",
			InputSchema: objectSchema(schemaProps{"org": orgProp}),
			Annotations: readOnly("Team budgets"),
			Handler:     toolTeamBudgets,
		},
		{
			Name:        "team_export_csv",
			Title:       "Export team spend (CSV)",
			Description: "Team spend as CSV text grouped by client, repo, member, pr or day, for invoices or spreadsheets.",
			InputSchema: objectSchema(schemaProps{
				"org":    orgProp,
				"period": periodProp,
				"by":     strProp("Grouping (default repo).", "client", "repo", "member", "pr", "day"),
			}),
			Annotations: readOnly("Export team spend"),
			Handler:     toolTeamExport,
		},

		// ---- team, writes ----
		{
			Name:  "team_set_budget",
			Title: "Set a team budget",
			Description: "Create or change a monthly budget for the whole team, one repo, or one member (owners and admins). " +
				"Setting a scope that already has a budget changes its amount.",
			InputSchema: objectSchema(schemaProps{
				"org":         orgProp,
				"scope":       strProp("What the budget covers.", "org", "repo", "member"),
				"target":      strProp("Repo name (e.g. acme/api) for scope=repo, or member email for scope=member. Omit for scope=org."),
				"monthly_usd": map[string]interface{}{"type": "number", "description": "Monthly budget in USD.", "exclusiveMinimum": 0},
			}, "scope", "monthly_usd"),
			Annotations: teamWrite("Set a team budget", false, true),
			Handler:     toolTeamSetBudget,
		},
		{
			Name:        "team_remove_budget",
			Title:       "Remove a team budget",
			Description: "Delete a budget by id (from team_budgets). Owners and admins.",
			InputSchema: objectSchema(schemaProps{"org": orgProp, "budget_id": strProp("Budget id from team_budgets.")}, "budget_id"),
			Annotations: teamWrite("Remove a team budget", true, true),
			Handler:     toolTeamRemoveBudget,
		},
		{
			Name:  "team_tag_repo",
			Title: "Tag a repo with a client",
			Description: "Attribute a repo's spend to a client so it rolls up per client in team_spend and exports. " +
				"An empty client removes the tag. Owners and admins.",
			InputSchema: objectSchema(schemaProps{
				"org":    orgProp,
				"repo":   strProp("Repo name as it appears in team_spend (e.g. acme/api)."),
				"client": strProp("Client name; empty to clear."),
			}, "repo", "client"),
			Annotations: teamWrite("Tag a repo with a client", false, true),
			Handler:     toolTeamTagRepo,
		},
		{
			Name:        "team_invite",
			Title:       "Invite a teammate",
			Description: "Invite someone by email and get the invite link to send them. Owners and admins.",
			InputSchema: objectSchema(schemaProps{
				"org":   orgProp,
				"email": strProp("Their email address."),
				"role":  strProp("Role (default member).", "member", "admin", "viewer"),
			}, "email"),
			Annotations: teamWrite("Invite a teammate", false, false),
			Handler:     toolTeamInvite,
		},
		{
			Name:  "team_create",
			Title: "Create a team",
			Description: "Create a PromptConduit team with you as owner (free for up to 3 people). " +
				"By default your last 30 days of usage is added so the team view isn't empty.",
			InputSchema: objectSchema(schemaProps{
				"name":          strProp("Team name."),
				"share_history": boolProp("Add your last 30 days of usage to the team (default true)."),
			}, "name"),
			Annotations: teamWrite("Create a team", false, false),
			Handler:     toolTeamCreate,
		},
		{
			Name:        "team_join",
			Title:       "Join a team",
			Description: "Accept a team invite link sent to your account's email.",
			InputSchema: objectSchema(schemaProps{
				"invite":        strProp("The invite link (…/invite/<token>) or token."),
				"share_history": boolProp("Also add your last 30 days of usage to the team (default false)."),
			}, "invite"),
			Annotations: teamWrite("Join a team", false, false),
			Handler:     toolTeamJoin,
		},
	}, adminMCPTools()...)
}

// ---- local handlers ----

func toolSessionCost(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Cwd string `json:"cwd"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	explicit := a.Cwd != ""
	cwd := a.Cwd
	if !explicit {
		// Claude Code starts MCP servers in the project directory; other hosts
		// may not, which the fallback below covers.
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	summary, ok, err := latestSessionCost(cwd)
	if err != nil {
		return nil, err
	}
	if ok {
		return withUnpriced(map[string]interface{}{"workspace": cwd, "session": summary}, summary), nil
	}
	if !explicit {
		// Not started in a project with sessions: report the newest session
		// on this machine and say so.
		if summary, ok, err = newestSessionCostAnywhere(); err != nil {
			return nil, err
		}
		if ok {
			return withUnpriced(map[string]interface{}{
				"workspace": "newest session on this machine",
				"note":      fmt.Sprintf("No session found for %s; pass cwd to pick a workspace.", cwd),
				"session":   summary,
			}, summary), nil
		}
	}
	return fmt.Sprintf("No AI session cost recorded yet for %s. Claude Code sessions are priced from their transcripts; "+
		"for Cursor run `promptconduit install cursor`.", cwd), nil
}

// withUnpriced names the session's unpriced models at the top level so an
// agent can't read a partial total as the whole cost.
func withUnpriced(out map[string]interface{}, s cost.SessionSummary) map[string]interface{} {
	var models []string
	for _, m := range s.ByModel {
		if !m.ModelPriced {
			models = append(models, m.Model)
		}
	}
	if len(models) > 0 {
		out["unpriced_models"] = models
		out["unpriced_note"] = "These models are missing from the rate table: their tokens are counted but not their cost, so the total is a lower bound."
	}
	return out
}

func toolCostHistory(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Days int `json:"days"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Days == 0 {
		a.Days = 7
	}
	if a.Days < 1 || a.Days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	days, err := loadCostHistory(a.Days)
	if err != nil {
		return nil, err
	}
	if days == nil {
		days = []costDay{}
	}
	total, subagent, unpriced := 0.0, 0.0, 0
	for _, d := range days {
		total += d.CostTotal
		subagent += d.SubagentCost
		unpriced += d.Unpriced
	}
	out := map[string]interface{}{
		"days":              days,
		"total_usd":         total,
		"subagent_usd":      subagent,
		"unpriced_requests": unpriced,
		"unpriced_models":   mergeModelCounts(days),
		"window_days":       a.Days,
	}
	if unpriced > 0 {
		out["unpriced_note"] = "Requests on models missing from the rate table count tokens but no cost, so total_usd is a lower bound."
	}
	if retention := client.LoadConfig().RetentionDays(); retention > 0 && a.Days > retention {
		out["note"] = fmt.Sprintf("Local history is kept for at least %d days; earlier days may be incomplete.", retention)
	}
	return out, nil
}

func toolRecentSessions(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		WithinHours    int    `json:"within_hours"`
		Cwd            string `json:"cwd"`
		IncludeRunning *bool  `json:"include_running"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.WithinHours == 0 {
		a.WithinHours = 24
	}
	list, err := sessions.ReadRecent(time.Duration(a.WithinHours)*time.Hour, time.Now())
	if err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	sessions.MarkAlive(list, sessions.LiveClaudeCwds())
	if a.Cwd != "" {
		if base, err := filepath.Abs(a.Cwd); err == nil {
			list = filterUnder(list, base)
		}
	}
	if a.IncludeRunning != nil && !*a.IncludeRunning {
		list = filterInterrupted(list)
	}
	if list == nil {
		list = []sessions.Session{}
	}
	return map[string]interface{}{"sessions": list}, nil
}

func toolSetupStatus(_ context.Context, raw json.RawMessage) (interface{}, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	cfg := client.LoadConfig()
	home, _ := os.UserHomeDir()

	tools := map[string]interface{}{}
	for _, t := range mcpTargets {
		tools[t.Tool] = map[string]bool{"hooks": t.hooksInstalled(home), "mcp": t.registered(home)}
	}
	mode := "local-only (events stay on this machine)"
	if cfg.ShouldSend() {
		mode = "cloud sync (events also sent to the platform)"
	}
	_, logErr := os.Stat(eventlog.EventsJSONLPath())
	out := map[string]interface{}{
		"cli_version": Version,
		"signed_in":   cfg.IsConfigured(),
		"mode":        mode,
		"api_url":     cfg.APIURL,
		"event_log":   map[string]interface{}{"path": eventlog.EventsJSONLPath(), "exists": logErr == nil},
		"tools":       tools,
	}
	if !cfg.IsConfigured() {
		out["next_step"] = "Run `promptconduit login` to use the team tools."
	}
	return out, nil
}

// ---- team read handlers ----

func toolTeamList(_ context.Context, raw json.RawMessage) (interface{}, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	c, err := newTeamClient()
	if err != nil {
		return nil, err
	}
	orgs, err := listTeamOrgs(c)
	if err != nil {
		return nil, err
	}
	out := make([]interface{}, len(orgs))
	for i, o := range orgs {
		out[i] = map[string]interface{}{"id": o.ID, "name": o.Name, "slug": o.Slug, "role": o.Role, "plan": o.Plan}
	}
	res := map[string]interface{}{"organizations": out}
	if len(orgs) == 0 {
		res["next_step"] = "Not on a team yet. team_create starts one (free for up to 3 people)."
	}
	return res, nil
}

func toolTeamSpend(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org       string `json:"org"`
		Period    string `json:"period"`
		Breakdown string `json:"breakdown"`
		Limit     int    `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	period, err := checkPeriod(a.Period)
	if err != nil {
		return nil, err
	}
	if _, ok := spendBreakdowns[a.Breakdown]; a.Breakdown != "" && !ok {
		return nil, fmt.Errorf("breakdown must be one of member, repo, client, model, branch, pr, day")
	}
	if a.Limit == 0 {
		a.Limit = 10
	}
	if a.Limit < 1 || a.Limit > 500 {
		return nil, fmt.Errorf("limit must be between 1 and 500")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		resp := c.GetTeamCost(o.ID, period)
		if !resp.Success {
			return nil, teamAPIError("failed to get team spend", resp)
		}
		return withOrg(trimSpend(resp.Data, a.Breakdown, a.Limit), o), nil
	})
}

// trimSpend keeps the agent's context small: only the requested breakdown
// (or all of them), each capped at limit, with days newest first and a count
// of what was left out.
func trimSpend(data map[string]interface{}, only string, limit int) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range data {
		out[k] = v
	}
	omitted := map[string]int{}
	for name, key := range spendBreakdowns {
		rows, ok := data[key].([]interface{})
		if !ok {
			continue
		}
		if only != "" && name != only {
			delete(out, key)
			continue
		}
		if name == "day" {
			rows = reversed(rows)
		}
		if len(rows) > limit {
			omitted[key] = len(rows) - limit
			rows = rows[:limit]
		}
		out[key] = rows
	}
	if len(omitted) > 0 {
		out["omittedRows"] = omitted
	}
	return out
}

func toolTeamSavings(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org    string `json:"org"`
		Period string `json:"period"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	period, err := checkPeriod(a.Period)
	if err != nil {
		return nil, err
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		return teamData("failed to get team savings", c.GetTeamSavings(o.ID, period), o)
	})
}

func toolTeamBudgets(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org string `json:"org"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		return teamData("failed to list budgets", c.ListBudgets(o.ID), o)
	})
}

func toolTeamExport(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org    string `json:"org"`
		Period string `json:"period"`
		By     string `json:"by"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	period, err := checkPeriod(a.Period)
	if err != nil {
		return nil, err
	}
	if a.By == "" {
		a.By = "repo"
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		csv, fail := c.ExportTeamCost(o.ID, period, a.By)
		if fail != nil {
			return nil, teamAPIError("failed to export team spend", fail)
		}
		return csv, nil
	})
}

// ---- team write handlers ----

func toolTeamSetBudget(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org        string  `json:"org"`
		Scope      string  `json:"scope"`
		Target     string  `json:"target"`
		MonthlyUSD float64 `json:"monthly_usd"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	scope, value, err := budgetScope(a.Scope == "org", targetIf(a.Scope, "repo", a.Target), targetIf(a.Scope, "member", a.Target))
	if err != nil {
		return nil, fmt.Errorf("scope must be org, or repo/member with a target")
	}
	if a.MonthlyUSD <= 0 {
		return nil, fmt.Errorf("monthly_usd must be greater than 0")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		if scope == "member" {
			if value, err = memberUserID(c, o, value); err != nil {
				return nil, err
			}
		}
		return teamData("failed to set budget", c.SetBudget(o.ID, scope, value, a.MonthlyUSD), o)
	})
}

// targetIf returns target when scope matches want (so budgetScope sees exactly one scope).
func targetIf(scope, want, target string) string {
	if scope != want {
		return ""
	}
	return strings.TrimSpace(target)
}

func toolTeamRemoveBudget(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org      string `json:"org"`
		BudgetID string `json:"budget_id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.BudgetID == "" {
		return nil, fmt.Errorf("budget_id is required")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		return teamData("failed to remove budget", c.DeleteBudget(o.ID, a.BudgetID), o)
	})
}

func toolTeamTagRepo(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org    string `json:"org"`
		Repo   string `json:"repo"`
		Client string `json:"client"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Repo) == "" {
		return nil, fmt.Errorf("repo is required")
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		return teamData("failed to tag repo", c.SetRepoClient(o.ID, strings.TrimSpace(a.Repo), strings.TrimSpace(a.Client)), o)
	})
}

func toolTeamInvite(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Org   string `json:"org"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if !strings.Contains(a.Email, "@") {
		return nil, fmt.Errorf("email must be an email address")
	}
	if a.Role == "" {
		a.Role = "member"
	}
	return teamCall(a.Org, func(c *client.Client, o teamOrgInfo) (interface{}, error) {
		out, err := teamData("failed to invite", c.InviteMember(o.ID, a.Email, a.Role), o)
		if m, ok := out.(map[string]interface{}); ok {
			m["next_step"] = "Send them the inviteUrl. They can open it or run `promptconduit team join <link>`."
		}
		return out, err
	})
}

func toolTeamCreate(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Name         string `json:"name"`
		ShareHistory *bool  `json:"share_history"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(a.Name)) < 2 {
		return nil, fmt.Errorf("name must be at least 2 characters")
	}
	c, err := newTeamClient()
	if err != nil {
		return nil, err
	}
	share := a.ShareHistory == nil || *a.ShareHistory
	resp := c.CreateOrganization(strings.TrimSpace(a.Name), "", share)
	if !resp.Success {
		return nil, teamAPIError("failed to create team", resp)
	}
	data := resp.Data
	if data == nil {
		data = map[string]interface{}{}
	}
	data["next_step"] = "Invite people with team_invite."
	return data, nil
}

func toolTeamJoin(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Invite       string `json:"invite"`
		ShareHistory bool   `json:"share_history"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	token := inviteToken(a.Invite)
	if token == "" {
		return nil, fmt.Errorf("%q is not an invite link (…/invite/<token>) or token", a.Invite)
	}
	c, err := newTeamClient()
	if err != nil {
		return nil, err
	}
	resp := c.AcceptInvite(token)
	if !resp.Success {
		return nil, teamAPIError("failed to join team", resp)
	}
	data := resp.Data
	if data == nil {
		data = map[string]interface{}{}
	}
	if a.ShareHistory {
		org, _ := data["organization"].(map[string]interface{})
		if share := c.AttachHistory(jsonStr(org["id"])); share.Success {
			data["attachedEvents"] = share.Data["attachedEvents"]
		} else {
			data["shareHistoryError"] = teamAPIError("sharing history failed", share).Error()
		}
	}
	return data, nil
}

// ============================================================================
// Prompts (surface as slash commands in Claude Code and others)
// ============================================================================

func mcpPrompts() []*mcp.Prompt {
	return []*mcp.Prompt{
		{
			Name:        "spend_review",
			Title:       "Review AI spend",
			Description: "Summarize AI coding spend and the top ways to cut it.",
			Arguments:   []mcp.PromptArgument{{Name: "period", Description: "7d, 30d or 90d (default 7d)"}},
			Render: func(args map[string]string) string {
				period := args["period"]
				if period == "" {
					period = "7d"
				}
				return fmt.Sprintf(`Review my AI coding spend for the last %s using the PromptConduit tools.

1. Call team_spend and team_savings for period %s. If I'm not signed in or not on a team, use cost_history and session_cost instead and say that team numbers need `+"`promptconduit login`"+`.
2. Report the total, the top members, repos and clients, and the model mix.
3. Call out any runaway sessions.
4. End with the three most valuable concrete actions to spend less, each with its estimated monthly saving.

Keep it under 250 words and use a small table for the breakdowns.`, period, period)
			},
		},
		{
			Name:        "budget_check",
			Title:       "Check budgets",
			Description: "Check every team budget against month-to-date and projected spend.",
			Render: func(map[string]string) string {
				return `Check my team's AI budgets with the PromptConduit team_budgets tool.

For each budget show the scope, spent, budget, percent used and projected month-end spend. Flag anything projected over 100% and anything already over 80%. If there are no budgets, suggest a sensible whole-team budget based on team_spend for the last 30d.

Don't change any budget without asking me first.`
			},
		},
		{
			Name:        "session_cost",
			Title:       "This session's cost",
			Description: "What this coding session has cost so far.",
			Render: func(map[string]string) string {
				return `Use the PromptConduit session_cost tool to tell me what this session has cost so far: the total, the split by model, and how much prompt caching saved. One short paragraph.`
			},
		},
	}
}
