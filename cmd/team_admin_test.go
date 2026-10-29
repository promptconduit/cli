package cmd

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAdminAPI serves the billing, members, Slack and /v1/me endpoints.
func fakeAdminAPI(t *testing.T) *[]string {
	t.Helper()
	isolatedHome(t)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		key := r.Method + " " + r.URL.Path
		switch key {
		case "GET /v1/organizations":
			_, _ = io.WriteString(w, `{"organizations":[{"id":"o1","name":"Acme Eng","slug":"acme","role":"owner","plan":"team"}]}`)
		case "GET /v1/organizations/o1/billing":
			_, _ = io.WriteString(w, `{"billing":{"plan":"team","active":true,"seats":15,"activeMembers":4,"subscriptionStatus":"trialing"}}`)
		case "POST /v1/organizations/o1/billing/checkout":
			_, _ = io.WriteString(w, `{"url":"https://checkout.stripe.com/c/pay/cs_test","sessionId":"cs_test"}`)
		case "POST /v1/organizations/o1/billing/portal":
			_, _ = io.WriteString(w, `{"url":"https://billing.stripe.com/p/session/x"}`)
		case "GET /v1/organizations/o1/members":
			_, _ = io.WriteString(w, `{"members":[
				{"id":"m1","role":"owner","user":{"id":"u1","email":"owner@example.com","name":"Olive"}},
				{"id":"m9","role":"member","user":{"id":"u9","email":"Dev@Example.com","name":"Dev"}}]}`)
		case "PATCH /v1/organizations/o1/members/m9":
			_, _ = io.WriteString(w, `{"member":{"id":"m9","role":"admin"}}`)
		case "DELETE /v1/organizations/o1/members/m9":
			_, _ = io.WriteString(w, `{"removed":true}`)
		case "DELETE /v1/organizations/o1/members/m1":
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"detail":"An organization needs at least one owner; make someone else owner first"}`)
		case "PUT /v1/organizations/o1/budget-alerts/slack", "DELETE /v1/organizations/o1/budget-alerts/slack", "GET /v1/organizations/o1/budgets":
			connected := r.Method == "PUT"
			_, _ = io.WriteString(w, `{"budgets":[],"canManage":true,"thresholds":[80,100],"slackConnected":`+map[bool]string{true: "true", false: "false"}[connected]+`}`)
		case "GET /v1/me/analytics/cost":
			_, _ = io.WriteString(w, `{"period":"30d","totals":{"usd":12.5,"runs":3,"sessions":2},"change":0.25,"byModel":[{"model":"claude-sonnet","usd":12.5,"runs":3}]}`)
		case "GET /v1/me/trends":
			_, _ = io.WriteString(w, `{"period":{"days":30},"metrics":{"prompts":40},"insights":[{"title":"Use plan mode","detail":"…","article_url":"https://promptconduit.dev/coaching/use-plan-mode"}]}`)
		case "GET /v1/me/insights", "GET /v1/me/insights/tools":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"detail":"not found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PROMPTCONDUIT_API_URL", srv.URL)
	t.Setenv("PROMPTCONDUIT_API_KEY", "k")
	return &calls
}

func lastCall(calls *[]string) string { return (*calls)[len(*calls)-1] }

func TestFindMember(t *testing.T) {
	members := []teamMember{{ID: "m9", UserID: "u9", Email: "Dev@Example.com"}}
	for _, who := range []string{"dev@example.com", " u9 ", "m9"} {
		if m, err := findMember(members, testOrgs[0], who); err != nil || m.ID != "m9" {
			t.Errorf("findMember(%q) = %v, %v", who, m, err)
		}
	}
	if _, err := findMember(members, testOrgs[0], "nobody@example.com"); err == nil || !strings.Contains(err.Error(), "team members") {
		t.Errorf("unknown member: %v", err)
	}
}

func TestSlackWebhookArg(t *testing.T) {
	if u, _ := slackWebhookArg(" https://hooks.slack.com/services/a ", nil); u != "https://hooks.slack.com/services/a" {
		t.Errorf("arg = %q", u)
	}
	if u, _ := slackWebhookArg("-", strings.NewReader("https://hooks.slack.com/services/b\n")); u != "https://hooks.slack.com/services/b" {
		t.Errorf("stdin = %q", u)
	}
	if _, err := slackWebhookArg("-", strings.NewReader("  ")); err == nil {
		t.Error("empty stdin accepted")
	}
}

func TestWriteTeamBilling(t *testing.T) {
	var buf bytes.Buffer
	writeTeamBilling(&buf, teamOrgInfo{Name: "Acme", Role: "owner"},
		map[string]interface{}{"billing": map[string]interface{}{"plan": "free", "active": true, "seats": float64(3), "activeMembers": float64(2)}})
	out := buf.String()
	if !strings.Contains(out, "2 of 3 used") || !strings.Contains(out, "billing checkout --tier team") {
		t.Errorf("billing text:\n%s", out)
	}
	buf.Reset()
	writeTeamBilling(&buf, teamOrgInfo{Name: "Acme", Role: "member"},
		map[string]interface{}{"billing": map[string]interface{}{"plan": "team", "active": true}})
	if strings.Contains(buf.String(), "promptconduit team billing") {
		t.Errorf("members can't manage billing, so no hint:\n%s", buf.String())
	}
}

func TestMCPBillingToolsReturnURLs(t *testing.T) {
	calls := fakeAdminAPI(t)
	sc := callTool(t, "team_billing_checkout", `{"tier":"growth"}`)["structuredContent"].(map[string]interface{})
	if sc["url"] != "https://checkout.stripe.com/c/pay/cs_test" || sc["next_step"] == nil {
		t.Errorf("checkout = %v", sc)
	}
	if !strings.Contains(lastCall(calls), `{"tier":"growth"}`) {
		t.Errorf("checkout call = %s", lastCall(calls))
	}
	if r := callTool(t, "team_billing_checkout", `{"tier":"enterprise"}`); r["isError"] != true {
		t.Errorf("bad tier accepted: %v", r)
	}
	if sc := callTool(t, "team_billing_portal", `{}`)["structuredContent"].(map[string]interface{}); sc["url"] == nil {
		t.Errorf("portal = %v", sc)
	}
	if sc := callTool(t, "team_billing", `{}`)["structuredContent"].(map[string]interface{}); sc["billing"] == nil {
		t.Errorf("billing = %v", sc)
	}
}

func TestMCPMemberTools(t *testing.T) {
	calls := fakeAdminAPI(t)
	sc := callTool(t, "team_members", `{}`)["structuredContent"].(map[string]interface{})
	if rows := sc["members"].([]interface{}); len(rows) != 2 || rows[1].(map[string]interface{})["memberId"] != "m9" {
		t.Errorf("members = %v", sc)
	}

	r := callTool(t, "team_set_member_role", `{"member":"dev@example.com","role":"admin"}`)
	if r["isError"] == true || !strings.HasPrefix(lastCall(calls), `PATCH /v1/organizations/o1/members/m9? {"role":"admin"}`) {
		t.Errorf("set role = %v, call %s", r, lastCall(calls))
	}
	if r := callTool(t, "team_set_member_role", `{"member":"dev@example.com","role":"god"}`); r["isError"] != true {
		t.Errorf("bad role accepted: %v", r)
	}

	n := len(*calls)
	if r := callTool(t, "team_remove_member", `{"member":"dev@example.com","confirm":false}`); r["isError"] != true || len(*calls) != n {
		t.Errorf("remove without confirm = %v (calls %d → %d)", r, n, len(*calls))
	}
	if r := callTool(t, "team_remove_member", `{"member":"dev@example.com","confirm":true}`); r["isError"] == true ||
		!strings.HasPrefix(lastCall(calls), "DELETE /v1/organizations/o1/members/m9") {
		t.Errorf("remove = %v, call %s", r, lastCall(calls))
	}
	if r := callTool(t, "team_remove_member", `{"member":"owner@example.com","confirm":true}`); r["isError"] != true ||
		!strings.Contains(toolText(r), "at least one owner") {
		t.Errorf("API refusal not surfaced: %v", r)
	}
}

func TestMCPSlackTools(t *testing.T) {
	calls := fakeAdminAPI(t)
	sc := callTool(t, "team_connect_slack", `{"webhook_url":"https://hooks.slack.com/services/T/B/X"}`)["structuredContent"].(map[string]interface{})
	if sc["slackConnected"] != true || !strings.Contains(lastCall(calls), `"webhookUrl":"https://hooks.slack.com/services/T/B/X"`) {
		t.Errorf("connect = %v, call %s", sc, lastCall(calls))
	}
	if strings.Contains(toolText(callTool(t, "team_connect_slack", `{"webhook_url":"https://hooks.slack.com/services/T/B/X"}`)), "hooks.slack.com") {
		t.Error("the webhook URL is a secret and must not be echoed back")
	}
	n := len(*calls)
	if r := callTool(t, "team_disconnect_slack", `{}`); r["isError"] != true || len(*calls) != n {
		t.Errorf("disconnect without confirm = %v", r)
	}
	if sc := callTool(t, "team_disconnect_slack", `{"confirm":true}`)["structuredContent"].(map[string]interface{}); sc["slackConnected"] != false {
		t.Errorf("disconnect = %v", sc)
	}
}

func TestMCPPersonalTools(t *testing.T) {
	calls := fakeAdminAPI(t)
	if sc := callTool(t, "my_spend", `{"period":"30d"}`)["structuredContent"].(map[string]interface{}); sc["totals"] == nil {
		t.Errorf("my_spend = %v", sc)
	}
	if !strings.Contains(lastCall(calls), "/v1/me/analytics/cost?period=30d") {
		t.Errorf("my_spend call = %s", lastCall(calls))
	}
	if sc := callTool(t, "my_trends", `{}`)["structuredContent"].(map[string]interface{}); sc["insights"] == nil {
		t.Errorf("my_trends = %v", sc)
	}
	if !strings.Contains(lastCall(calls), "/v1/me/trends?period=30d") {
		t.Errorf("my_trends default period: %s", lastCall(calls))
	}
	// The errors endpoint 404s in the fake: the summary still comes back, with the failure named.
	sc := callTool(t, "my_insights", `{"period":"24h"}`)["structuredContent"].(map[string]interface{})
	if sc["summary"] == nil || sc["tools"] == nil || sc["errors_error"] == nil {
		t.Errorf("my_insights = %v", sc)
	}
	if r := callTool(t, "my_spend", `{"period":"24h"}`); r["isError"] != true {
		t.Errorf("my_spend accepted 24h: %v", r)
	}
}

func TestTeamMembersRmNeedsYes(t *testing.T) {
	calls := fakeAdminAPI(t)
	memberYes = false
	t.Cleanup(func() { memberYes = false })
	n := len(*calls)
	err := runTeamMembersRm(teamMembersRmCmd, []string{"dev@example.com"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("rm without --yes = %v", err)
	}
	for _, c := range (*calls)[n:] {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("deleted without --yes: %s", c)
		}
	}
}

func TestTeamAlertsSlackArgs(t *testing.T) {
	t.Cleanup(func() { slackOff, slackYes = false, false })
	slackOff, slackYes = true, false
	if err := runTeamAlertsSlack(teamAlertsSlackCmd, nil); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("disconnect without --yes = %v", err)
	}
	slackOff = false
	if err := runTeamAlertsSlack(teamAlertsSlackCmd, nil); err == nil {
		t.Error("no URL and no --disconnect accepted")
	}
}

func TestWriteMySpendAndTrends(t *testing.T) {
	var buf bytes.Buffer
	_ = writeMySpend(&buf, map[string]interface{}{"period": "30d", "totals": map[string]interface{}{"usd": 12.5}, "change": 0.25,
		"byModel": []interface{}{map[string]interface{}{"model": "claude-sonnet", "usd": 12.5, "runs": float64(3)}}})
	if out := buf.String(); !strings.Contains(out, "$12.50") || !strings.Contains(out, "+25%") || !strings.Contains(out, "claude-sonnet") {
		t.Errorf("spend text:\n%s", out)
	}
	buf.Reset()
	writeMyTrends(&buf, map[string]interface{}{"period": map[string]interface{}{"days": float64(30)},
		"metrics":  map[string]interface{}{"prompts": float64(40), "plan_mode_adoption_rate": 0.1},
		"insights": []interface{}{map[string]interface{}{"title": "Use plan mode", "detail": "d", "article_url": "https://x"}}})
	if out := buf.String(); !strings.Contains(out, "30 days") || !strings.Contains(out, "10% of prompts") || !strings.Contains(out, "Use plan mode") {
		t.Errorf("trends text:\n%s", out)
	}
}
