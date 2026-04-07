package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/git"
	intsync "github.com/promptconduit/cli/internal/sync"
	"github.com/spf13/cobra"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	localSkillsStateFile      = "local_skills_state.json"
	localMinConversations     = 5
	localMaxConversations     = 50
	anthropicMessagesURL      = "https://api.anthropic.com/v1/messages"
	anthropicVersion          = "2023-06-01"
	openAIChatURL             = "https://api.openai.com/v1/chat/completions"
	localSkillsHaikuModel     = "claude-haiku-4-5-20251001"
	localSkillsOpenAIModel    = "gpt-4o-mini"
	localSkillsMaxTokens      = 8000
	localSkillsTemperature    = 0.2
	localSkillsHTTPTimeoutSec = 120
)

// ============================================================================
// TYPES
// ============================================================================

type localSkillsState struct {
	AnalyzedSessions map[string]string `json:"analyzed_sessions"` // hash → RFC3339
	LastAnalyzed     string            `json:"last_analyzed"`
}

type localDetectedSkill struct {
	Name           string  `json:"name"`
	DisplayName    string  `json:"displayName"`
	Description    string  `json:"description"`
	SkillType      string  `json:"skillType"`
	CommandContent string  `json:"commandContent"`
	TriggerPattern string  `json:"triggerPattern"`
	ExampleUsage   string  `json:"exampleUsage"`
	Confidence     float64 `json:"confidence"`
}

type localSkillDetectionResponse struct {
	Skills []localDetectedSkill `json:"skills"`
}

type convoSummary struct {
	Title            string
	Summary          string
	ToolUseCount     int
	UserMessageCount int
	UserPrompts      []string // first few user prompts for pattern detection
}

// ============================================================================
// SYSTEM / DETECTION PROMPT (ported from platform skillDetection.ts)
// ============================================================================

const localSystemPrompt = `You are an expert at analyzing AI coding assistant usage patterns and generating actionable Claude Code command files.

Your job is to identify workflows that appear multiple times across conversations, then generate proper Claude Code command files for them.

SKILL TYPES:
1. "workflow" - Multi-step executable processes (e.g., CI monitoring, deployment checks, refactoring pipelines)
   - Use when: the pattern involves a sequence of tool calls, checks, or iterations
   - Format: Step-by-step instructions with bash commands, conditionals, loops

2. "command" - Single-purpose automation (e.g., generate a commit message, create a PR)
   - Use when: the pattern is a well-defined, single-output task
   - Format: Direct instructions with clear inputs/outputs

3. "template" - Recurring structure or boilerplate (e.g., create a new component, scaffold a test)
   - Use when: the pattern produces a consistent artifact each time
   - Format: Template with placeholders and instructions

4. "checklist" - Review/audit patterns (e.g., UI/UX review, security audit, code review against a library)
   - Use when: the pattern involves checking things against known standards
   - Format: Checklist with criteria and examples

COMMAND FILE FORMAT (commandContent field):
Each command must be a complete, executable Claude Code command file using this format:

[Brief description of what this command does, 1-2 sentences]

## When to Use
[Describe what user prompts should trigger this command]

## Steps
[For workflow/command: numbered steps with actual commands]
[For template: the template structure]
[For checklist: the checklist items with criteria]

## Example
/skill-name

IMPORTANT CONSTRAINTS:
- Keep commandContent under 120 words total — be concise, cut prose, keep commands
- Include only the most critical bash commands in Steps (2-4 steps max)
- Skip verbose explanations — the steps themselves are the documentation`

func buildLocalDetectionPrompt(summaries []convoSummary, repo string) string {
	lines := make([]string, len(summaries))
	for i, s := range summaries {
		title := s.Title
		if title == "" {
			title = "Untitled"
		}
		summary := s.Summary
		if summary == "" {
			summary = "No summary"
		}
		line := fmt.Sprintf(`%d. "%s" — %s (%d tools, %d messages)`,
			i+1, title, summary, s.ToolUseCount, s.UserMessageCount)
		if len(s.UserPrompts) > 0 {
			line += "\n   User said: " + strings.Join(s.UserPrompts, " / ")
		}
		lines[i] = line
	}

	scopeNote := ""
	if repo != "" {
		scopeNote = fmt.Sprintf("\nPROJECT SCOPE: These conversations are from the %q repository. Generate skills specific to this project's patterns — use project-specific tool names, APIs, and conventions where relevant.\n", repo)
	}

	return fmt.Sprintf(`Analyze these %d conversations and identify 3-4 distinct workflow patterns that appear repeatedly.%s

CONVERSATIONS:
%s

For each skill, output:
- name: kebab-case (e.g., "ci-monitor", "ui-review")
- displayName: human readable (e.g., "CI Monitor", "UI/UX Review")
- description: one sentence
- skillType: "workflow" | "command" | "template" | "checklist"
- commandContent: the COMPLETE command file content (see format instructions)
- triggerPattern: what user prompts trigger this (be specific)
- exampleUsage: "/skill-name [args]"
- confidence: 0.0-1.0

OUTPUT JSON ONLY:
{"skills": [{"name":"ci-monitor","displayName":"CI Monitor","description":"Check CI status and fix failures until green","skillType":"workflow","commandContent":"Check CI pipeline status and fix failing jobs until green.\n\n## When to Use\nWhen CI is failing or you need the pipeline green before merging.\n\n## Steps\n\n1. Run: gh run list --limit 10\n2. For failures: gh run view RUN_ID --log-failed\n3. Fix errors, re-run: gh run rerun --failed RUN_ID\n4. Repeat until all checks pass.\n\n## Example\n/ci-monitor","triggerPattern":"Prompts like: 'how is CI doing?', 'check if CI is passing', 'fix the failing tests in CI'","exampleUsage":"/ci-monitor","confidence":0.9}]}`,
		len(summaries), scopeNote, strings.Join(lines, "\n"))
}

// ============================================================================
// ENTRY POINT
// ============================================================================

func runSkillsGenerateLocal(cmd *cobra.Command, args []string) error {
	// 1. Detect AI provider
	provider, err := detectLocalAIProvider()
	if err != nil {
		return err
	}

	// 2. Resolve repo scope (mirrors platform mode logic)
	repoFlag := cmd.Flags().Lookup("repo")
	repo := skillsRepo
	if !repoFlag.Changed {
		cwd, err := os.Getwd()
		if err == nil {
			repo = git.GetRepoName(cwd)
		}
	}
	cwd, _ := os.Getwd()
	gitRoot := detectGitRoot(cwd)

	// 3. Collect transcripts from all requested parsers
	parsers := getActiveParsers(skillsTool)
	if len(parsers) == 0 {
		return fmt.Errorf("no AI coding tool transcripts found\n\nInstall one of: Claude Code, OpenAI Codex CLI, GitHub Copilot CLI")
	}

	var allConvos []*intsync.ParsedConversation
	toolCounts := map[string]int{}

	for _, p := range parsers {
		paths, err := p.GetTranscriptPaths()
		if err != nil || len(paths) == 0 {
			continue
		}
		for _, path := range paths {
			conv, err := p.ParseFile(path)
			if err != nil || conv == nil {
				continue
			}
			allConvos = append(allConvos, conv)
		}
		toolCounts[p.GetToolName()] = len(paths)
	}

	if len(allConvos) == 0 {
		return fmt.Errorf("no transcripts found\n\nInstall Claude Code hooks first: promptconduit install claude-code")
	}

	// 4. Filter by repo if scoped
	if repo != "" {
		var filtered []*intsync.ParsedConversation
		for _, c := range allConvos {
			if c.RepoName == repo {
				filtered = append(filtered, c)
			}
		}
		allConvos = filtered
	}

	// Cap at max
	if len(allConvos) > localMaxConversations {
		allConvos = allConvos[:localMaxConversations]
	}

	// 5. Load state and filter already-analyzed (unless --force)
	state, _ := loadLocalSkillsState()
	var toAnalyze []*intsync.ParsedConversation
	if skillsForce {
		toAnalyze = allConvos
	} else {
		for _, c := range allConvos {
			if !isLocalTranscriptAnalyzed(state, c.SourceFileHash) {
				toAnalyze = append(toAnalyze, c)
			}
		}
	}

	if len(toAnalyze) == 0 {
		fmt.Println("No new transcripts to analyze.")
		if state.LastAnalyzed != "" {
			fmt.Printf("Last analyzed: %s\n", state.LastAnalyzed)
		}
		fmt.Println("\nUse --force to re-analyze all transcripts.")
		return nil
	}

	// 6. Minimum conversation guard
	if len(toAnalyze) < localMinConversations {
		if len(toAnalyze) > 0 {
			fmt.Printf("Only %d new transcript(s) since last analysis (need %d).\n", len(toAnalyze), localMinConversations)
			fmt.Println("Use --force to re-analyze all transcripts.")
			return nil
		}
		return fmt.Errorf("need at least %d conversations for skill detection, found %d\n\nUse Claude Code more, or run with --force to include previously analyzed sessions",
			localMinConversations, len(toAnalyze))
	}

	// 7. Progress header
	if repo != "" {
		fmt.Printf("Analyzing %d local transcripts (%s)...\n", len(toAnalyze), repo)
	} else {
		fmt.Printf("Analyzing %d local transcripts (global)...\n", len(toAnalyze))
	}
	if len(toolCounts) > 1 {
		for tool, count := range toolCounts {
			if count > 0 {
				fmt.Printf("  %s: %d sessions\n", tool, count)
			}
		}
	}
	fmt.Printf("Using %s.\n\n", providerLabel(provider))

	// 8. Detect skills via AI
	skills, err := callLocalSkillDetection(provider, toAnalyze, repo)
	if err != nil {
		return fmt.Errorf("skill detection failed: %w", err)
	}

	if len(skills) == 0 {
		fmt.Println("No skills detected in this batch.")
		return nil
	}

	// 9. Write SKILL.md files
	written, writeErrs := writeLocalSkills(skills, gitRoot, repo)

	// 10. Save state
	markLocalTranscriptsAnalyzed(state, toAnalyze)
	_ = saveLocalSkillsState(state)

	// 11. Output results
	outputLocalSkillsGenerated(skills, written, writeErrs, len(toAnalyze))

	return nil
}

// ============================================================================
// AI PROVIDER DETECTION
// ============================================================================

func detectLocalAIProvider() (string, error) {
	if _, err := exec.LookPath("claude"); err == nil {
		return "claude-cli", nil
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "anthropic", nil
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return "openai", nil
	}
	return "", fmt.Errorf(`no AI provider found. Options:

  1. Install Claude Code (uses your existing subscription):
       https://claude.ai/download

  2. Set ANTHROPIC_API_KEY (direct API access):
       export ANTHROPIC_API_KEY=sk-ant-...
       https://console.anthropic.com

  3. Set OPENAI_API_KEY (OpenAI fallback):
       export OPENAI_API_KEY=sk-...
       https://platform.openai.com`)
}

func providerLabel(provider string) string {
	switch provider {
	case "claude-cli":
		return "Claude Code (claude CLI)"
	case "anthropic":
		return "Anthropic API (ANTHROPIC_API_KEY)"
	case "openai":
		return "OpenAI API (OPENAI_API_KEY)"
	default:
		return provider
	}
}

// ============================================================================
// PARSER COLLECTION
// ============================================================================

func getActiveParsers(tool string) []intsync.Parser {
	all := tool == "all" || tool == ""

	var parsers []intsync.Parser

	if all || tool == "claude-code" {
		if p, err := intsync.NewClaudeCodeParser(); err == nil {
			parsers = append(parsers, p)
		}
	}
	if all || tool == "codex" {
		if p, err := intsync.NewCodexParser(); err == nil {
			parsers = append(parsers, p)
		}
	}
	if all || tool == "copilot" {
		if p, err := intsync.NewCopilotParser(); err == nil {
			parsers = append(parsers, p)
		}
	}

	return parsers
}

// ============================================================================
// AI CALLERS
// ============================================================================

func callLocalSkillDetection(provider string, convos []*intsync.ParsedConversation, repo string) ([]localDetectedSkill, error) {
	summaries := summarizeConvos(convos)
	userPrompt := buildLocalDetectionPrompt(summaries, repo)

	var responseText string
	var err error

	switch provider {
	case "claude-cli":
		responseText, err = callViaClaudeCLI(userPrompt)
	case "anthropic":
		responseText, err = callViaAnthropicAPI(os.Getenv("ANTHROPIC_API_KEY"), userPrompt)
	case "openai":
		responseText, err = callViaOpenAI(os.Getenv("OPENAI_API_KEY"), userPrompt)
	default:
		return nil, fmt.Errorf("unknown provider: %s", provider)
	}

	if err != nil {
		return nil, err
	}

	return parseLocalSkillResponse(responseText)
}

func summarizeConvos(convos []*intsync.ParsedConversation) []convoSummary {
	out := make([]convoSummary, len(convos))
	for i, c := range convos {
		toolCount := 0
		userMsgCount := 0
		var prompts []string
		for _, m := range c.Messages {
			if m.ToolName != "" || (m.Type == "assistant" && m.ToolUseID != "") {
				toolCount++
			} else if m.Type == "user" && m.ToolUseID == "" && m.Content != "" {
				userMsgCount++
				if len(prompts) < 3 {
					prompts = append(prompts, truncateString(m.Content, 120))
				}
			}
		}
		out[i] = convoSummary{
			Title:            c.Title,
			Summary:          c.Summary,
			ToolUseCount:     toolCount,
			UserMessageCount: userMsgCount,
			UserPrompts:      prompts,
		}
	}
	return out
}

// callViaClaudeCLI invokes the claude CLI as a subprocess in non-interactive mode.
func callViaClaudeCLI(userPrompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localSkillsHTTPTimeoutSec*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude",
		"--print",
		"--output-format", "json",
		"--model", "haiku",
		"--system-prompt", localSystemPrompt,
		"--no-session-persistence",
	)
	// Pass the prompt via stdin to avoid command-line length limits
	cmd.Stdin = strings.NewReader(userPrompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("claude CLI error: %s", errMsg)
	}

	// Parse the JSON envelope: {"type":"result","result":"...","is_error":false,...}
	var envelope struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		// Fallback: treat raw stdout as the response text
		return strings.TrimSpace(stdout.String()), nil
	}
	if envelope.IsError {
		return "", fmt.Errorf("claude CLI returned an error: %s", envelope.Result)
	}
	return envelope.Result, nil
}

// callViaAnthropicAPI calls the Anthropic Messages API directly.
func callViaAnthropicAPI(apiKey, userPrompt string) (string, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]interface{}{
		"model":       localSkillsHaikuModel,
		"max_tokens":  localSkillsMaxTokens,
		"temperature": localSkillsTemperature,
		"system":      localSystemPrompt,
		"messages":    []message{{Role: "user", Content: userPrompt}},
	}
	return doAnthropicRequest(apiKey, body)
}

func doAnthropicRequest(apiKey string, body interface{}) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), localSkillsHTTPTimeoutSec*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Anthropic API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("Anthropic API HTTP %d: %s", resp.StatusCode, string(respBody[:min(200, len(respBody))]))
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse Anthropic response: %w", err)
	}
	if result.Error != nil {
		return "", fmt.Errorf("Anthropic API error (%s): %s", result.Error.Type, result.Error.Message)
	}
	for _, c := range result.Content {
		if c.Type == "text" {
			return c.Text, nil
		}
	}
	return "", fmt.Errorf("Anthropic API returned no text content")
}

// callViaOpenAI calls the OpenAI Chat Completions API.
func callViaOpenAI(apiKey, userPrompt string) (string, error) {
	body := map[string]interface{}{
		"model":       localSkillsOpenAIModel,
		"max_tokens":  localSkillsMaxTokens,
		"temperature": localSkillsTemperature,
		"messages": []map[string]string{
			{"role": "system", "content": localSystemPrompt},
			{"role": "user", "content": userPrompt},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), localSkillsHTTPTimeoutSec*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIChatURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenAI API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("OpenAI API HTTP %d: %s", resp.StatusCode, string(respBody[:min(200, len(respBody))]))
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse OpenAI response: %w", err)
	}
	if result.Error != nil {
		return "", fmt.Errorf("OpenAI API error: %s", result.Error.Message)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("OpenAI API returned no choices")
	}
	return result.Choices[0].Message.Content, nil
}

// ============================================================================
// RESPONSE PARSER
// ============================================================================

var codeFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

func parseLocalSkillResponse(text string) ([]localDetectedSkill, error) {
	text = strings.TrimSpace(text)

	// Strip code fences if present
	if m := codeFenceRe.FindStringSubmatch(text); len(m) > 1 {
		text = strings.TrimSpace(m[1])
	}

	// Find JSON object boundaries
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("no JSON object found in response (got: %s)", text[:min(200, len(text))])
	}
	text = text[start : end+1]

	var parsed localSkillDetectionResponse
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse skill JSON: %w (snippet: %s)", err, text[:min(300, len(text))])
	}

	validTypes := map[string]bool{"workflow": true, "command": true, "template": true, "checklist": true}
	kebabRe := regexp.MustCompile(`[^a-z0-9-]`)

	var out []localDetectedSkill
	for _, s := range parsed.Skills {
		if s.Name == "" || s.DisplayName == "" || s.Description == "" || s.CommandContent == "" {
			continue
		}
		s.Name = kebabRe.ReplaceAllString(strings.ToLower(s.Name), "-")
		if !validTypes[s.SkillType] {
			s.SkillType = "workflow"
		}
		if s.Confidence < 0 {
			s.Confidence = 0
		}
		if s.Confidence > 1 {
			s.Confidence = 1
		}
		if s.ExampleUsage == "" {
			s.ExampleUsage = "/" + s.Name
		}
		out = append(out, s)
	}
	return out, nil
}

// ============================================================================
// SKILL FILE WRITER
// ============================================================================

// writeLocalSkills writes each skill as a SKILL.md file in the new skills/ format.
// Global skills → ~/.claude/skills/<name>/SKILL.md
// Project skills → <gitRoot>/.claude/skills/<name>/SKILL.md
// Returns a map of skill name → written path so callers can look up by name.
func writeLocalSkills(skills []localDetectedSkill, gitRoot, repo string) (written map[string]string, errs []error) {
	written = map[string]string{}
	home, err := os.UserHomeDir()
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to get home dir: %w", err))
		return
	}

	for _, skill := range skills {
		var targetDir string
		if gitRoot != "" && repo != "" {
			targetDir = filepath.Join(gitRoot, ".claude", "skills", skill.Name)
		} else {
			targetDir = filepath.Join(home, ".claude", "skills", skill.Name)
		}

		if err := os.MkdirAll(targetDir, 0755); err != nil {
			errs = append(errs, fmt.Errorf("failed to create dir for %s: %w", skill.Name, err))
			continue
		}

		dest := filepath.Join(targetDir, "SKILL.md")
		content := formatLocalSkillFile(skill)
		if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
			errs = append(errs, fmt.Errorf("failed to write %s: %w", dest, err))
			continue
		}
		written[skill.Name] = dest
	}
	return
}

func formatLocalSkillFile(s localDetectedSkill) string {
	var sb strings.Builder

	// YAML frontmatter
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", s.Name))
	// description = one sentence + trigger context for Claude auto-loading
	desc := s.Description
	if s.TriggerPattern != "" {
		desc = s.Description + " " + s.TriggerPattern
	}
	sb.WriteString(fmt.Sprintf("description: %q\n", desc))
	sb.WriteString("---\n\n")

	// Body: commandContent + attribution
	sb.WriteString(s.CommandContent)
	sb.WriteString("\n\n---\n")
	sb.WriteString(fmt.Sprintf("*Generated locally by PromptConduit · Confidence: %d%%*\n",
		int(s.Confidence*100)))

	return sb.String()
}

// ============================================================================
// STATE MANAGEMENT
// ============================================================================

func localSkillsStatePath() string {
	dir := client.ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, localSkillsStateFile)
}

func loadLocalSkillsState() (*localSkillsState, error) {
	path := localSkillsStatePath()
	if path == "" {
		return &localSkillsState{AnalyzedSessions: map[string]string{}}, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &localSkillsState{AnalyzedSessions: map[string]string{}}, nil
	}
	if err != nil {
		return &localSkillsState{AnalyzedSessions: map[string]string{}}, err
	}
	var s localSkillsState
	if err := json.Unmarshal(data, &s); err != nil {
		return &localSkillsState{AnalyzedSessions: map[string]string{}}, nil
	}
	if s.AnalyzedSessions == nil {
		s.AnalyzedSessions = map[string]string{}
	}
	return &s, nil
}

func saveLocalSkillsState(s *localSkillsState) error {
	path := localSkillsStatePath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func isLocalTranscriptAnalyzed(s *localSkillsState, hash string) bool {
	if hash == "" || s == nil {
		return false
	}
	_, ok := s.AnalyzedSessions[hash]
	return ok
}

func markLocalTranscriptsAnalyzed(s *localSkillsState, convos []*intsync.ParsedConversation) {
	now := time.Now().UTC().Format(time.RFC3339)
	for _, c := range convos {
		if c.SourceFileHash != "" {
			s.AnalyzedSessions[c.SourceFileHash] = now
		}
	}
	s.LastAnalyzed = now
}

// ============================================================================
// OUTPUT
// ============================================================================

func outputLocalSkillsGenerated(skills []localDetectedSkill, written map[string]string, errs []error, convCount int) {
	fmt.Printf("Detected %d skills:\n\n", len(skills))

	for _, s := range skills {
		fmt.Printf("  /%s  (%s, %d%%)\n", s.Name, s.SkillType, int(s.Confidence*100))
		fmt.Printf("    %s\n", s.DisplayName)
		fmt.Printf("    %s\n", truncateString(s.Description, 80))
		if path, ok := written[s.Name]; ok {
			fmt.Printf("    → Written to: %s\n", path)
		}
		fmt.Println()
	}

	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  warning: %s\n", e)
		}
		fmt.Println()
	}

	successCount := len(written)
	if successCount > 0 && len(skills) > 0 {
		fmt.Printf("%d skill%s written. Use them with /%s, etc.\n",
			successCount, pluralS(successCount), skills[0].Name)
		fmt.Println("Review and delete ~/.claude/skills/<name>/ for any you don't want.")
	} else {
		fmt.Println("No skills written.")
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

