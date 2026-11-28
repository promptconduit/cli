package cmd

import (
	"strings"
	"testing"

	"github.com/promptconduit/cli/internal/cost"
)

func TestMCPTeamCI(t *testing.T) {
	isolatedHome(t)
	srv, calls := fakeTeamAPI(t)
	t.Setenv("PROMPTCONDUIT_API_URL", srv.URL)
	t.Setenv("PROMPTCONDUIT_API_KEY", "k")
	sc := callTool(t, "team_ci", `{"period":"30d"}`)["structuredContent"].(map[string]interface{})
	if sc["usd"] != 22.61 || sc["organization"] == nil {
		t.Errorf("team_ci = %v", sc)
	}
	if !strings.Contains(lastCall(calls), "/v1/organizations/o1/ci?period=30d") {
		t.Errorf("team_ci call = %s", lastCall(calls))
	}
	if r := callTool(t, "team_ci", `{"period":"1y"}`); r["isError"] != true {
		t.Errorf("bad period accepted: %v", r)
	}
}

func TestSessionCostCI(t *testing.T) {
	s := cost.SessionSummary{SessionID: "s-1", Totals: cost.SessionTotal{CostTotal: 9.53}}

	isolatedHome(t)
	// Signed out: token cost only, no CI fields.
	out := withCI(map[string]interface{}{}, s)
	if out["token_usd"] != 9.53 || out["ci_usd"] != nil {
		t.Errorf("signed out = %v", out)
	}

	// Signed in: ci_usd from the service, next to token_usd.
	srv, _ := fakeTeamAPI(t)
	t.Setenv("PROMPTCONDUIT_API_URL", srv.URL)
	t.Setenv("PROMPTCONDUIT_API_KEY", "k")
	out = withCI(map[string]interface{}{}, s)
	if out["ci_usd"] != 4.49 || out["ci_minutes"] != 748.0 || out["total_usd"] != 9.53+4.49 {
		t.Errorf("signed in = %v", out)
	}
	// Unknown session: the lookup fails quietly.
	out = withCI(map[string]interface{}{}, cost.SessionSummary{SessionID: "nope"})
	if _, ok := out["ci_usd"]; ok {
		t.Errorf("unknown session = %v", out)
	}
}
