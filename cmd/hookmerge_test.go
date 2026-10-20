package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testExe = "/opt/bin/promptconduit"

// userHookCmd is a user-owned hook command that must never be touched.
const userHookCmd = "/usr/local/bin/my-notifier --done"

func writeJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]interface{} {
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

// collectCommands returns every handler command registered on an event,
// across both matcher-group and flat-handler shapes.
func collectCommands(t *testing.T, settings map[string]interface{}, event string) []string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]interface{})
	entries, _ := hooks[event].([]interface{})
	var out []string
	for _, e := range entries {
		m, _ := e.(map[string]interface{})
		if c, ok := m["command"].(string); ok {
			out = append(out, c)
			continue
		}
		inner, _ := m["hooks"].([]interface{})
		for _, h := range inner {
			hm, _ := h.(map[string]interface{})
			if c, ok := hm["command"].(string); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func countMatching(cmds []string, substr string) int {
	n := 0
	for _, c := range cmds {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// groupedUserHook is a Claude Code / Gemini / Codex style matcher group.
func groupedUserHook(matcher string) map[string]interface{} {
	g := map[string]interface{}{
		"hooks": []interface{}{
			map[string]interface{}{"type": "command", "command": userHookCmd},
		},
	}
	if matcher != "" {
		g["matcher"] = matcher
	}
	return g
}

type sharedFileTool struct {
	name      string
	path      func(home string) string
	install   func(exe string) error
	uninstall func() error
	// event is one we register on; userEntry is a user hook placed there.
	event     string
	userEntry interface{}
	ourMarker string
}

func sharedFileTools() []sharedFileTool {
	return []sharedFileTool{
		{
			name:      "claude-code",
			path:      func(h string) string { return filepath.Join(h, ".claude", "settings.json") },
			install:   installClaudeCode,
			uninstall: uninstallClaudeCode,
			event:     "Stop",
			userEntry: groupedUserHook(""),
			ourMarker: testExe + " hook",
		},
		{
			name:      "cursor",
			path:      func(h string) string { return filepath.Join(h, ".cursor", "hooks.json") },
			install:   installCursor,
			uninstall: uninstallCursor,
			event:     "stop",
			userEntry: map[string]interface{}{"command": userHookCmd},
			ourMarker: testExe + " hook",
		},
		{
			name:      "gemini-cli",
			path:      func(h string) string { return filepath.Join(h, ".gemini", "settings.json") },
			install:   installGemini,
			uninstall: uninstallGemini,
			event:     "BeforeTool",
			userEntry: groupedUserHook("run_shell_command"),
			ourMarker: testExe + " hook",
		},
		{
			name:      "codex",
			path:      func(h string) string { return filepath.Join(h, ".codex", "hooks.json") },
			install:   installCodex,
			uninstall: uninstallCodex,
			event:     "PreToolUse",
			userEntry: groupedUserHook("Bash"),
			ourMarker: testExe + " hook --tool codex",
		},
	}
}

func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	prev := installNoExtension
	installNoExtension = true
	t.Cleanup(func() { installNoExtension = prev })
	return home
}

func TestInstall_PreservesUserHooksOnSameEvent(t *testing.T) {
	for _, tc := range sharedFileTools() {
		t.Run(tc.name, func(t *testing.T) {
			home := setupHome(t)
			path := tc.path(home)
			writeJSON(t, path, map[string]interface{}{
				"model": "keep-me",
				"hooks": map[string]interface{}{
					tc.event:        []interface{}{tc.userEntry},
					"MyCustomEvent": []interface{}{tc.userEntry},
				},
			})

			if err := tc.install(testExe); err != nil {
				t.Fatalf("install: %v", err)
			}
			got := readJSON(t, path)

			if got["model"] != "keep-me" {
				t.Errorf("unknown top-level field lost: %v", got["model"])
			}
			cmds := collectCommands(t, got, tc.event)
			if countMatching(cmds, userHookCmd) != 1 {
				t.Errorf("user hook on %s lost or duplicated: %v", tc.event, cmds)
			}
			if countMatching(cmds, tc.ourMarker) != 1 {
				t.Errorf("our hook missing on %s: %v", tc.event, cmds)
			}
			if c := collectCommands(t, got, "MyCustomEvent"); len(c) != 1 || c[0] != userHookCmd {
				t.Errorf("unrelated user event changed: %v", c)
			}
		})
	}
}

func TestInstall_Idempotent(t *testing.T) {
	for _, tc := range sharedFileTools() {
		t.Run(tc.name, func(t *testing.T) {
			home := setupHome(t)
			path := tc.path(home)
			writeJSON(t, path, map[string]interface{}{
				"hooks": map[string]interface{}{tc.event: []interface{}{tc.userEntry}},
			})

			for i := 0; i < 3; i++ {
				if err := tc.install(testExe); err != nil {
					t.Fatalf("install #%d: %v", i+1, err)
				}
			}
			got := readJSON(t, path)
			hooks := got["hooks"].(map[string]interface{})
			for event := range hooks {
				cmds := collectCommands(t, got, event)
				if n := countMatching(cmds, "promptconduit"); n != 1 {
					t.Errorf("event %s has %d PromptConduit handlers after re-install, want 1: %v", event, n, cmds)
				}
			}
			if n := countMatching(collectCommands(t, got, tc.event), userHookCmd); n != 1 {
				t.Errorf("user hook count on %s = %d, want 1", tc.event, n)
			}
		})
	}
}

func TestUninstall_KeepsUserHooks(t *testing.T) {
	for _, tc := range sharedFileTools() {
		t.Run(tc.name, func(t *testing.T) {
			home := setupHome(t)
			path := tc.path(home)
			writeJSON(t, path, map[string]interface{}{
				"model": "keep-me",
				"hooks": map[string]interface{}{tc.event: []interface{}{tc.userEntry}},
			})
			if err := tc.install(testExe); err != nil {
				t.Fatalf("install: %v", err)
			}
			if err := tc.uninstall(); err != nil {
				t.Fatalf("uninstall: %v", err)
			}

			got := readJSON(t, path)
			if got["model"] != "keep-me" {
				t.Errorf("unknown top-level field lost: %v", got["model"])
			}
			hooks, _ := got["hooks"].(map[string]interface{})
			if len(hooks) != 1 {
				t.Errorf("expected only the user's event to remain, got %v", hooks)
			}
			cmds := collectCommands(t, got, tc.event)
			if len(cmds) != 1 || cmds[0] != userHookCmd {
				t.Errorf("user hook on %s not preserved: %v", tc.event, cmds)
			}
		})
	}
}

func TestUninstall_RemovesHooksKeyWhenOnlyOurs(t *testing.T) {
	home := setupHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	writeJSON(t, path, map[string]interface{}{"model": "keep-me"})
	if err := installClaudeCode(testExe); err != nil {
		t.Fatal(err)
	}
	if err := uninstallClaudeCode(); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, path)
	if _, ok := got["hooks"]; ok {
		t.Errorf("hooks key should be removed when only PromptConduit hooks existed: %v", got["hooks"])
	}
	if got["model"] != "keep-me" {
		t.Errorf("unknown field lost")
	}
}

func TestStripPromptConduitHooks_SharedMatcherGroup(t *testing.T) {
	// A single matcher group holding both a user handler and ours: only ours
	// is removed, the group (and its matcher) stays.
	hooks := map[string]interface{}{
		"PreToolUse": []interface{}{
			map[string]interface{}{
				"matcher": "Bash",
				"hooks": []interface{}{
					map[string]interface{}{"type": "command", "command": userHookCmd},
					map[string]interface{}{"type": "command", "command": testExe + " hook"},
				},
			},
		},
		"WorktreeCreate": []interface{}{ // stale event we no longer ship
			map[string]interface{}{"hooks": []interface{}{
				map[string]interface{}{"type": "command", "command": testExe + " hook"},
			}},
		},
		"Weird": "not-an-array", // unknown shape is left alone
	}
	if n := stripPromptConduitHooks(hooks); n != 2 {
		t.Errorf("removed = %d, want 2", n)
	}
	if _, ok := hooks["WorktreeCreate"]; ok {
		t.Error("stale PromptConduit-only event should be deleted")
	}
	if hooks["Weird"] != "not-an-array" {
		t.Error("unknown-shape entry should be untouched")
	}
	group := hooks["PreToolUse"].([]interface{})[0].(map[string]interface{})
	if group["matcher"] != "Bash" {
		t.Error("matcher lost")
	}
	inner := group["hooks"].([]interface{})
	if len(inner) != 1 || inner[0].(map[string]interface{})["command"] != userHookCmd {
		t.Errorf("user handler not preserved: %v", inner)
	}
}
