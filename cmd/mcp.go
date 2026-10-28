package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/promptconduit/cli/internal/mcp"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run the PromptConduit MCP server so AI agents can use your cost data",
	Long: `Run the PromptConduit Model Context Protocol server over stdio. Agents that
speak MCP (Claude Code, Cursor, Gemini CLI, ...) start it themselves and can
then answer "what has this session cost?" or "are we on track for this
month's AI budget?" on their own.

'promptconduit install' registers it for Claude Code, Cursor and Gemini CLI.
To manage the registration yourself:
  promptconduit mcp install [tool...]     # default: every tool with hooks installed
  promptconduit mcp uninstall [tool...]   # and stop re-adding it after upgrades
  promptconduit mcp tools                 # list the tools agents see

Local tools (session_cost, cost_history, recent_sessions, setup_status) work on
the Free tier and read only files on this machine. team_* tools need
'promptconduit login' and a team.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runMCPServe,
}

var mcpInstallCmd = &cobra.Command{
	Use:          "install [tool...]",
	Short:        "Register the MCP server with Claude Code, Cursor or Gemini CLI",
	SilenceUsage: true,
	RunE:         runMCPInstall,
}

var mcpUninstallCmd = &cobra.Command{
	Use:          "uninstall [tool...]",
	Short:        "Remove the MCP server registration (and don't re-add it on upgrade)",
	SilenceUsage: true,
	RunE:         runMCPUninstall,
}

var mcpToolsCmd = &cobra.Command{
	Use:          "tools",
	Short:        "List the tools the MCP server exposes",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		for _, t := range buildMCPServer().Tools() {
			kind := "read"
			if !t.Annotations.ReadOnlyHint {
				kind = "write"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, kind, firstSentence(t.Description))
		}
		return w.Flush()
	},
}

func init() {
	mcpCmd.AddCommand(mcpInstallCmd, mcpUninstallCmd, mcpToolsCmd)
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

func runMCPServe(cmd *cobra.Command, args []string) error {
	srv := buildMCPServer()
	if os.Getenv("PROMPTCONDUIT_DEBUG") != "" {
		srv.Logf = func(format string, a ...interface{}) {
			_, _ = fmt.Fprintf(os.Stderr, format+"\n", a...)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

// mcpInstallTools returns the tools to act on: the named ones, else every
// MCP-capable tool whose hooks are installed, else all of them.
func mcpInstallTools(args []string, home string) ([]mcpTarget, error) {
	var out []mcpTarget
	for _, a := range args {
		t, ok := mcpTargetFor(normalizeToolName(a))
		if !ok {
			return nil, fmt.Errorf("%s doesn't support MCP registration here; supported: claude-code, cursor, gemini-cli", a)
		}
		out = append(out, t)
	}
	if len(out) > 0 {
		return out, nil
	}
	for _, t := range mcpTargets {
		if t.hooksInstalled(home) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = mcpTargets
	}
	return out, nil
}

func runMCPInstall(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	targets, err := mcpInstallTools(args, home)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = stableExecutablePath(exe); err != nil {
		return err
	}
	var failed []string
	for _, t := range targets {
		if err := t.register(home, exe); err != nil {
			failed = append(failed, t.Label)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s: %v\n", t.Label, err)
			if t.viaClaudeCLI {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  Register it yourself: %s\n", claudeMCPAddCommand(exe))
			}
			continue
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Registered the PromptConduit MCP server with %s\n", t.Label)
	}
	if len(failed) < len(targets) {
		_ = saveMCPState(mcpState{Disabled: false})
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  Start a new agent session to pick it up.")
	}
	if len(failed) > 0 {
		return fmt.Errorf("couldn't register with %s", strings.Join(failed, ", "))
	}
	return nil
}

func runMCPUninstall(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	targets := mcpTargets
	if len(args) > 0 {
		if targets, err = mcpInstallTools(args, home); err != nil {
			return err
		}
	} else {
		// Removing everywhere is an opt-out; don't re-add after upgrades.
		_ = saveMCPState(mcpState{Disabled: true})
	}
	for _, t := range targets {
		removed, err := t.unregister(home)
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s: %v\n", t.Label, err)
		case removed:
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed the PromptConduit MCP server from %s\n", t.Label)
		default:
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "• %s: not registered\n", t.Label)
		}
	}
	return nil
}

// buildMCPServer assembles the server with every tool and prompt.
func buildMCPServer() *mcp.Server {
	srv := mcp.NewServer("promptconduit", Version, mcpInstructions)
	for _, t := range mcpTools() {
		srv.AddTool(t)
	}
	for _, p := range mcpPrompts() {
		srv.AddPrompt(p)
	}
	return srv
}

const mcpInstructions = `PromptConduit measures what AI coding costs: per session, per day, and for a whole team by member, repo, client, model and pull request. Use these tools whenever the user asks about AI or token cost, spend, budgets, or which sessions or repos are expensive.

- Local tools need no account and read only this machine: session_cost (the current session), cost_history (daily totals), recent_sessions (resumable sessions), setup_status (what is installed and signed in).
- team_* tools cover the paid team product. If one fails with "not signed in", tell the user to run ` + "`promptconduit login`" + `. If they aren't on a team, team_create starts one (free for up to 3 people).
- If the user belongs to several teams, pass org (id, slug or name); the error lists the choices.
- All amounts are USD. Periods are 7d, 30d or 90d.
- Tools that change things (team_set_budget, team_remove_budget, team_tag_repo, team_invite, team_create, team_join) act on shared team settings: confirm with the user first unless they asked for that exact change.`
