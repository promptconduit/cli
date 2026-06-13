package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/promptconduit/cli/internal/cost"
	"github.com/spf13/cobra"
)

var (
	costCwd  string
	costAll  bool
	costJSON bool
	costDays int
)

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Real-time AI token cost for your sessions (100% local)",
	Long: `Compute what your AI coding sessions cost, in real time, entirely on
your machine. The cost feature reads local transcripts, prices each turn against
a bundled rate table, and never sends any of your data to the PromptConduit
platform or anywhere else.

Subcommands:
  watch           Stream cost events as your session runs (the editor extension's feed)
  session         Print the current session's cost summary
  history         Aggregate cost over the last N days
  install-hooks   Wire the local Cursor cost hook into ~/.cursor/hooks.json
  uninstall-hooks Remove it

Prices both Claude Code (exact token counts from the transcript) and Cursor
(exact token counts from its agent hooks — run install-hooks first). Models
not in the bundled rate table are reported with exact tokens but flagged
unpriced rather than guessed.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var costWatchCmd = &cobra.Command{
	Use:           "watch",
	Short:         "Stream real-time cost events (newline-delimited JSON)",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostWatch,
}

var costSessionCmd = &cobra.Command{
	Use:           "session",
	Short:         "Print the current session's cost summary",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostSession,
}

var costHistoryCmd = &cobra.Command{
	Use:           "history",
	Short:         "Aggregate cost over recent days from the local store",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostHistory,
}

var costHookCmd = &cobra.Command{
	Use:           "hook",
	Short:         "Record a Cursor agent hook payload's cost (local-only; for ~/.cursor/hooks.json)",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostHook,
}

var costInstallHooksCmd = &cobra.Command{
	Use:           "install-hooks",
	Short:         "Wire the local Cursor cost hook into ~/.cursor/hooks.json",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostInstallHooks,
}

var costUninstallHooksCmd = &cobra.Command{
	Use:           "uninstall-hooks",
	Short:         "Remove the Cursor cost hook from ~/.cursor/hooks.json",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCostUninstallHooks,
}

func init() {
	costWatchCmd.Flags().StringVar(&costCwd, "cwd", "", "workspace directory to scope to (default: current directory)")
	costWatchCmd.Flags().BoolVar(&costAll, "all", false, "watch every Claude Code project, not just the current workspace")
	costWatchCmd.Flags().BoolVar(&costJSON, "json", true, "emit newline-delimited JSON (the only format today)")

	costSessionCmd.Flags().StringVar(&costCwd, "cwd", "", "workspace directory to scope to (default: current directory)")
	costSessionCmd.Flags().BoolVar(&costJSON, "json", true, "emit JSON (the only format today)")

	costHistoryCmd.Flags().IntVar(&costDays, "days", 7, "number of days to include")
	costHistoryCmd.Flags().BoolVar(&costJSON, "json", false, "emit JSON instead of a table")

	costCmd.AddCommand(costWatchCmd)
	costCmd.AddCommand(costSessionCmd)
	costCmd.AddCommand(costHistoryCmd)
	costCmd.AddCommand(costHookCmd)
	costCmd.AddCommand(costInstallHooksCmd)
	costCmd.AddCommand(costUninstallHooksCmd)
}

// resolveCwd returns the explicit --cwd or the process working directory.
func resolveCwd() string {
	if costCwd != "" {
		return costCwd
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

func runCostWatch(cmd *cobra.Command, args []string) error {
	table, err := cost.LoadBundledPriceTable()
	if err != nil {
		return fmt.Errorf("load pricing table: %w", err)
	}

	dirs, err := cost.ResolveDirs(resolveCwd(), costAll)
	if err != nil {
		return fmt.Errorf("resolve transcript directories: %w", err)
	}

	// Persist to the local store best-effort; a store failure must never stop
	// the live stream the extension depends on.
	store, err := cost.OpenStore()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "cost: local store unavailable (%v); continuing without history\n", err)
		store = nil
	}
	if store != nil {
		defer store.Close()
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cursorFeeds := cost.CursorFeedPaths(resolveCwd(), costAll)

	scope := "current workspace"
	if costAll {
		scope = "all projects"
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "cost: watching %s (Claude Code + Cursor) — local only, Ctrl-C to stop.\n", scope)

	w := cost.NewWatcher(table, store, cmd.OutOrStdout(), true)
	return w.Run(ctx, dirs, cursorFeeds)
}

func runCostSession(cmd *cobra.Command, args []string) error {
	table, err := cost.LoadBundledPriceTable()
	if err != nil {
		return fmt.Errorf("load pricing table: %w", err)
	}
	dirs, err := cost.ResolveDirs(resolveCwd(), false)
	if err != nil {
		return fmt.Errorf("resolve transcript directories: %w", err)
	}
	w := cost.NewWatcher(table, nil, cmd.OutOrStdout(), false)
	w.SeedNewest(dirs)
	w.SeedCursorFeeds(cost.CursorFeedPaths(resolveCwd(), false))
	w.EmitLatestSession()
	return nil
}

func runCostHook(cmd *cobra.Command, args []string) error {
	// Always tell the agent to continue, no matter what — a cost hook must
	// never block or fail the editor.
	defer fmt.Fprintln(cmd.OutOrStdout(), `{"continue": true}`)

	raw, err := io.ReadAll(cmd.InOrStdin())
	if err != nil || len(raw) == 0 {
		return nil
	}
	table, err := cost.LoadBundledPriceTable()
	if err != nil {
		return nil
	}
	ev, cwd, ok := cost.ParseCursorHookPayload(raw, table)
	if !ok {
		return nil // not a token-bearing Cursor event — nothing to record
	}
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339)
	_ = cost.AppendCursorEvent(ev, cwd) // best-effort, local-only
	return nil
}

// cursorCostHooks are the Cursor agent events the cost hook listens on. Both
// carry identical per-generation tokens; the watcher dedups by generation_id,
// so installing both is safe and resilient.
var cursorCostHookEvents = []string{"afterAgentResponse", "stop"}

func runCostInstallHooks(cmd *cobra.Command, args []string) error {
	exe, err := osExecutable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".cursor", "hooks.json")

	settings := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = map[string]interface{}{}
	}

	hookCmd := fmt.Sprintf("%s cost hook", exe)
	for _, event := range cursorCostHookEvents {
		entries, _ := hooks[event].([]interface{})
		// Drop any prior cost-hook entry so re-install is idempotent.
		kept := entries[:0:0]
		for _, e := range entries {
			if m, ok := e.(map[string]interface{}); ok {
				if c, _ := m["command"].(string); containsCostHook(c) {
					continue
				}
			}
			kept = append(kept, e)
		}
		kept = append(kept, map[string]interface{}{"command": hookCmd})
		hooks[event] = kept
	}
	settings["hooks"] = hooks
	if settings["version"] == nil {
		settings["version"] = 1
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed Cursor cost hooks (%v) in %s\n", cursorCostHookEvents, path)
	fmt.Fprintln(cmd.OutOrStdout(), "Run `promptconduit cost watch` (or the editor extension) to see live cost.")
	return nil
}

func runCostUninstallHooks(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".cursor", "hooks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "No Cursor hooks file — nothing to remove.")
		return nil
	}
	settings := map[string]interface{}{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	if hooks, ok := settings["hooks"].(map[string]interface{}); ok {
		for event, v := range hooks {
			entries, _ := v.([]interface{})
			kept := entries[:0:0]
			for _, e := range entries {
				if m, ok := e.(map[string]interface{}); ok {
					if c, _ := m["command"].(string); containsCostHook(c) {
						continue
					}
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				delete(hooks, event)
			} else {
				hooks[event] = kept
			}
		}
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed Cursor cost hooks from %s\n", path)
	return nil
}

// containsCostHook reports whether a hook command string is our cost hook
// (`<binary> cost hook`), so install/uninstall can find and replace just ours.
func containsCostHook(command string) bool {
	return strings.HasSuffix(command, "cost hook")
}

// osExecutable resolves the running binary's path (indirection kept tiny so the
// install command reads cleanly).
func osExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func runCostHistory(cmd *cobra.Command, args []string) error {
	events, err := cost.ReadEvents()
	if err != nil {
		return fmt.Errorf("read cost history: %w", err)
	}

	// Group by calendar day (the YYYY-MM-DD prefix of the RFC3339 timestamp).
	type dayTotal struct {
		day  string
		cost float64
		in   int64
		out  int64
	}
	byDay := map[string]*dayTotal{}
	for _, e := range events {
		day := e.Timestamp
		if len(day) >= 10 {
			day = day[:10]
		}
		dt := byDay[day]
		if dt == nil {
			dt = &dayTotal{day: day}
			byDay[day] = dt
		}
		dt.cost += e.Cost.Total
		dt.in += e.Tokens.Input
		dt.out += e.Tokens.Output
	}

	days := make([]*dayTotal, 0, len(byDay))
	for _, dt := range byDay {
		days = append(days, dt)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].day > days[j].day })
	if costDays > 0 && len(days) > costDays {
		days = days[:costDays]
	}

	out := cmd.OutOrStdout()
	if costJSON {
		fmt.Fprint(out, "[")
		for i, dt := range days {
			if i > 0 {
				fmt.Fprint(out, ",")
			}
			fmt.Fprintf(out, `{"day":%q,"cost_total":%.6f,"input":%d,"output":%d,"currency":%q}`,
				dt.day, dt.cost, dt.in, dt.out, cost.Currency)
		}
		fmt.Fprintln(out, "]")
		return nil
	}

	if len(days) == 0 {
		fmt.Fprintln(out, "No cost history yet. Run `promptconduit cost watch` during a session.")
		return nil
	}
	fmt.Fprintf(out, "%-12s %12s %12s %12s\n", "DAY", "COST (USD)", "INPUT", "OUTPUT")
	var total float64
	for _, dt := range days {
		fmt.Fprintf(out, "%-12s %12.4f %12d %12d\n", dt.day, dt.cost, dt.in, dt.out)
		total += dt.cost
	}
	fmt.Fprintf(out, "%-12s %12.4f\n", "TOTAL", total)
	return nil
}
