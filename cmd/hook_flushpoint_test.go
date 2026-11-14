package cmd

import "testing"

func TestIsFlushPointEvent(t *testing.T) {
	for _, ev := range []string{"Stop", "StopFailure", "SessionEnd", "stop", "sessionEnd", "session_end"} {
		if !isFlushPointEvent(ev) {
			t.Errorf("%q should flush the spool", ev)
		}
	}
	for _, ev := range []string{"PreToolUse", "PostToolUse", "UserPromptSubmit", "SubagentStop", "afterFileEdit", ""} {
		if isFlushPointEvent(ev) {
			t.Errorf("%q should not flush the spool", ev)
		}
	}
}
