package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// isolatedHome points HOME and the config dir at a temp dir and clears any
// API settings from the environment.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PROMPTCONDUIT_API_KEY", "")
	t.Setenv("PROMPTCONDUIT_API_URL", "")
	t.Setenv("PROMPTCONDUIT_ORG", "")
	return home
}

// fakeTeamAPI serves the team endpoints the MCP tools call.
func fakeTeamAPI(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/organizations":
			_, _ = io.WriteString(w, `{"organizations":[{"id":"o1","name":"Acme Eng","slug":"acme","role":"owner","plan":"team"}]}`)
		case "/v1/organizations/o1/analytics/cost":
			rows := make([]string, 15)
			for i := range rows {
				rows[i] = `{"repoName":"r` + string(rune('a'+i)) + `","usd":1,"sessions":1}`
			}
			_, _ = io.WriteString(w, `{"period":"7d","totals":{"usd":15},"byRepo":[`+strings.Join(rows, ",")+`],
				"byDay":[{"day":"2026-10-01","usd":1},{"day":"2026-10-02","usd":2}],"byMember":[]}`)
		case "/v1/organizations/o1/members":
			_, _ = io.WriteString(w, `{"members":[{"user":{"id":"u9","email":"Dev@Example.com"}}]}`)
		case "/v1/organizations/o1/budgets":
			_, _ = io.WriteString(w, `{"budgets":[],"canManage":true}`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"detail":"not found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// callTool runs one tools/call through the real server and returns the result.
func callTool(t *testing.T, name string, args string) map[string]interface{} {
	t.Helper()
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}` + "\n"
	pr, pw := io.Pipe()
	go func() {
		_ = buildMCPServer().Serve(context.Background(), strings.NewReader(req), pw)
		_ = pw.Close()
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	if !sc.Scan() {
		t.Fatalf("no reply for %s", name)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	r, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: %v", name, resp)
	}
	return r
}

func toolText(r map[string]interface{}) string {
	return r["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
}

func TestMCPToolDefinitions(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z][a-z_]*$`)
	writes := map[string]bool{"team_set_budget": true, "team_remove_budget": true, "team_tag_repo": true,
		"team_invite": true, "team_create": true, "team_join": true}
	tools := buildMCPServer().Tools()
	if len(tools) != 15 {
		t.Errorf("tool count = %d", len(tools))
	}
	for _, tool := range tools {
		if !snake.MatchString(tool.Name) || tool.Description == "" || tool.Handler == nil || tool.Annotations == nil {
			t.Errorf("%s: incomplete definition", tool.Name)
			continue
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s: schema type = %v", tool.Name, tool.InputSchema["type"])
		}
		if _, err := json.Marshal(tool.InputSchema); err != nil {
			t.Errorf("%s: schema not JSON: %v", tool.Name, err)
		}
		if tool.Annotations.ReadOnlyHint == writes[tool.Name] {
			t.Errorf("%s: readOnlyHint = %v", tool.Name, tool.Annotations.ReadOnlyHint)
		}
		if tool.Annotations.DestructiveHint != (tool.Name == "team_remove_budget") {
			t.Errorf("%s: destructiveHint = %v", tool.Name, tool.Annotations.DestructiveHint)
		}
	}
}

func TestMCPLocalToolsWorkWithoutAccount(t *testing.T) {
	isolatedHome(t)
	r := callTool(t, "cost_history", `{"days":3}`)
	sc := r["structuredContent"].(map[string]interface{})
	if r["isError"] == true || sc["window_days"] != float64(3) || sc["total_usd"] != float64(0) {
		t.Errorf("cost_history = %v", r)
	}
	if r := callTool(t, "recent_sessions", `{}`); r["isError"] == true {
		t.Errorf("recent_sessions = %v", r)
	}
	st := callTool(t, "setup_status", `{}`)["structuredContent"].(map[string]interface{})
	if st["signed_in"] != false || !strings.Contains(st["next_step"].(string), "promptconduit login") {
		t.Errorf("setup_status = %v", st)
	}
	if r := callTool(t, "cost_history", `{"days":500}`); r["isError"] != true {
		t.Errorf("out-of-range days accepted: %v", r)
	}
	if r := callTool(t, "cost_history", `{"bogus":1}`); r["isError"] != true || !strings.Contains(toolText(r), "invalid arguments") {
		t.Errorf("unknown arg accepted: %v", r)
	}
}

func TestMCPTeamToolsNeedLogin(t *testing.T) {
	isolatedHome(t)
	r := callTool(t, "team_spend", `{}`)
	if r["isError"] != true || !strings.Contains(toolText(r), "promptconduit login") {
		t.Errorf("team_spend signed out = %v", r)
	}
}

func TestMCPTeamSpendTrimsAndTagsOrg(t *testing.T) {
	isolatedHome(t)
	srv, calls := fakeTeamAPI(t)
	t.Setenv("PROMPTCONDUIT_API_URL", srv.URL)
	t.Setenv("PROMPTCONDUIT_API_KEY", "k")

	sc := callTool(t, "team_spend", `{"period":"7d","limit":10}`)["structuredContent"].(map[string]interface{})
	if org := sc["organization"].(map[string]interface{}); org["slug"] != "acme" {
		t.Errorf("organization = %v", org)
	}
	if rows := sc["byRepo"].([]interface{}); len(rows) != 10 {
		t.Errorf("byRepo rows = %d", len(rows))
	}
	if om := sc["omittedRows"].(map[string]interface{}); om["byRepo"] != float64(5) {
		t.Errorf("omittedRows = %v", om)
	}
	if days := sc["byDay"].([]interface{}); days[0].(map[string]interface{})["day"] != "2026-10-02" {
		t.Errorf("byDay not newest first: %v", days)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "/analytics/cost?period=7d") {
		t.Errorf("calls = %v", *calls)
	}

	only := callTool(t, "team_spend", `{"breakdown":"day"}`)["structuredContent"].(map[string]interface{})
	if _, ok := only["byRepo"]; ok {
		t.Errorf("breakdown=day kept byRepo: %v", only)
	}
	if r := callTool(t, "team_spend", `{"period":"1y"}`); r["isError"] != true {
		t.Errorf("bad period accepted: %v", r)
	}
}

func TestMCPSetMemberBudgetResolvesEmail(t *testing.T) {
	isolatedHome(t)
	srv, calls := fakeTeamAPI(t)
	t.Setenv("PROMPTCONDUIT_API_URL", srv.URL)
	t.Setenv("PROMPTCONDUIT_API_KEY", "k")

	if r := callTool(t, "team_set_budget", `{"scope":"member","target":"dev@example.com","monthly_usd":150}`); r["isError"] == true {
		t.Fatalf("set budget = %v", r)
	}
	last := (*calls)[len(*calls)-1]
	if !strings.HasPrefix(last, "POST /v1/organizations/o1/budgets") || !strings.Contains(last, `"scopeValue":"u9"`) {
		t.Errorf("budget call = %s", last)
	}
	if r := callTool(t, "team_set_budget", `{"scope":"repo","monthly_usd":10}`); r["isError"] != true {
		t.Errorf("repo budget without target accepted: %v", r)
	}
}

func TestMCPPromptsRender(t *testing.T) {
	for _, p := range mcpPrompts() {
		if text := p.Render(map[string]string{}); !strings.Contains(text, "PromptConduit") {
			t.Errorf("%s prompt doesn't point at the tools: %q", p.Name, text)
		}
	}
}

// ---- registration ----

func readJSONFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMCPRegisterJSONTargetsMerge(t *testing.T) {
	home := isolatedHome(t)
	gemini, _ := mcpTargetFor("gemini-cli")
	path := gemini.configFile(home)
	writeFile(t, path, `{"theme":"dark","hooks":{"x":1},"mcpServers":{"other":{"command":"o"}}}`)

	if err := gemini.register(home, "/bin/pc"); err != nil {
		t.Fatal(err)
	}
	m := readJSONFile(t, path)
	servers := m["mcpServers"].(map[string]interface{})
	ours := servers["promptconduit"].(map[string]interface{})
	if m["theme"] != "dark" || m["hooks"] == nil || servers["other"] == nil || ours["command"] != "/bin/pc" || ours["args"].([]interface{})[0] != "mcp" {
		t.Errorf("merged = %v", m)
	}
	if !gemini.registered(home) {
		t.Error("registered = false after register")
	}

	if removed, err := gemini.unregister(home); err != nil || !removed {
		t.Fatalf("unregister = %v, %v", removed, err)
	}
	m = readJSONFile(t, path)
	if servers := m["mcpServers"].(map[string]interface{}); servers["promptconduit"] != nil || servers["other"] == nil {
		t.Errorf("after unregister = %v", m)
	}

	cursor, _ := mcpTargetFor("cursor")
	if err := cursor.register(home, "/bin/pc"); err != nil {
		t.Fatal(err)
	}
	if _, err := cursor.unregister(home); err != nil {
		t.Fatal(err)
	}
	if m := readJSONFile(t, cursor.configFile(home)); m["mcpServers"] != nil {
		t.Errorf("empty mcpServers should be dropped: %v", m)
	}
	if removed, _ := cursor.unregister(home); removed {
		t.Error("second unregister reported removed")
	}
}

func fakeClaudeCLI(t *testing.T) *[][]string {
	t.Helper()
	var got [][]string
	orig := runClaudeCLI
	runClaudeCLI = func(args ...string) error {
		got = append(got, args)
		return nil
	}
	t.Cleanup(func() { runClaudeCLI = orig })
	return &got
}

func TestMCPRegisterClaudeCodeUsesCLI(t *testing.T) {
	home := isolatedHome(t)
	calls := fakeClaudeCLI(t)
	cc, _ := mcpTargetFor("claude-code")
	if err := cc.register(home, "/bin/pc"); err != nil {
		t.Fatal(err)
	}
	// Not registered yet: just add, no remove.
	if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "mcp add --scope user promptconduit -- /bin/pc mcp" {
		t.Errorf("claude calls = %v", *calls)
	}
	if _, err := os.Stat(cc.configFile(home)); err == nil {
		t.Error("~/.claude.json must not be written directly")
	}

	// Up to date: a re-install touches nothing, so a working entry is never removed.
	writeFile(t, cc.configFile(home), `{"mcpServers":{"promptconduit":{"type":"stdio","command":"/bin/pc","args":["mcp"]}}}`)
	*calls = nil
	if err := cc.register(home, "/bin/pc"); err != nil || len(*calls) != 0 {
		t.Errorf("up-to-date register ran %v (%v)", *calls, err)
	}
	// Stale (binary moved): remove, then add.
	if err := cc.register(home, "/new/pc"); err != nil || len(*calls) != 2 || (*calls)[0][1] != "remove" {
		t.Errorf("stale register ran %v (%v)", *calls, err)
	}
	if !cc.registered(home) {
		t.Error("registered should read ~/.claude.json")
	}
	if removed, err := cc.unregister(home); err != nil || !removed {
		t.Errorf("unregister = %v, %v", removed, err)
	}
	if last := (*calls)[len(*calls)-1]; strings.Join(last, " ") != "mcp remove --scope user promptconduit" {
		t.Errorf("remove call = %v", last)
	}
}

func TestReconcileMCPRegistersWhereHooksAreInstalled(t *testing.T) {
	home := isolatedHome(t)
	claudeCalls := fakeClaudeCLI(t)
	cursor, _ := mcpTargetFor("cursor")
	writeFile(t, cursor.hooksFile(home), `{"hooks":{"stop":[{"command":"/usr/local/bin/promptconduit hook"}]}}`)

	var notices []string
	notice := func(f string, a ...interface{}) { notices = append(notices, f) }

	reconcileMCP(notice)
	if !cursor.registered(home) || len(notices) != 1 || len(*claudeCalls) != 0 {
		t.Fatalf("registered=%v notices=%d claude=%v", cursor.registered(home), len(notices), *claudeCalls)
	}
	reconcileMCP(notice)
	if len(notices) != 1 {
		t.Error("second reconcile should be a no-op")
	}

	// An explicit opt-out sticks.
	if _, err := cursor.unregister(home); err != nil {
		t.Fatal(err)
	}
	if err := saveMCPState(mcpState{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	reconcileMCP(notice)
	if cursor.registered(home) {
		t.Error("reconcile ignored the opt-out")
	}
}

func TestPerToolOptOut(t *testing.T) {
	home := isolatedHome(t)
	fakeClaudeCLI(t)
	cursor, _ := mcpTargetFor("cursor")
	gemini, _ := mcpTargetFor("gemini-cli")
	writeFile(t, cursor.hooksFile(home), `{"hooks":{"stop":[{"command":"/x/promptconduit hook"}]}}`)
	writeFile(t, gemini.hooksFile(home), `{"hooks":{"Stop":[{"command":"/x/promptconduit hook"}]}}`)
	if err := cursor.register(home, "/x/promptconduit"); err != nil {
		t.Fatal(err)
	}

	// install cursor --no-mcp: removes Cursor's entry and opts out only Cursor.
	registerMCPForInstall("cursor", "/x/promptconduit", true)
	if cursor.registered(home) {
		t.Error("--no-mcp left the existing entry")
	}
	reconcileMCP(func(string, ...interface{}) {})
	if cursor.registered(home) || !gemini.registered(home) {
		t.Errorf("reconcile: cursor=%v (want false) gemini=%v (want true)", cursor.registered(home), gemini.registered(home))
	}

	// Registering again lifts the opt-out.
	registerMCPForInstall("cursor", "/x/promptconduit", false)
	if s := loadMCPState(); s.disabledFor("cursor") {
		t.Errorf("state after re-register = %+v", s)
	}
}

func TestHooksInstalledIsPrecise(t *testing.T) {
	home := isolatedHome(t)
	cc, _ := mcpTargetFor("claude-code")
	writeFile(t, cc.hooksFile(home), `{"permissions":{"allow":["Bash(promptconduit status)"]},"hooks":{"Stop":[{"command":"other hook"}]}}`)
	if cc.hooksInstalled(home) {
		t.Error("unrelated mentions counted as installed hooks")
	}
	writeFile(t, cc.hooksFile(home), `{"hooks":{"Stop":[{"command":"/usr/local/bin/promptconduit hook"}]}}`)
	if !cc.hooksInstalled(home) {
		t.Error("real hook not detected")
	}
}

func TestClaudeMCPAddCommandQuotesPath(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/promptconduit": "/usr/local/bin/promptconduit",
		"/Users/x/App Support/pc":      "'/Users/x/App Support/pc'",
		"/tmp/it's":                    `'/tmp/it'\''s'`,
	} {
		if got := claudeMCPAddCommand(in); got != "claude mcp add --scope user promptconduit -- "+want+" mcp" {
			t.Errorf("claudeMCPAddCommand(%q) = %q", in, got)
		}
	}
}

func TestMCPTeamSpendRejectsBadLimit(t *testing.T) {
	isolatedHome(t)
	for _, args := range []string{`{"limit":-1}`, `{"limit":501}`} {
		if r := callTool(t, "team_spend", args); r["isError"] != true || !strings.Contains(toolText(r), "limit") {
			t.Errorf("%s: %v", args, r)
		}
	}
}

func TestWriteMCPStatusOnlySuggestsToolsWithHooks(t *testing.T) {
	home := isolatedHome(t)
	cc, _ := mcpTargetFor("claude-code")
	cursor, _ := mcpTargetFor("cursor")
	writeFile(t, cc.configFile(home), `{"mcpServers":{"promptconduit":{"command":"/x/promptconduit","args":["mcp"]}}}`)
	writeFile(t, cursor.hooksFile(home), `{"hooks":{"stop":[{"command":"/x/promptconduit hook"}]}}`)

	var buf bytes.Buffer
	writeMCPStatus(&buf, home, mcpState{})
	out := buf.String()
	for _, want := range []string{
		"Claude Code: Registered",
		"Cursor:      Not registered\n",
		"Gemini CLI:  Not registered (no PromptConduit hooks)",
		"Register with: promptconduit mcp install cursor\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Gemini not installed and Cursor opted out: nothing to suggest.
	buf.Reset()
	writeMCPStatus(&buf, home, mcpState{DisabledTools: []string{"cursor"}})
	if out := buf.String(); strings.Contains(out, "Register with") || !strings.Contains(out, "Cursor:      Not registered (turned off)") {
		t.Errorf("opted out:\n%s", out)
	}
}

func TestBuildMCPStatusJSON(t *testing.T) {
	home := isolatedHome(t)
	cursor, _ := mcpTargetFor("cursor")
	writeFile(t, cursor.hooksFile(home), `{"hooks":{"stop":[{"command":"/x/promptconduit hook"}]}}`)

	r := buildMCPStatus(home, mcpState{})
	if len(r.Tools) != len(mcpTargets) || len(r.Suggest) != 1 || r.Suggest[0] != "cursor" {
		t.Fatalf("report = %+v", r)
	}
	data, _ := json.Marshal(r)
	if !strings.Contains(string(data), `{"tool":"cursor","label":"Cursor","registered":false,"hooks_installed":true,"turned_off":false}`) {
		t.Errorf("json shape changed: %s", data)
	}

	writeFile(t, cursor.configFile(home), `{"mcpServers":{"promptconduit":{"command":"/x/promptconduit","args":["mcp"]}}}`)
	r = buildMCPStatus(home, mcpState{})
	if r.Suggest == nil || len(r.Suggest) != 0 {
		t.Errorf("registered → no suggestion (and [] not null), got %#v", r.Suggest)
	}
}

func TestMCPInstallToolsSelection(t *testing.T) {
	home := isolatedHome(t)
	all, _ := mcpInstallTools(nil, home)
	if len(all) != len(mcpTargets) {
		t.Errorf("no hooks installed → all targets, got %d", len(all))
	}
	gemini, _ := mcpTargetFor("gemini-cli")
	writeFile(t, gemini.hooksFile(home), `{"hooks":{"x":[{"command":"promptconduit hook"}]}}`)
	got, _ := mcpInstallTools(nil, home)
	if len(got) != 1 || got[0].Tool != "gemini-cli" {
		t.Errorf("default selection = %v", got)
	}
	if got, err := mcpInstallTools([]string{"gemini"}, home); err != nil || got[0].Tool != "gemini-cli" {
		t.Errorf("alias = %v, %v", got, err)
	}
	if _, err := mcpInstallTools([]string{"codex"}, home); err == nil {
		t.Error("codex should be rejected")
	}
}
