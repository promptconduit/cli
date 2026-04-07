package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

var (
	skillsApproved string
	skillsType     string
	skillsLimit    int
	skillsForce    bool
	skillsSyncDir  string
)

// skillsCmd is the parent command
var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage AI-generated skills and slash commands",
	Long: `PromptConduit analyzes your AI assistant usage patterns and generates
reusable slash commands (skills) tailored to how you work.

Skills are Claude Code command files stored in ~/.claude/commands/.
Once synced, invoke them with /skill-name inside Claude Code.

Examples:
  promptconduit skills generate         # Detect skills from your usage patterns
  promptconduit skills list             # List detected skills
  promptconduit skills list --approved  # Show only approved skills
  promptconduit skills sync             # Install approved skills to ~/.claude/commands/
  promptconduit skills patterns         # Show recurring prompt patterns`,
}

// skillsListCmd lists skills from the platform
var skillsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List detected skills",
	Long: `List skills detected from your AI coding session patterns.

Use --approved to filter for approved skills ready to install.
Use --type to filter by skill type (workflow, command, template, checklist).`,
	RunE: runSkillsList,
}

// skillsGenerateCmd triggers skill generation on the platform
var skillsGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Detect skills from your usage patterns",
	Long: `Analyze your AI coding session history to detect repeatable workflows
and generate suggested slash commands (skills).

The platform analyzes:
  - Conversation patterns from synced transcripts
  - Recurring prompt themes (repeated questions and requests)

Generated skills are pending approval. Review them with 'skills list',
then install with 'skills sync' after approving in the dashboard.

Results are cached for 24 hours. Use --force to re-analyze.`,
	RunE: runSkillsGenerate,
}

// skillsSyncCmd downloads approved skills to ~/.claude/commands/
var skillsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Install approved skills to ~/.claude/commands/",
	Long: `Download all approved skills as Claude Code command files.

Each skill becomes a .md file in ~/.claude/commands/ (or --dir).
After syncing, invoke skills with /skill-name inside Claude Code.

Skills are only synced if they have been approved in the PromptConduit
dashboard or via the API. Pending skills are not installed.`,
	RunE: runSkillsSync,
}

// skillsPatternsCmd shows detected prompt patterns
var skillsPatternsCmd = &cobra.Command{
	Use:   "patterns",
	Short: "Show recurring prompt patterns from your usage",
	Long: `Analyze your prompt history to show recurring themes and requests.

This shows what you repeatedly ask your AI assistant, grouped by theme.
These patterns drive skill generation — if you see a pattern here,
running 'skills generate' will produce a skill for it.

Requires the Anthropic API key to be configured on the platform.`,
	RunE: runSkillsPatterns,
}

func init() {
	skillsListCmd.Flags().StringP("format", "f", "text", "Output format (text, json)")
	skillsListCmd.Flags().StringVar(&skillsApproved, "approved", "", "Filter by approval: true, false, or pending")
	skillsListCmd.Flags().StringVar(&skillsType, "type", "", "Filter by type: workflow, command, template, checklist")
	skillsListCmd.Flags().IntVarP(&skillsLimit, "limit", "l", 50, "Maximum number of skills to show")

	skillsGenerateCmd.Flags().BoolVar(&skillsForce, "force", false, "Re-analyze even if cache is still valid")
	skillsGenerateCmd.Flags().StringP("format", "f", "text", "Output format (text, json)")

	skillsSyncCmd.Flags().StringVar(&skillsSyncDir, "dir", "", "Target directory (default: ~/.claude/commands/)")
	skillsSyncCmd.Flags().StringP("format", "f", "text", "Output format (text, json)")

	skillsPatternsCmd.Flags().StringP("format", "f", "text", "Output format (text, json)")

	skillsCmd.AddCommand(skillsListCmd)
	skillsCmd.AddCommand(skillsGenerateCmd)
	skillsCmd.AddCommand(skillsSyncCmd)
	skillsCmd.AddCommand(skillsPatternsCmd)
}

// ============================================================================
// HANDLERS
// ============================================================================

func runSkillsList(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	// Translate --approved flag
	approvedFilter := ""
	if skillsApproved == "true" {
		approvedFilter = "true"
	} else if skillsApproved == "false" || skillsApproved == "pending" {
		approvedFilter = "false"
	}

	apiClient := client.NewClient(cfg, Version)
	resp := apiClient.GetSkills(approvedFilter, skillsType, skillsLimit)

	if !resp.Success {
		return fmt.Errorf("failed to list skills: %s", resp.Error)
	}

	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		return outputJSON(resp.Data)
	}

	return outputSkillsList(resp.Data)
}

func runSkillsGenerate(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	fmt.Println("Analyzing your AI coding session patterns...")
	fmt.Println("This may take up to 30 seconds.")
	fmt.Println()

	apiClient := client.NewClient(cfg, Version)
	resp := apiClient.GenerateSkills(skillsForce)

	if !resp.Success {
		return fmt.Errorf("failed to generate skills: %s", resp.Error)
	}

	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		return outputJSON(resp.Data)
	}

	return outputSkillsGenerated(resp.Data)
}

func runSkillsSync(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	// Determine target directory
	targetDir := skillsSyncDir
	if targetDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to determine home directory: %w", err)
		}
		targetDir = filepath.Join(home, ".claude", "commands")
	}

	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create commands directory %s: %w", targetDir, err)
	}

	apiClient := client.NewClient(cfg, Version)

	// Fetch approved skills
	resp := apiClient.GetSkills("true", "", 100)
	if !resp.Success {
		return fmt.Errorf("failed to fetch approved skills: %s", resp.Error)
	}

	skillsList, ok := resp.Data["skills"].([]interface{})
	if !ok || len(skillsList) == 0 {
		fmt.Println("No approved skills to sync.")
		fmt.Println()
		fmt.Println("To get started:")
		fmt.Println("  1. Run 'promptconduit skills generate' to detect skills from your usage")
		fmt.Println("  2. Approve skills in the PromptConduit dashboard")
		fmt.Println("  3. Run 'promptconduit skills sync' again")
		return nil
	}

	synced := 0
	failed := 0

	fmt.Printf("Syncing %d approved skills to %s\n\n", len(skillsList), targetDir)

	for _, s := range skillsList {
		skill, ok := s.(map[string]interface{})
		if !ok {
			continue
		}

		id, _ := skill["id"].(string)
		name, _ := skill["name"].(string)
		displayName, _ := skill["display_name"].(string)
		if displayName == "" {
			displayName = name
		}

		if id == "" || name == "" {
			continue
		}

		// Fetch the formatted command file
		content, err := apiClient.GetSkillCommandFile(id)
		if err != nil {
			fmt.Printf("  [FAIL] %s: %v\n", name, err)
			failed++
			continue
		}

		// Write to ~/.claude/commands/<name>.md
		filename := filepath.Join(targetDir, name+".md")
		if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
			fmt.Printf("  [FAIL] %s: failed to write file: %v\n", name, err)
			failed++
			continue
		}

		fmt.Printf("  [OK]   /%s → %s\n", name, filepath.Base(filename))
		if displayName != name {
			fmt.Printf("         %s\n", displayName)
		}
		synced++
	}

	fmt.Printf("\n%d synced, %d failed\n", synced, failed)

	if synced > 0 {
		fmt.Printf("\nSkills installed! Use them in Claude Code with /<skill-name>\n")
		fmt.Printf("Location: %s\n", targetDir)
	}

	if failed > 0 {
		return fmt.Errorf("%d skill(s) failed to sync", failed)
	}

	return nil
}

func runSkillsPatterns(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	fmt.Println("Analyzing your prompt history for recurring patterns...")
	fmt.Println()

	apiClient := client.NewClient(cfg, Version)
	resp := apiClient.GetSkillPatterns()

	if !resp.Success {
		return fmt.Errorf("failed to get patterns: %s", resp.Error)
	}

	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		return outputJSON(resp.Data)
	}

	return outputPromptPatterns(resp.Data)
}

// ============================================================================
// OUTPUT FORMATTERS
// ============================================================================

func outputSkillsList(data map[string]interface{}) error {
	skills, _ := data["skills"].([]interface{})
	total, _ := data["total"].(float64)

	fmt.Println("Skills")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	if len(skills) == 0 {
		fmt.Println("No skills found.")
		fmt.Println()
		fmt.Println("Run 'promptconduit skills generate' to detect skills from your usage patterns.")
		return nil
	}

	for _, s := range skills {
		skill, ok := s.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := skill["name"].(string)
		displayName, _ := skill["display_name"].(string)
		description, _ := skill["description"].(string)
		skillType, _ := skill["skill_type"].(string)
		confidence, _ := skill["confidence"].(float64)
		isApproved := skill["is_approved"]

		// Approval status indicator
		status := "[pending]"
		if isApproved == true || isApproved == float64(1) {
			status = "[approved]"
		} else if isApproved == false || isApproved == float64(0) {
			status = "[rejected]"
		}

		fmt.Printf("/%s  %s  %s  %.0f%%\n", name, skillType, status, confidence*100)
		if displayName != "" && displayName != name {
			fmt.Printf("  %s\n", displayName)
		}
		if description != "" {
			fmt.Printf("  %s\n", description)
		}
		fmt.Println()
	}

	if total > float64(len(skills)) {
		fmt.Printf("Showing %d of %.0f skills\n", len(skills), total)
	}

	fmt.Println("Approve skills in the PromptConduit dashboard, then run 'promptconduit skills sync' to install.")
	return nil
}

func outputSkillsGenerated(data map[string]interface{}) error {
	skills, _ := data["skills"].([]interface{})
	convCount, _ := data["conversationCount"].(float64)
	fromCache, _ := data["fromCache"].(bool)

	fmt.Println("Skill Detection Results")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	if fromCache {
		fmt.Println("(Returned from cache — use --force to re-analyze)")
		fmt.Println()
	} else if convCount > 0 {
		fmt.Printf("Analyzed %.0f conversations\n\n", convCount)
	}

	if len(skills) == 0 {
		fmt.Println("No skills detected.")
		fmt.Println()
		fmt.Println("Tips:")
		fmt.Println("  • Sync more transcripts: promptconduit sync")
		fmt.Println("  • More sessions improve detection accuracy")
		return nil
	}

	fmt.Printf("Detected %d skills:\n\n", len(skills))

	for _, s := range skills {
		skill, ok := s.(map[string]interface{})
		if !ok {
			continue
		}

		name, _ := skill["name"].(string)
		displayName, _ := skill["display_name"].(string)
		description, _ := skill["description"].(string)
		skillType, _ := skill["skill_type"].(string)
		confidence, _ := skill["confidence"].(float64)
		triggerPattern, _ := skill["trigger_pattern"].(string)

		fmt.Printf("  /%s  (%s, %.0f%% confidence)\n", name, skillType, confidence*100)
		if displayName != "" {
			fmt.Printf("  %s\n", displayName)
		}
		if description != "" {
			fmt.Printf("  %s\n", description)
		}
		if triggerPattern != "" {
			fmt.Printf("  Triggers: %s\n", truncateString(triggerPattern, 80))
		}
		fmt.Println()
	}

	fmt.Println("Next steps:")
	fmt.Println("  1. Review and approve skills at promptconduit.io/skills")
	fmt.Println("  2. Run 'promptconduit skills sync' to install approved skills")
	return nil
}

func outputPromptPatterns(data map[string]interface{}) error {
	clusters, _ := data["clusters"].([]interface{})
	totalPrompts, _ := data["totalPromptsAnalyzed"].(float64)
	analysisNotes, _ := data["analysisNotes"].(string)
	available, _ := data["available"].(bool)

	fmt.Println("Prompt Patterns")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	if !available {
		fmt.Println("Prompt pattern analysis is not available.")
		fmt.Println("The Anthropic API key must be configured on the platform.")
		return nil
	}

	if totalPrompts > 0 {
		fmt.Printf("Analyzed %.0f prompts\n\n", totalPrompts)
	}

	if len(clusters) == 0 {
		fmt.Println("No recurring patterns detected yet.")
		fmt.Println()
		fmt.Println("Patterns emerge after using your AI assistant more.")
		fmt.Println("Sync transcripts and try again: promptconduit sync")
		return nil
	}

	fmt.Printf("Found %d recurring patterns:\n\n", len(clusters))

	for i, cl := range clusters {
		cluster, ok := cl.(map[string]interface{})
		if !ok {
			continue
		}

		theme, _ := cluster["theme"].(string)
		frequency, _ := cluster["frequency"].(float64)
		description, _ := cluster["description"].(string)
		suggestedType, _ := cluster["suggestedSkillType"].(string)
		examples, _ := cluster["examplePrompts"].([]interface{})

		fmt.Printf("%d. %s (%.0fx, suggested: %s)\n", i+1, theme, frequency, suggestedType)
		if description != "" {
			fmt.Printf("   %s\n", description)
		}
		if len(examples) > 0 {
			for _, ex := range examples {
				if exStr, ok := ex.(string); ok {
					fmt.Printf("   • \"%s\"\n", truncateString(exStr, 70))
				}
			}
		}
		fmt.Println()
	}

	if analysisNotes != "" {
		fmt.Printf("Note: %s\n\n", analysisNotes)
	}

	fmt.Println("Run 'promptconduit skills generate' to turn these patterns into skills.")
	return nil
}

func truncateString(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-3]) + "..."
}
