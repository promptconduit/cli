package cmd

import (
	"os"
	"testing"
)

func TestDetectTool(t *testing.T) {
	tests := []struct {
		name  string
		event map[string]interface{}
		want  string
	}{
		{
			name:  "claude code",
			event: map[string]interface{}{"hook_event_name": "PostToolUse", "session_id": "abc"},
			want:  "claude-code",
		},
		{
			name: "cursor with hook_event_name",
			event: map[string]interface{}{
				"hook_event_name": "postToolUse",
				"cursor_version":  "1.0",
				"conversation_id": "conv-1",
				"generation_id":   "gen-1",
				"model":           "composer-2.5",
				"workspace_roots": []interface{}{"/tmp/proj"},
			},
			want: "cursor",
		},
		{
			name:  "gemini",
			event: map[string]interface{}{"gemini_session": "sess", "event": "foo"},
			want:  "gemini-cli",
		},
		{
			name:  "unknown generic event",
			event: map[string]interface{}{"event": "something"},
			want:  "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectTool(tt.event); got != tt.want {
				t.Errorf("detectTool() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		event map[string]interface{}
		want  string
	}{
		{
			name:  "claude code cwd",
			event: map[string]interface{}{"cwd": "/tmp/tumbling-timmy"},
			want:  "/tmp/tumbling-timmy",
		},
		{
			name:  "empty cwd falls through to workspace_roots",
			event: map[string]interface{}{"cwd": "", "workspace_roots": []interface{}{"/tmp/tumbling-timmy"}},
			want:  "/tmp/tumbling-timmy",
		},
		{
			name:  "cursor workspace_dir",
			event: map[string]interface{}{"workspace_dir": "/tmp/from-dir"},
			want:  "/tmp/from-dir",
		},
		{
			name: "cursor workspace_roots (not hook process cwd)",
			event: map[string]interface{}{
				"cursor_version":  "1.0",
				"workspace_roots": []interface{}{"/tmp/tumbling-timmy", "/tmp/extra"},
			},
			want: "/tmp/tumbling-timmy",
		},
		{
			name:  "no fields → process cwd",
			event: map[string]interface{}{"hook_event_name": "stop"},
			want:  wd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getWorkingDirectory(tt.event); got != tt.want {
				t.Errorf("getWorkingDirectory() = %q, want %q", got, tt.want)
			}
		})
	}
}
