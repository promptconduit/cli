package loop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envLine(session, prompt, hook, at, enrich string) string {
	return fmt.Sprintf(`{"schema":2,"session_id":%q,"prompt_id":%q,"tool":"claude-code","hook_event":%q,"captured_at":%q,"raw_event":{},"enrichments":{%s}}`,
		session, prompt, hook, at, enrich)
}

func costReq(id, ts, tools string) string {
	t := ""
	if tools != "" {
		t = fmt.Sprintf(`,"tools":{"total":1,"by_name":{%q:1}}`, tools)
	}
	return fmt.Sprintf(`{"request_id":%q,"model":"claude-opus-5-5","ts":%q,"tokens":{"input":2,"output":100,"cache_read":200000,"cache_write":500},"usd":{"input":0.000008,"output":0.002,"cache_read":0.04,"cache_write":0.004,"total":0.046008}%s}`, id, ts, t)
}

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFromEventLog_LatestPromptReattributed(t *testing.T) {
	log := writeLog(t,
		envLine("s-old", "o1", "UserPromptSubmit", "2026-11-08T09:00:00Z", `"prompt":{"count":1,"task_kind":"build"}`),
		envLine("s1", "p1", "UserPromptSubmit", "2026-11-08T10:00:00Z", `"prompt":{"count":1,"task_kind":"debug"}`),
		// p1's Stop priced only its first call (flush race)…
		envLine("s1", "p1", "Stop", "2026-11-08T10:01:00Z", `"cost":{"requests":[`+costReq("r1", "2026-11-08T10:00:05.100Z", "Bash")+`]}`),
		envLine("s1", "p1", "PostToolUse", "2026-11-08T10:00:20Z", `"tools":{"total":1,"failed":0,"calls":[{"name":"Bash","ok":true,"duration_ms":3000}]}`),
		// The batch repeats the same call: not counted twice.
		envLine("s1", "p1", "PostToolBatch", "2026-11-08T10:00:20Z", `"tools":{"total":1,"failed":0,"calls":[{"name":"Bash","ok":true}]}`),
		envLine("s1", "p2", "UserPromptSubmit", "2026-11-08T10:05:00Z", `"prompt":{"count":2,"task_kind":"ship"}`),
		// …so p2's Stop carries p1's final call (r2) plus its own.
		envLine("s1", "p2", "Stop", "2026-11-08T10:06:00Z", `"cost":{"requests":[`+
			costReq("r2", "2026-11-08T10:00:59.500Z", "")+","+
			costReq("r3", "2026-11-08T10:05:03.000Z", "Read")+","+
			costReq("r4", "2026-11-08T10:05:30.000Z", "")+`]}`),
	)

	res, err := FromEventLogFile(log, "", "", table(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "s1" || res.Ledger.PromptID != "p2" || res.Ledger.TaskKind != "ship" {
		t.Fatalf("picked %s/%s (%s), want s1/p2 ship", res.SessionID, res.Ledger.PromptID, res.Ledger.TaskKind)
	}
	if res.Ledger.RoundTrips != 2 || res.Ledger.Calls[0].RequestID != "r3" {
		t.Fatalf("p2 calls = %+v, want r3,r4 (r2 re-attributed to p1)", res.Ledger.Calls)
	}
	if res.Opening == nil || res.Opening.FirstCallContext != 200502 {
		t.Errorf("opening = %+v", res.Opening)
	}
	if res.Prompts != 2 {
		t.Errorf("sessionPrompts = %d", res.Prompts)
	}

	res, err = FromEventLogFile(log, "", "p1", table(t))
	if err != nil {
		t.Fatal(err)
	}
	l := res.Ledger
	if l.RoundTrips != 2 || l.Calls[1].RequestID != "r2" {
		t.Fatalf("p1 calls = %+v, want r1,r2", l.Calls)
	}
	if tl := l.Calls[0].Tools; len(tl) != 1 || tl[0].OK == nil || !*tl[0].OK || tl[0].DurationMs != 3000 {
		t.Errorf("r1 tools = %+v, want Bash ok 3000ms from the PostToolUse event", tl)
	}
	if l.EndedAt == "" || l.StartedAt == "" {
		t.Errorf("startedAt/endedAt missing: %+v", l)
	}
}

func TestFromEventLog_NotFound(t *testing.T) {
	var nf ErrNotFound
	if _, err := FromEventLogFile(filepath.Join(t.TempDir(), "missing.jsonl"), "", "", table(t)); !errors.As(err, &nf) {
		t.Errorf("missing log: err = %v, want ErrNotFound", err)
	}
	log := writeLog(t, envLine("s1", "p1", "UserPromptSubmit", "2026-11-08T10:00:00Z", `"prompt":{"count":1}`))
	if _, err := FromEventLogFile(log, "", "", table(t)); !errors.As(err, &nf) || !strings.Contains(err.Error(), "no priced calls") {
		t.Errorf("open prompt: err = %v", err)
	}
	if _, err := FromEventLogFile(log, "", "nope", table(t)); !errors.As(err, &nf) {
		t.Errorf("unknown prompt: err = %v", err)
	}
}
