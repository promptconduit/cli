package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/cobra"
)

var (
	insightsPeriod string
	insightsRepo   string
	insightsFormat string
	insightsLimit  int
)

var insightsCmd = &cobra.Command{
	Use:   "insights",
	Short: "View your AI coding assistant usage insights",
	Long: `View personal analytics and insights about your AI coding assistant usage.

This command shows you:
  - Session counts and activity trends
  - Tool usage patterns and mastery levels
  - Error patterns and areas for improvement
  - Activity streaks and consistency

All data is private to you - compare yourself to your own history, not others.`,
	RunE: runInsights,
}

var insightsToolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "View tool usage breakdown",
	Long:  `See which tools you use most frequently and your success rates.`,
	RunE:  runInsightsTools,
}

var insightsErrorsCmd = &cobra.Command{
	Use:   "errors",
	Short: "View error patterns",
	Long:  `See common error patterns to identify areas for improvement.`,
	RunE:  runInsightsErrors,
}

var insightsSessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List recent sessions",
	Long:  `View your recent AI coding assistant sessions.`,
	RunE:  runInsightsSessions,
}

func init() {
	// Main insights command flags
	insightsCmd.PersistentFlags().StringVarP(&insightsPeriod, "period", "p", "7d", "Time period (24h, 7d, 30d, 90d)")
	insightsCmd.PersistentFlags().StringVarP(&insightsRepo, "repo", "r", "", "Filter by repository name")
	insightsCmd.PersistentFlags().StringVarP(&insightsFormat, "format", "f", "text", "Output format (text, json)")

	// Sessions command flags
	insightsSessionsCmd.Flags().IntVarP(&insightsLimit, "limit", "l", 10, "Number of sessions to show")

	// Add subcommands
	insightsCmd.AddCommand(insightsToolsCmd)
	insightsCmd.AddCommand(insightsErrorsCmd)
	insightsCmd.AddCommand(insightsSessionsCmd)
}

func runInsights(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	apiClient := client.NewClient(cfg, Version)
	resp := apiClient.GetInsights(insightsPeriod, insightsRepo)

	if !resp.Success {
		return fmt.Errorf("failed to get insights: %s", resp.Error)
	}

	if insightsFormat == "json" {
		return outputJSON(resp.Data)
	}

	return outputInsightsSummary(resp.Data)
}

func runInsightsTools(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	apiClient := client.NewClient(cfg, Version)
	resp := apiClient.GetInsightsTools(insightsPeriod, insightsRepo)

	if !resp.Success {
		return fmt.Errorf("failed to get tool insights: %s", resp.Error)
	}

	if insightsFormat == "json" {
		return outputJSON(resp.Data)
	}

	return outputToolsBreakdown(resp.Data)
}

func runInsightsErrors(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	apiClient := client.NewClient(cfg, Version)
	resp := apiClient.GetInsightsErrors(insightsPeriod, insightsRepo)

	if !resp.Success {
		return fmt.Errorf("failed to get error insights: %s", resp.Error)
	}

	if insightsFormat == "json" {
		return outputJSON(resp.Data)
	}

	return outputErrorPatterns(resp.Data)
}

func runInsightsSessions(cmd *cobra.Command, args []string) error {
	cfg := client.LoadConfig()
	if !cfg.IsConfigured() {
		return fmt.Errorf("API key not configured. Run: promptconduit config set --api-key=\"your-key\"")
	}

	apiClient := client.NewClient(cfg, Version)
	resp := apiClient.GetSessions(insightsLimit, 0, insightsRepo)

	if !resp.Success {
		return fmt.Errorf("failed to get sessions: %s", resp.Error)
	}

	if insightsFormat == "json" {
		return outputJSON(resp.Data)
	}

	return outputSessionsList(resp.Data)
}

// Output helpers

func outputJSON(data map[string]interface{}) error {
	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format JSON: %w", err)
	}
	fmt.Println(string(jsonBytes))
	return nil
}

func outputInsightsSummary(data map[string]interface{}) error {
	fmt.Println("Your AI Coding Insights")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	// Period info
	period := getStringValue(data, "period")
	fmt.Printf("Period: %s\n\n", period)

	// Summary section
	if summary, ok := data["summary"].(map[string]interface{}); ok {
		fmt.Println("Summary")
		fmt.Println(strings.Repeat("-", 30))
		fmt.Printf("  Total Sessions:    %v\n", getValue(summary, "totalSessions"))
		fmt.Printf("  Total Events:      %v\n", getValue(summary, "totalEvents"))
		fmt.Printf("  Active Days:       %v\n", getValue(summary, "activeDays"))
		fmt.Printf("  Current Streak:    %v days\n", getValue(summary, "currentStreak"))
		fmt.Printf("  Longest Streak:    %v days\n", getValue(summary, "longestStreak"))
		fmt.Println()
	}

	// Comparison section
	if comparison, ok := data["comparison"].(map[string]interface{}); ok {
		fmt.Println("vs Previous Period")
		fmt.Println(strings.Repeat("-", 30))
		sessionsChange := getFloatValue(comparison, "sessionsChange")
		eventsChange := getFloatValue(comparison, "eventsChange")
		fmt.Printf("  Sessions:  %s\n", formatChange(sessionsChange))
		fmt.Printf("  Events:    %s\n", formatChange(eventsChange))
		fmt.Println()
	}

	// Tool mastery
	if tools, ok := data["toolMastery"].([]interface{}); ok && len(tools) > 0 {
		fmt.Println("Tool Mastery")
		fmt.Println(strings.Repeat("-", 30))
		for _, t := range tools {
			if tool, ok := t.(map[string]interface{}); ok {
				name := getStringValue(tool, "name")
				level := getStringValue(tool, "level")
				successRate := getFloatValue(tool, "successRate")
				icon := getMasteryIcon(level)
				fmt.Printf("  %s %-20s %s (%.0f%% success)\n", icon, name, level, successRate*100)
			}
		}
		fmt.Println()
	}

	// Quick tips based on data
	fmt.Println("Run 'promptconduit insights tools' for detailed tool breakdown")
	fmt.Println("Run 'promptconduit insights errors' to see error patterns")
	fmt.Println("Run 'promptconduit insights sessions' to see recent sessions")

	return nil
}

func outputToolsBreakdown(data map[string]interface{}) error {
	fmt.Println("Tool Usage Breakdown")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	period := getStringValue(data, "period")
	fmt.Printf("Period: %s\n\n", period)

	if tools, ok := data["tools"].([]interface{}); ok {
		if len(tools) == 0 {
			fmt.Println("No tool usage data found for this period.")
			return nil
		}

		// Header
		fmt.Printf("%-25s %8s %8s %10s\n", "Tool", "Uses", "Success", "Rate")
		fmt.Println(strings.Repeat("-", 55))

		for _, t := range tools {
			if tool, ok := t.(map[string]interface{}); ok {
				name := getStringValue(tool, "name")
				totalUses := getValue(tool, "totalUses")
				successCount := getValue(tool, "successCount")
				successRate := getFloatValue(tool, "successRate")

				// Truncate long names
				if len(name) > 24 {
					name = name[:21] + "..."
				}

				fmt.Printf("%-25s %8v %8v %9.0f%%\n", name, totalUses, successCount, successRate*100)
			}
		}
	}

	fmt.Println()
	return nil
}

func outputErrorPatterns(data map[string]interface{}) error {
	fmt.Println("Error Patterns")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	period := getStringValue(data, "period")
	fmt.Printf("Period: %s\n\n", period)

	if errors, ok := data["errors"].([]interface{}); ok {
		if len(errors) == 0 {
			fmt.Println("No error patterns found - great job!")
			return nil
		}

		for i, e := range errors {
			if errPattern, ok := e.(map[string]interface{}); ok {
				pattern := getStringValue(errPattern, "pattern")
				count := getValue(errPattern, "count")
				lastSeen := getStringValue(errPattern, "lastSeen")

				fmt.Printf("%d. %s\n", i+1, pattern)
				fmt.Printf("   Occurrences: %v | Last seen: %s\n\n", count, formatTimestamp(lastSeen))
			}
		}
	}

	return nil
}

func outputSessionsList(data map[string]interface{}) error {
	fmt.Println("Recent Sessions")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()

	if sessions, ok := data["sessions"].([]interface{}); ok {
		if len(sessions) == 0 {
			fmt.Println("No sessions found.")
			return nil
		}

		for _, s := range sessions {
			if session, ok := s.(map[string]interface{}); ok {
				id := getStringValue(session, "id")
				tool := getStringValue(session, "tool")
				repo := getStringValue(session, "repoName")
				startedAt := getStringValue(session, "startedAt")
				eventCount := getValue(session, "eventCount")

				// Format display
				shortID := id
				if len(id) > 8 {
					shortID = id[:8]
				}

				repoDisplay := repo
				if repoDisplay == "" {
					repoDisplay = "(no repo)"
				}

				fmt.Printf("%s  %-12s  %-25s  %v events  %s\n",
					shortID, tool, repoDisplay, eventCount, formatTimestamp(startedAt))
			}
		}
	}

	// Pagination info
	if total, ok := data["total"].(float64); ok {
		fmt.Printf("\nShowing %v of %.0f total sessions\n", insightsLimit, total)
	}

	return nil
}

// Helper functions

func getValue(data map[string]interface{}, key string) interface{} {
	if v, ok := data[key]; ok {
		return v
	}
	return 0
}

func getStringValue(data map[string]interface{}, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func getFloatValue(data map[string]interface{}, key string) float64 {
	if v, ok := data[key].(float64); ok {
		return v
	}
	return 0
}

func formatChange(change float64) string {
	if change > 0 {
		return fmt.Sprintf("+%.0f%% increase", change*100)
	} else if change < 0 {
		return fmt.Sprintf("%.0f%% decrease", change*100)
	}
	return "No change"
}

func getMasteryIcon(level string) string {
	switch level {
	case "master":
		return "[*]"
	case "growing":
		return "[+]"
	case "needs_attention":
		return "[!]"
	default:
		return "[ ]"
	}
}

func formatTimestamp(ts string) string {
	if ts == "" {
		return "-"
	}
	// Return just the date portion for display
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}
