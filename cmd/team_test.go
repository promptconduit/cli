package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

var testOrgs = []teamOrgInfo{
	{ID: "o1", Name: "Acme Eng", Slug: "acme"},
	{ID: "o2", Name: "Side Project", Slug: "side"},
}

func TestPickTeamOrg(t *testing.T) {
	for _, want := range []string{"o2", "side", "side project"} {
		got, err := pickTeamOrg(testOrgs, want)
		if err != nil || got.ID != "o2" {
			t.Errorf("pickTeamOrg(%q) = %v, %v", want, got, err)
		}
	}
	if got, err := pickTeamOrg(testOrgs[:1], ""); err != nil || got.ID != "o1" {
		t.Errorf("single org: %v, %v", got, err)
	}
	if _, err := pickTeamOrg(testOrgs, ""); err == nil || !strings.Contains(err.Error(), "--org") || !strings.Contains(err.Error(), "acme") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := pickTeamOrg(testOrgs, "nope"); err == nil || !strings.Contains(err.Error(), "Side Project (side)") {
		t.Errorf("no match: %v", err)
	}
	if _, err := pickTeamOrg(nil, ""); err == nil {
		t.Error("no orgs: want error")
	}
}

func TestBudgetScope(t *testing.T) {
	cases := []struct {
		orgWide      bool
		repo, member string
		scope, value string
		ok           bool
	}{
		{true, "", "", "org", "", true},
		{false, "acme/api", "", "repo", "acme/api", true},
		{false, "", "a@b.co", "member", "a@b.co", true},
		{false, "", "", "", "", false},
		{true, "acme/api", "", "", "", false},
	}
	for _, tc := range cases {
		scope, value, err := budgetScope(tc.orgWide, tc.repo, tc.member)
		if (err == nil) != tc.ok || scope != tc.scope || value != tc.value {
			t.Errorf("budgetScope(%v,%q,%q) = %q,%q,%v", tc.orgWide, tc.repo, tc.member, scope, value, err)
		}
	}
}

func TestTeamAPIError(t *testing.T) {
	err := teamAPIError("failed", &client.APIResponse{StatusCode: 403, Error: "HTTP 403: {...}",
		Data: map[string]interface{}{"detail": "upgrade to Team"}})
	if err.Error() != "failed: upgrade to Team" {
		t.Errorf("got %q", err)
	}
	err = teamAPIError("failed", &client.APIResponse{StatusCode: 401, Error: "HTTP 401"})
	if !strings.Contains(err.Error(), "promptconduit login") {
		t.Errorf("401 should suggest login: %q", err)
	}
}

func TestInviteToken(t *testing.T) {
	cases := map[string]string{
		"https://app.promptconduit.dev/invite/abc123":  "abc123",
		"https://app.promptconduit.dev/invite/abc123/": "abc123",
		"https://app.promptconduit.dev/invite/abc?x=1": "abc",
		"  abc123 ":                             "abc123",
		"https://app.promptconduit.dev/invite/": "",
	}
	for in, want := range cases {
		if got := inviteToken(in); got != want {
			t.Errorf("inviteToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTeamCommandsSilenceUsage(t *testing.T) {
	for _, c := range []*cobra.Command{teamCmd, teamSpendCmd, teamBudgetsSetCmd, teamJoinCmd} {
		if !c.SilenceUsage {
			t.Errorf("%s: SilenceUsage = false", c.Name())
		}
	}
}

// spend and export each own their --by default; a shared variable would let
// export's "repo" default collapse spend's overview to one section.
func TestTeamByDefaults(t *testing.T) {
	if spendBy != "" || teamSpendCmd.Flags().Lookup("by").DefValue != "" {
		t.Errorf("spend --by default = %q", spendBy)
	}
	if exportBy != "repo" {
		t.Errorf("export --by default = %q", exportBy)
	}
}

func TestUSD(t *testing.T) {
	cases := map[float64]string{0: "$0.00", 3.456: "$3.46", 1234.5: "$1,234.50", 1234567.891: "$1,234,567.89", -12: "-$12.00"}
	for in, want := range cases {
		if got := usd(in); got != want {
			t.Errorf("usd(%v) = %q, want %q", in, got, want)
		}
	}
}

func decode(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWriteTeamSpend(t *testing.T) {
	data := decode(t, `{
		"period": "30d",
		"totals": {"usd": 1234.5, "runs": 1200, "sessions": 40, "members": 3},
		"cache": {"savingsUsd": 50, "hitRate": 0.8},
		"byMember": [{"name": "Ada", "email": "ada@x.co", "usd": 1000, "sessions": 30},
		             {"name": "", "email": "bob@x.co", "usd": 234.5, "sessions": 1}],
		"byRepo": [{"repoName": "acme/api", "client": "Acme", "usd": 1234.5, "sessions": 40}],
		"byModel": [],
		"byClient": [{"client": null, "usd": 1234.5, "repos": 2, "sessions": 40}],
		"byPR": [{"repoName": "acme/api", "prNumber": 42, "usd": 10, "sessions": 2}]
	}`)
	var buf bytes.Buffer
	if err := writeTeamSpend(&buf, testOrgs[0], data, "", 1); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Acme Eng · last 30d", "$1,234.50", "1,200 runs", "Cache saved  $50.00 (80% hit rate)",
		"Ada <ada@x.co>", "… 1 more", "acme/api [Acme]", "By model\n  (no spend)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	buf.Reset()
	_ = writeTeamSpend(&buf, testOrgs[0], data, "client", 0)
	if !strings.Contains(buf.String(), "(untagged)") || strings.Contains(buf.String(), "By member") {
		t.Errorf("--by client:\n%s", buf.String())
	}

	buf.Reset()
	_ = writeTeamSpend(&buf, testOrgs[0], data, "pr", 0)
	if !strings.Contains(buf.String(), "acme/api#42") {
		t.Errorf("--by pr:\n%s", buf.String())
	}
}

func TestWriteBudgets(t *testing.T) {
	data := decode(t, `{"slackConnected": true, "budgets": [
		{"id": "b1", "scope": "repo", "scopeValue": "acme/api", "label": "acme/api",
		 "monthlyUsd": 500, "spendUsd": 400, "pct": 80, "projectedUsd": 650}]}`)
	var buf bytes.Buffer
	if err := writeBudgets(&buf, testOrgs[0], data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Slack alerts: on", "acme/api", "$400.00", "$500.00", "80%", "$650.00", "b1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	buf.Reset()
	_ = writeBudgets(&buf, testOrgs[0], map[string]interface{}{"budgets": []interface{}{}})
	if !strings.Contains(buf.String(), "budgets set --org-wide") {
		t.Errorf("empty:\n%s", buf.String())
	}
}

func TestWriteTeamSavings(t *testing.T) {
	data := decode(t, `{"period": "30d",
		"spend": {"usd": 900, "monthlyUsd": 900},
		"cache": {"savingsUsd": 100, "monthlyUsd": 100},
		"modelMix": {"turns": 1500, "monthlyUsd": 120},
		"runaway": {"count": 1, "usd": 80, "thresholdUsd": 25, "typicalSessionUsd": 5,
		            "top": [{"usd": 80, "name": "Ada", "repoName": "acme/api", "sessionId": "s1"}]},
		"opportunity": {"monthlyUsd": 120}}`)
	var buf bytes.Buffer
	writeTeamSavings(&buf, testOrgs[0], data)
	out := buf.String()
	for _, want := range []string{"$120.00/month if 1,500 light", "Runaway sessions 1 costing $80.00", "Ada", "s1", "Opportunity      $120.00/month"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
