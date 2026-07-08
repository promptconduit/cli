package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/outbound"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Interactive setup wizard",
	Long: `Guided setup for PromptConduit: choose cloud sync or local-only mode,
authenticate (if needed), detect installed AI tools, and install hooks.

Run with no flags for an interactive wizard, or use --yes to accept defaults.`,
	RunE: runInit,
}

var (
	initLocalOnly bool
	initAPIKey    string
	initYes       bool
	initTools     string
)

func init() {
	initCmd.Flags().BoolVar(&initLocalOnly, "local-only", false, "Local-only mode — no account or cloud sync")
	initCmd.Flags().StringVar(&initAPIKey, "api-key", "", "Set API key directly (skips login)")
	initCmd.Flags().BoolVar(&initYes, "yes", false, "Non-interactive: install all detected tools")
	initCmd.Flags().StringVar(&initTools, "tools", "", "Comma-separated tools to install (skips picker)")
}

func runInit(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()

	_, _ = fmt.Fprintf(out, "PromptConduit CLI v%s\n", Version)
	_, _ = fmt.Fprintln(out, "================================")
	_, _ = fmt.Fprintln(out)

	cloudMode := !initLocalOnly

	// Step 1: Choose mode (unless flags preset it)
	if !initLocalOnly && !initYes && outbound.IsTerminal(os.Stdin) && initAPIKey == "" {
		cloudMode, initLocalOnly = promptInitMode(cmd)
	} else if !initLocalOnly && !outbound.IsTerminal(os.Stdin) && initAPIKey == "" {
		cfg := client.LoadConfig()
		if !cfg.IsConfigured() {
			return fmt.Errorf("non-interactive mode: pass --local-only, --api-key, or run in a terminal to sign in")
		}
	}

	if initLocalOnly {
		cloudMode = false
		if err := persistLocalOnly(true); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not save local-only setting: %v\n", err)
		}
		_, _ = fmt.Fprintln(out, "Mode: Free (local-only) — events captured locally, nothing sent to the cloud")
	} else {
		if err := persistLocalOnly(false); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not update local-only setting: %v\n", err)
		}
		_, _ = fmt.Fprintln(out, "Mode: Cloud sync — events sent to the PromptConduit platform")
	}
	_, _ = fmt.Fprintln(out)

	// Step 2: API key / login for cloud mode
	if cloudMode {
		if initAPIKey != "" {
			if err := client.SaveAPIKey(initAPIKey); err != nil {
				return fmt.Errorf("failed to save API key: %w", err)
			}
			_, _ = fmt.Fprintf(out, "API key configured: %s\n\n", client.MaskAPIKey(initAPIKey))
		} else {
			cfg := client.LoadConfig()
			if !cfg.IsConfigured() {
				if !outbound.IsTerminal(os.Stdin) {
					return fmt.Errorf("non-interactive mode: pass --api-key or run promptconduit login in a terminal first")
				}
				_, _ = fmt.Fprintln(out, "No API key found — starting login...")
				_, _ = fmt.Fprintln(out)
				if _, err := performLogin(cmd, "", true); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(out)
			} else {
				_, _ = fmt.Fprintf(out, "API key already configured: %s\n\n", client.MaskAPIKey(cfg.APIKey))
			}
		}
	}

	// Step 3: Detect and select tools
	detected := detectInstalledTools()
	var tools []string
	var err error

	if initTools != "" {
		tools, err = parseToolSelection(initTools)
		if err != nil {
			return err
		}
	} else if initYes {
		if len(detected) > 0 {
			tools = detected
		} else {
			tools = toolNames()
		}
		_, _ = fmt.Fprintf(out, "Installing tools: %s\n\n", strings.Join(tools, ", "))
	} else if len(args) > 0 {
		tools, err = resolveInstallTools(cmd, args)
		if err != nil {
			return err
		}
	} else {
		tools, err = selectToolsForInit(cmd, detected)
		if err != nil {
			return err
		}
	}

	if len(tools) == 0 {
		_, _ = fmt.Fprintln(out, "No tools selected — skipping hook installation.")
	} else {
		exePath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to get executable path: %w", err)
		}
		exePath, err = stableExecutablePath(exePath)
		if err != nil {
			return fmt.Errorf("failed to resolve executable path: %w", err)
		}

		_, _ = fmt.Fprintln(out, "Installing hooks...")
		for i, tool := range tools {
			if i > 0 {
				_, _ = fmt.Fprintln(out)
			}
			if err := installTool(tool, exePath); err != nil {
				return fmt.Errorf("install %s: %w", tool, err)
			}
		}
		_, _ = fmt.Fprintln(out)
	}

	// Step 4: Test connectivity (cloud mode with API key)
	cfg := client.LoadConfig()
	if cfg.ShouldSend() {
		_, _ = fmt.Fprintln(out, "Testing API connectivity...")
		apiClient := client.NewClient(cfg, Version)
		response := apiClient.TestConnection()
		if response.Success {
			_, _ = fmt.Fprintln(out, "✓ API connection verified")
		} else {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠ API test failed: %s\n", response.Error)
		}
		_, _ = fmt.Fprintln(out)
	}

	// Step 5: Summary
	printInitSummary(cmd, tools, cloudMode)
	return nil
}

func promptInitMode(cmd *cobra.Command) (cloudMode bool, localOnly bool) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "How would you like to use PromptConduit?")
	_, _ = fmt.Fprintln(out, "  1) Cloud sync — sign in and send events to the platform")
	_, _ = fmt.Fprintln(out, "  2) Local-only — capture events locally, no account needed")
	_, _ = fmt.Fprint(out, "Choose [1/2] (default 1): ")

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return true, false
	}
	choice := strings.TrimSpace(line)
	if choice == "2" {
		return false, true
	}
	return true, false
}

// selectToolsForInit shows the tool picker with detected tools pre-selected.
func selectToolsForInit(cmd *cobra.Command, detected []string) ([]string, error) {
	if !outbound.IsTerminal(os.Stdin) {
		if len(detected) > 0 {
			return detected, nil
		}
		return nil, fmt.Errorf("run in a terminal to pick tools, or pass --tools or --yes")
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Which AI tools should PromptConduit set up?")
	for i, t := range installableTools {
		marker := " "
		for _, d := range detected {
			if d == t.name {
				marker = "*"
				break
			}
		}
		_, _ = fmt.Fprintf(out, "  %s %d) %s\n", marker, i+1, t.label)
	}
	if len(detected) > 0 {
		_, _ = fmt.Fprintf(out, "\n  * = detected on this machine (%s)\n", strings.Join(detected, ", "))
	}
	defaultHint := "all"
	if len(detected) > 0 {
		defaultHint = strings.Join(detected, ",")
	}
	_, _ = fmt.Fprintf(out, "Enter numbers (e.g. 1,2), names, or 'all' [%s]: ", defaultHint)

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		if len(detected) > 0 {
			return detected, nil
		}
		return toolNames(), nil
	}
	if strings.TrimSpace(line) == "" {
		if len(detected) > 0 {
			return detected, nil
		}
		return toolNames(), nil
	}
	return parseToolSelection(line)
}

// detectInstalledTools checks common config paths for installed AI tools.
func detectInstalledTools() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	checks := []struct {
		tool  string
		paths []string
	}{
		{"cursor", []string{".cursor/hooks.json", ".cursor"}},
		{"claude-code", []string{".claude/settings.json", ".claude"}},
		{"gemini-cli", []string{".gemini/settings.json"}},
		{"codex", []string{".codex/hooks.json"}},
		{"copilot", []string{".copilot/hooks"}},
	}

	var detected []string
	seen := map[string]bool{}
	for _, c := range checks {
		for _, rel := range c.paths {
			p := filepath.Join(home, rel)
			if _, err := os.Stat(p); err == nil {
				if !seen[c.tool] {
					seen[c.tool] = true
					detected = append(detected, c.tool)
				}
				break
			}
		}
	}
	return detected
}

func printInitSummary(cmd *cobra.Command, tools []string, cloudMode bool) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Setup complete!")
	_, _ = fmt.Fprintln(out, "================")
	if cloudMode {
		_, _ = fmt.Fprintln(out, "  Mode:   Cloud sync")
		cfg := client.LoadConfig()
		if cfg.IsConfigured() {
			_, _ = fmt.Fprintf(out, "  API:    %s (%s)\n", cfg.APIURL, client.MaskAPIKey(cfg.APIKey))
		}
	} else {
		_, _ = fmt.Fprintln(out, "  Mode:   Local-only (Free)")
	}
	if len(tools) > 0 {
		_, _ = fmt.Fprintf(out, "  Tools:  %s\n", strings.Join(tools, ", "))
	} else {
		_, _ = fmt.Fprintln(out, "  Tools:  (none installed)")
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Next steps:")
	if len(tools) > 0 {
		_, _ = fmt.Fprintln(out, "  • Restart your AI tools to activate hooks")
	}
	_, _ = fmt.Fprintln(out, "  • Run `promptconduit status` to verify installation")
	if cloudMode {
		_, _ = fmt.Fprintln(out, "  • Run `promptconduit test` to verify API connectivity")
	}
	_, _ = fmt.Fprintln(out)
}
