package cmd

import (
	"fmt"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage PromptConduit configuration",
	Long: `Manage PromptConduit configuration stored in ~/.promptconduit/config.json.

This config file is used by hooks when environment variables are not set,
making it ideal for use with tools like Claude Code that spawn subprocesses.

Quick start:
  promptconduit config set --api-key=sk_xxx

Priority order: environment variables > config file > defaults`,
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show current configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := client.LoadConfig()
		fc, _ := client.LoadFileConfig()

		cmd.Printf("API URL: %s\n", cfg.APIURL)
		cmd.Printf("API Key: %s\n", client.MaskAPIKey(cfg.APIKey))
		if cfg.Debug {
			cmd.Printf("Debug:   %v\n", cfg.Debug)
		}
		cmd.Println()
		cmd.Printf("Config:  %s\n", client.ConfigPath())

		// Show environment info if using environments
		if fc != nil && fc.CurrentEnv != "" && len(fc.Environments) > 0 {
			cmd.Printf("Env:     %s\n", fc.CurrentEnv)
		}

		return nil
	},
}

var (
	setAPIKey string
	setAPIURL string
	setDebug  bool
)

var configSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set configuration values",
	Long: `Set configuration values in ~/.promptconduit/config.json.

Examples:
  # Basic setup (most users)
  promptconduit config set --api-key=sk_xxx

  # With custom API URL
  promptconduit config set --api-key=sk_xxx --api-url=http://localhost:8787

  # Enable debug mode
  promptconduit config set --debug`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fc, err := client.LoadFileConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if fc == nil {
			fc = &client.FileConfig{}
		}

		// Update values if provided
		changed := false
		if setAPIKey != "" {
			fc.APIKey = setAPIKey
			changed = true
		}
		if cmd.Flags().Changed("api-url") {
			fc.APIURL = setAPIURL
			changed = true
		}
		if cmd.Flags().Changed("debug") {
			fc.Debug = setDebug
			changed = true
		}

		if !changed {
			return fmt.Errorf("no values provided. Use --api-key, --api-url, or --debug")
		}

		if err := client.SaveFileConfig(fc); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		cmd.Println("Configuration saved")
		if fc.APIKey != "" {
			cmd.Printf("  API Key: %s\n", client.MaskAPIKey(fc.APIKey))
		}
		if fc.APIURL != "" {
			cmd.Printf("  API URL: %s\n", fc.APIURL)
		}
		if fc.Debug {
			cmd.Printf("  Debug:   %v\n", fc.Debug)
		}

		return nil
	},
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Show the config file path",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Println(client.ConfigPath())
	},
}

// Environment management subcommands (power user feature)
var configEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage multiple environments (advanced)",
	Long: `Manage multiple environment configurations for switching between
local development, staging, and production.

Most users don't need this - just use 'promptconduit config set --api-key=...'`,
}

var configEnvListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured environments",
	RunE: func(cmd *cobra.Command, args []string) error {
		fc, err := client.LoadFileConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if fc == nil || len(fc.Environments) == 0 {
			cmd.Println("No environments configured.")
			cmd.Println("Add one with: promptconduit config env add <name> --api-key=...")
			return nil
		}

		for name, env := range fc.Environments {
			marker := "  "
			if name == fc.CurrentEnv {
				marker = "→ "
			}
			cmd.Printf("%s%s: %s (%s)\n", marker, name, env.APIURL, client.MaskAPIKey(env.APIKey))
		}

		return nil
	},
}

var (
	envAPIKey string
	envAPIURL string
	envDebug  bool
)

var configEnvAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add or update an environment",
	Long: `Add or update a named environment configuration.

Examples:
  promptconduit config env add local --api-key=sk_xxx --api-url=http://localhost:8787
  promptconduit config env add prod --api-key=sk_xxx`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		envName := args[0]

		if envAPIKey == "" {
			return fmt.Errorf("--api-key is required")
		}

		fc, err := client.LoadFileConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if fc == nil {
			fc = &client.FileConfig{}
		}

		if fc.Environments == nil {
			fc.Environments = make(map[string]*client.Config)
		}

		apiURL := envAPIURL
		if apiURL == "" {
			apiURL = client.DefaultAPIURL
		}

		fc.Environments[envName] = &client.Config{
			APIKey: envAPIKey,
			APIURL: apiURL,
			Debug:  envDebug,
		}

		// Auto-switch to first environment added
		if fc.CurrentEnv == "" {
			fc.CurrentEnv = envName
		}

		if err := client.SaveFileConfig(fc); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		cmd.Printf("Added environment '%s'\n", envName)
		if fc.CurrentEnv == envName {
			cmd.Printf("  (currently active)\n")
		}

		return nil
	},
}

var configEnvUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Switch to an environment",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		envName := args[0]

		fc, err := client.LoadFileConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if fc == nil || fc.Environments == nil {
			return fmt.Errorf("no environments configured")
		}

		if _, ok := fc.Environments[envName]; !ok {
			cmd.Printf("Environment '%s' not found. Available:\n", envName)
			for name := range fc.Environments {
				cmd.Printf("  - %s\n", name)
			}
			return fmt.Errorf("environment not found")
		}

		fc.CurrentEnv = envName
		if err := client.SaveFileConfig(fc); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		env := fc.Environments[envName]
		cmd.Printf("→ %s (%s)\n", envName, env.APIURL)

		return nil
	},
}

var configEnvRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove an environment",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		envName := args[0]

		fc, err := client.LoadFileConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if fc == nil || fc.Environments == nil {
			return fmt.Errorf("no environments configured")
		}

		if _, ok := fc.Environments[envName]; !ok {
			return fmt.Errorf("environment '%s' not found", envName)
		}

		delete(fc.Environments, envName)

		if fc.CurrentEnv == envName {
			fc.CurrentEnv = ""
			for name := range fc.Environments {
				fc.CurrentEnv = name
				break
			}
		}

		if err := client.SaveFileConfig(fc); err != nil {
			return fmt.Errorf("failed to save config: %w", err)
		}

		cmd.Printf("Removed environment '%s'\n", envName)

		return nil
	},
}

func init() {
	// Main config commands
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configEnvCmd)

	// config set flags
	configSetCmd.Flags().StringVar(&setAPIKey, "api-key", "", "API key")
	configSetCmd.Flags().StringVar(&setAPIURL, "api-url", "", "API URL (default: https://api.promptconduit.dev)")
	configSetCmd.Flags().BoolVar(&setDebug, "debug", false, "Enable debug mode")

	// Environment subcommands
	configEnvCmd.AddCommand(configEnvListCmd)
	configEnvCmd.AddCommand(configEnvAddCmd)
	configEnvCmd.AddCommand(configEnvUseCmd)
	configEnvCmd.AddCommand(configEnvRemoveCmd)

	// config env add flags
	configEnvAddCmd.Flags().StringVar(&envAPIKey, "api-key", "", "API key (required)")
	configEnvAddCmd.Flags().StringVar(&envAPIURL, "api-url", "", "API URL")
	configEnvAddCmd.Flags().BoolVar(&envDebug, "debug", false, "Enable debug mode")
}
