package cmd

import (
	"github.com/spf13/cobra"
)

var (
	// Version is set at build time via ldflags
	Version = "dev"
)

var rootCmd = &cobra.Command{
	Use:   "promptconduit",
	Short: "PromptConduit CLI - Capture AI assistant events",
	Long: `PromptConduit captures events from AI coding assistants and sends them to
the PromptConduit API for analysis and insights.

Supported tools:
  - Claude Code
  - Cursor
  - Gemini CLI

Get started:
  1. Set your API key: export PROMPTCONDUIT_API_KEY="your-key"
  2. Install hooks: promptconduit install claude-code
  3. Use your AI assistant as normal - events are captured automatically`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(testCmd)
	rootCmd.AddCommand(hookCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(configCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Printf("promptconduit %s\n", Version)
	},
}
