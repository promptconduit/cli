package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	intsync "github.com/promptconduit/cli/internal/sync"
)

// Registering `promptconduit mcp` with the agents that speak MCP, so the
// cost and team tools show up in every session without any setup.
//
//   - Claude Code: `claude mcp add --scope user` (Claude Code owns
//     ~/.claude.json and rewrites it constantly, so we never edit it directly;
//     we only read it to see whether we're registered).
//   - Cursor:     ~/.cursor/mcp.json        mcpServers.promptconduit
//   - Gemini CLI: ~/.gemini/settings.json   mcpServers.promptconduit
//
// Every write merges: other servers and settings are left untouched.

const mcpServerName = "promptconduit"

// mcpTarget is one agent we can register the MCP server with.
type mcpTarget struct {
	Tool  string // install name: claude-code, cursor, gemini-cli
	Label string
	// hooksFile is where install writes this tool's hooks; reconcile only
	// registers MCP for tools whose hooks are installed.
	hooksFile func(home string) string
	// configFile is the JSON file holding mcpServers (read-only for Claude Code).
	configFile func(home string) string
	// viaClaudeCLI routes writes through the `claude` CLI instead of the file.
	viaClaudeCLI bool
}

var mcpTargets = []mcpTarget{
	{
		Tool: "claude-code", Label: "Claude Code", viaClaudeCLI: true,
		hooksFile:  func(h string) string { return filepath.Join(h, ".claude", "settings.json") },
		configFile: func(h string) string { return filepath.Join(h, ".claude.json") },
	},
	{
		Tool: "cursor", Label: "Cursor",
		hooksFile:  func(h string) string { return filepath.Join(h, ".cursor", "hooks.json") },
		configFile: func(h string) string { return filepath.Join(h, ".cursor", "mcp.json") },
	},
	{
		Tool: "gemini-cli", Label: "Gemini CLI",
		hooksFile:  func(h string) string { return filepath.Join(h, ".gemini", "settings.json") },
		configFile: func(h string) string { return filepath.Join(h, ".gemini", "settings.json") },
	},
}

func mcpTargetFor(tool string) (mcpTarget, bool) {
	for _, t := range mcpTargets {
		if t.Tool == tool {
			return t, true
		}
	}
	return mcpTarget{}, false
}

// runClaudeCLI runs the `claude` CLI; a var so tests can fake it.
var runClaudeCLI = func(args ...string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return errNoClaudeCLI
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, bin, args...)
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("claude %s: %s", strings.Join(args, " "), msg)
		}
		return fmt.Errorf("claude %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

var errNoClaudeCLI = errors.New("the `claude` command isn't on your PATH")

// claudeMCPAddCommand is the manual fallback when `claude` isn't on PATH.
func claudeMCPAddCommand(exe string) string {
	return fmt.Sprintf("claude mcp add --scope user %s -- %s mcp", mcpServerName, shellQuote(exe))
}

// entry returns our server entry from the target's config, if any.
func (t mcpTarget) entry(home string) (map[string]interface{}, bool) {
	servers, _, err := readMCPServers(t.configFile(home))
	if err != nil {
		return nil, false
	}
	e, ok := servers[mcpServerName].(map[string]interface{})
	return e, ok
}

// registered reports whether our server entry exists in the target's config.
func (t mcpTarget) registered(home string) bool {
	_, ok := t.entry(home)
	return ok
}

// upToDate reports whether our entry already runs `<exe> mcp`.
func (t mcpTarget) upToDate(home, exe string) bool {
	e, ok := t.entry(home)
	if !ok || e["command"] != exe {
		return false
	}
	args, _ := e["args"].([]interface{})
	return len(args) == 1 && args[0] == "mcp"
}

// hooksInstalled reports whether `promptconduit install <tool>` ran here:
// install writes hook commands of the form "<abs path>/promptconduit hook".
func (t mcpTarget) hooksInstalled(home string) bool {
	data, err := os.ReadFile(t.hooksFile(home))
	return err == nil &&
		(bytes.Contains(data, []byte("promptconduit hook")) || bytes.Contains(data, []byte("promptconduit.exe hook")))
}

// register adds (or refreshes) our server entry pointing at exe.
func (t mcpTarget) register(home, exe string) error {
	if t.upToDate(home, exe) {
		return nil
	}
	if t.viaClaudeCLI {
		// `claude mcp add` refuses an existing name, so a stale entry (the
		// binary moved) is removed first. Only then: a working entry is never
		// removed by a re-install.
		if t.registered(home) {
			_ = runClaudeCLI("mcp", "remove", "--scope", "user", mcpServerName)
		}
		return runClaudeCLI("mcp", "add", "--scope", "user", mcpServerName, "--", exe, "mcp")
	}
	path := t.configFile(home)
	return updateMCPServers(path, func(servers map[string]interface{}) bool {
		servers[mcpServerName] = map[string]interface{}{"command": exe, "args": []interface{}{"mcp"}}
		return true
	})
}

// unregister removes our server entry; removed is false when it wasn't there.
func (t mcpTarget) unregister(home string) (removed bool, err error) {
	if !t.registered(home) {
		return false, nil
	}
	if t.viaClaudeCLI {
		return true, runClaudeCLI("mcp", "remove", "--scope", "user", mcpServerName)
	}
	err = updateMCPServers(t.configFile(home), func(servers map[string]interface{}) bool {
		if _, ok := servers[mcpServerName]; !ok {
			return false
		}
		delete(servers, mcpServerName)
		return true
	})
	return err == nil, err
}

// readMCPServers returns the file's top-level object and its mcpServers map.
// A missing file is an empty config.
func readMCPServers(path string) (servers map[string]interface{}, root map[string]interface{}, err error) {
	root = map[string]interface{}{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]interface{}{}, root, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	servers, _ = root["mcpServers"].(map[string]interface{})
	if servers == nil {
		servers = map[string]interface{}{}
	}
	return servers, root, nil
}

// updateMCPServers applies edit to the file's mcpServers and writes it back
// when edit reports a change. An emptied mcpServers key is dropped.
func updateMCPServers(path string, edit func(map[string]interface{}) bool) error {
	servers, root, err := readMCPServers(path)
	if err != nil {
		return err
	}
	if !edit(servers) {
		return nil
	}
	if len(servers) == 0 {
		delete(root, "mcpServers")
	} else {
		root["mcpServers"] = servers
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Atomic: the agent may read its config while we write; a crash or a
	// concurrent read must never see a truncated file with the user's other
	// servers and settings gone.
	return intsync.WriteFileAtomic(path, append(data, '\n'), fileMode(path, 0o644))
}

// fileMode keeps an existing file's permissions.
func fileMode(path string, fallback os.FileMode) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return fallback
}

// ============================================================================
// Opt-out state
// ============================================================================

// mcpState remembers explicit opt-outs (`mcp uninstall`, `install --no-mcp`)
// so the post-upgrade reconcile doesn't add the server back: Disabled for
// everywhere, DisabledTools per tool.
type mcpState struct {
	Disabled      bool     `json:"disabled"`
	DisabledTools []string `json:"disabled_tools,omitempty"`
}

func (s mcpState) disabledFor(tool string) bool {
	if s.Disabled {
		return true
	}
	for _, t := range s.DisabledTools {
		if t == tool {
			return true
		}
	}
	return false
}

// setToolOptOut records (or clears) a per-tool opt-out. Clearing one also
// lifts a global opt-out, since the user just asked for MCP on that tool.
func setToolOptOut(tool string, out bool) {
	s := loadMCPState()
	kept := s.DisabledTools[:0]
	for _, t := range s.DisabledTools {
		if t != tool {
			kept = append(kept, t)
		}
	}
	s.DisabledTools = kept
	if out {
		s.DisabledTools = append(s.DisabledTools, tool)
	} else {
		s.Disabled = false
	}
	_ = saveMCPState(s)
}

func mcpStatePath() string { return filepath.Join(client.ConfigDir(), "mcp.json") }

func loadMCPState() mcpState {
	var s mcpState
	if data, err := os.ReadFile(mcpStatePath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

func saveMCPState(s mcpState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(mcpStatePath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(mcpStatePath(), append(data, '\n'), 0o644)
}

// ============================================================================
// Install / uninstall / reconcile wiring
// ============================================================================

// registerMCPForInstall registers the MCP server after `install <tool>`.
// Best-effort: the hook install already succeeded, so problems only print a
// hint. Tools without MCP support (codex, copilot, grok) are skipped.
func registerMCPForInstall(tool, exe string, skip bool) {
	t, ok := mcpTargetFor(tool)
	if !ok {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if skip {
		// --no-mcp is for this tool only: record it and drop an existing entry.
		setToolOptOut(tool, true)
		if removed, err := t.unregister(home); err == nil && removed {
			fmt.Printf("✓ Removed the PromptConduit MCP server from %s (--no-mcp)\n\n", t.Label)
		}
		return
	}
	if err := t.register(home, exe); err != nil {
		fmt.Printf("• Couldn't register the PromptConduit MCP server with %s: %v\n", t.Label, err)
		if t.viaClaudeCLI {
			fmt.Printf("  Register it yourself: %s\n", claudeMCPAddCommand(exe))
		}
		fmt.Println()
		return
	}
	setToolOptOut(tool, false)
	fmt.Printf("✓ Registered the PromptConduit MCP server with %s\n", t.Label)
	fmt.Println("  Ask your agent \"what has this session cost?\" or \"how is the team tracking against budget?\"")
	if t.Tool == "cursor" {
		fmt.Println("  Turn it on in Cursor Settings → MCP if Cursor doesn't enable new servers automatically.")
	}
	fmt.Println()
}

// unregisterMCPForUninstall removes the MCP server alongside a tool's hooks.
func unregisterMCPForUninstall(tool string) {
	t, ok := mcpTargetFor(tool)
	if !ok {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	removed, err := t.unregister(home)
	switch {
	case err != nil:
		fmt.Printf("• Couldn't remove the PromptConduit MCP server from %s: %v\n", t.Label, err)
	case removed:
		fmt.Printf("✓ Removed the PromptConduit MCP server from %s\n", t.Label)
	}
}

// reconcileMCP runs once after a CLI upgrade: existing users installed hooks
// before the MCP server existed, so register it for every tool whose hooks
// are installed — unless they opted out. Quiet unless it changed something.
func reconcileMCP(notice func(format string, args ...interface{})) {
	state := loadMCPState()
	if state.Disabled {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if exe, err = stableExecutablePath(exe); err != nil {
		return
	}
	for _, t := range mcpTargets {
		if state.disabledFor(t.Tool) || !t.hooksInstalled(home) || t.registered(home) {
			continue
		}
		if err := t.register(home, exe); err != nil {
			continue
		}
		notice("promptconduit: added the PromptConduit MCP server to %s — your agent can now answer cost and budget questions.\n"+
			"  Turn it off with: promptconduit mcp uninstall\n", t.Label)
	}
}
