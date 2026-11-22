package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/logger"
	"github.com/promptconduit/cli/internal/transcriptcheck"
)

func TestCheckTranscriptLogsOncePerSession(t *testing.T) {
	isolatedHome(t)
	logger.SetDirForTest(t.TempDir())
	t.Cleanup(func() { logger.SetDirForTest("") })
	base := client.ConfigDir()
	absent := filepath.Join(t.TempDir(), "gone.jsonl")

	checkTranscript(base, "sess-1", absent, "/repo", time.Now())
	checkTranscript(base, "sess-1", absent, "/repo", time.Now())

	data, _ := os.ReadFile(logger.Path())
	if n := strings.Count(string(data), "transcript saving is off for session sess-1"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, data)
	}
	if got := transcriptcheck.List(base, time.Now()); len(got) != 1 || got[0].SessionID != "sess-1" {
		t.Errorf("recorded = %+v", got)
	}
}

func TestSetupStatusWarnsAboutMissingTranscripts(t *testing.T) {
	isolatedHome(t)
	st := callTool(t, "setup_status", `{}`)["structuredContent"].(map[string]interface{})
	if _, ok := st["warnings"]; ok {
		t.Fatalf("warnings present with nothing recorded: %v", st)
	}

	transcriptcheck.Observe(client.ConfigDir(), "0fb10889", "", "/repo/timmy", time.Now())
	st = callTool(t, "setup_status", `{}`)["structuredContent"].(map[string]interface{})
	warnings, _ := st["warnings"].([]interface{})
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "transcript saving is off for session 0fb10889") ||
		!strings.Contains(warnings[0].(string), "CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1") {
		t.Errorf("warnings = %v", st["warnings"])
	}
	sessions, _ := st["sessions_without_transcript"].([]interface{})
	if len(sessions) != 1 || sessions[0].(map[string]interface{})["cwd"] != "/repo/timmy" {
		t.Errorf("sessions_without_transcript = %v", st["sessions_without_transcript"])
	}
}

func TestPrintMissingTranscripts(t *testing.T) {
	var buf bytes.Buffer
	printMissingTranscripts(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("printed with no entries: %q", buf.String())
	}
	printMissingTranscripts(&buf, []transcriptcheck.Entry{{SessionID: "abc", Cwd: "/r", FirstSeen: time.Now()}})
	out := buf.String()
	for _, want := range []string{"1 Claude Code session(s)", "abc in /r", "CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
