package sync

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ClaudeCodeParser parses Claude Code transcript files
type ClaudeCodeParser struct {
	homeDir string
}

// NewClaudeCodeParser creates a new Claude Code parser
func NewClaudeCodeParser() (*ClaudeCodeParser, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return &ClaudeCodeParser{homeDir: homeDir}, nil
}

func (p *ClaudeCodeParser) GetToolName() string {
	return "claude-code"
}

// GetTranscriptPaths returns all transcript file paths sorted by modification time (newest first)
func (p *ClaudeCodeParser) GetTranscriptPaths() ([]string, error) {
	projectsDir := filepath.Join(p.homeDir, ".claude", "projects")

	// Check if projects directory exists
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		return nil, nil // No transcripts yet
	}

	var files []string
	err := filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip files with errors
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk projects directory: %w", err)
	}

	// Sort by modification time (newest first)
	sort.Slice(files, func(i, j int) bool {
		infoI, errI := os.Stat(files[i])
		infoJ, errJ := os.Stat(files[j])
		if errI != nil || errJ != nil {
			return false
		}
		return infoI.ModTime().After(infoJ.ModTime())
	})

	return files, nil
}

// ParseFile parses a single Claude Code transcript file
func (p *ClaudeCodeParser) ParseFile(path string) (*ParsedConversation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Calculate file hash
	hash, err := calculateFileHash(path)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate hash: %w", err)
	}

	// Extract session ID from filename
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	// Parse messages
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 100*1024*1024) // 100MB max line - some tool outputs are very large

	var messages []ParsedMessage
	var summary string
	var title string
	var firstTimestamp, lastTimestamp string
	var primaryModelCounts = make(map[string]int)
	var cliVersion string
	var workingDirectory string
	var repoName, branch string
	sequence := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Preserve raw JSON for server-side categorization
		rawJSON := string(line)

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}

		// Get type
		var msgType string
		if typeRaw, ok := raw["type"]; ok {
			json.Unmarshal(typeRaw, &msgType)
		}

		// Get timestamp
		var timestamp string
		if tsRaw, ok := raw["timestamp"]; ok {
			json.Unmarshal(tsRaw, &timestamp)
		}
		if timestamp == "" {
			timestamp = time.Now().UTC().Format(time.RFC3339)
		}

		// Track first/last timestamps
		if firstTimestamp == "" {
			firstTimestamp = timestamp
		}
		lastTimestamp = timestamp

		// Parse based on type
		switch msgType {
		case "summary":
			if summaryRaw, ok := raw["summary"]; ok {
				json.Unmarshal(summaryRaw, &summary)
			}
			if leafRaw, ok := raw["leafTitle"]; ok {
				json.Unmarshal(leafRaw, &title)
			}
			continue // Don't add summary as a message

		case "user":
			msg := parseUserMessage(raw, sequence, timestamp, rawJSON)
			if msg != nil {
				messages = append(messages, *msg)
				sequence++
			}

		case "assistant":
			msg := parseAssistantMessage(raw, sequence, timestamp, primaryModelCounts, rawJSON)
			if msg != nil {
				messages = append(messages, *msg)
				sequence++
			}

		case "file-history-snapshot":
			msg := parseFileSnapshot(raw, sequence, timestamp, rawJSON)
			if msg != nil {
				messages = append(messages, *msg)
				sequence++
			}

		case "queue-operation":
			// Skip queue operations
			continue

		default:
			// Handle other types generically
			msg := parseGenericMessage(raw, msgType, sequence, timestamp, rawJSON)
			if msg != nil {
				messages = append(messages, *msg)
				sequence++
			}
		}

		// Extract context from any message that has it
		if cwdRaw, ok := raw["cwd"]; ok && workingDirectory == "" {
			json.Unmarshal(cwdRaw, &workingDirectory)
		}
		if versionRaw, ok := raw["cliVersion"]; ok && cliVersion == "" {
			json.Unmarshal(versionRaw, &cliVersion)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	// Determine primary model
	var primaryModel string
	var maxCount int
	for model, count := range primaryModelCounts {
		if count > maxCount {
			maxCount = count
			primaryModel = model
		}
	}

	// Extract repo name from working directory or path
	if workingDirectory != "" {
		repoName = filepath.Base(workingDirectory)
	} else {
		// Try to extract from path (e.g., .claude/projects/project-name/session.jsonl)
		parts := strings.Split(filepath.Dir(path), string(filepath.Separator))
		if len(parts) > 0 {
			repoName = parts[len(parts)-1]
		}
	}

	// Use summary as title if no specific title
	if title == "" && summary != "" {
		title = summary
		if len(title) > 100 {
			title = title[:100] + "..."
		}
	}

	return &ParsedConversation{
		SessionID:        sessionID,
		Tool:             "claude-code",
		Title:            title,
		Summary:          summary,
		StartedAt:        firstTimestamp,
		EndedAt:          lastTimestamp,
		RepoName:         repoName,
		Branch:           branch,
		WorkingDirectory: workingDirectory,
		PrimaryModel:     primaryModel,
		CLIVersion:       cliVersion,
		SourceFilePath:   path,
		SourceFileHash:   hash,
		Messages:         messages,
	}, nil
}

func parseUserMessage(raw map[string]json.RawMessage, sequence int, timestamp string, rawJSON string) *ParsedMessage {
	var content string
	var uuid string
	var parentUUID string
	var cwd string
	var toolUseID string
	isToolResult := false

	if uuidRaw, ok := raw["uuid"]; ok {
		json.Unmarshal(uuidRaw, &uuid)
	}
	if parentRaw, ok := raw["parentUuid"]; ok {
		json.Unmarshal(parentRaw, &parentUUID)
	}
	if cwdRaw, ok := raw["cwd"]; ok {
		json.Unmarshal(cwdRaw, &cwd)
	}

	// Try to extract content from message.content
	if msgRaw, ok := raw["message"]; ok {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msgRaw, &msg); err == nil && msg.Content != nil {
			// First, try parsing content as a string (user prompts)
			var contentStr string
			if err := json.Unmarshal(msg.Content, &contentStr); err == nil {
				content = contentStr
			} else {
				// If not a string, try parsing as an array (tool results, text blocks)
				var contentArray []json.RawMessage
				if err := json.Unmarshal(msg.Content, &contentArray); err == nil {
					for _, c := range contentArray {
						var item struct {
							Type      string          `json:"type"`
							Text      string          `json:"text"`
							Content   json.RawMessage `json:"content"`
							ToolUseID string          `json:"tool_use_id"`
						}
						if err := json.Unmarshal(c, &item); err == nil {
							switch item.Type {
							case "text":
								content = item.Text
							case "tool_result":
								isToolResult = true
								toolUseID = item.ToolUseID
								// tool_result content can be string or array
								var toolContent string
								if err := json.Unmarshal(item.Content, &toolContent); err == nil {
									content = toolContent
								} else {
									// Try as array of text objects
									var textArray []struct {
										Type string `json:"type"`
										Text string `json:"text"`
									}
									if err := json.Unmarshal(item.Content, &textArray); err == nil {
										for _, t := range textArray {
											if t.Type == "text" {
												content = t.Text
												break
											}
										}
									}
								}
							}
							if content != "" {
								break
							}
						}
					}
				}
			}
		}
	}

	// Fallback: try direct content field
	if content == "" {
		if contentRaw, ok := raw["content"]; ok {
			// Try as string first
			var contentStr string
			if err := json.Unmarshal(contentRaw, &contentStr); err == nil {
				content = contentStr
			} else {
				// Try as array
				var contentArray []json.RawMessage
				if err := json.Unmarshal(contentRaw, &contentArray); err == nil {
					for _, c := range contentArray {
						var textContent struct {
							Type string `json:"type"`
							Text string `json:"text"`
						}
						if err := json.Unmarshal(c, &textContent); err == nil && textContent.Type == "text" {
							content = textContent.Text
							break
						}
					}
				}
			}
		}
	}

	// Determine message type: "user" for human prompts, "tool_result" for tool responses
	msgType := "user"
	role := "user"
	if isToolResult {
		msgType = "tool_result"
		role = "tool"
	}

	if uuid == "" {
		uuid = fmt.Sprintf("%s-%d", msgType, sequence)
	}

	return &ParsedMessage{
		UUID:           uuid,
		ParentUUID:     parentUUID,
		Type:           msgType,
		Role:           role,
		Content:        content,
		ToolUseID:      toolUseID,
		Timestamp:      timestamp,
		SequenceNumber: sequence,
		Cwd:            cwd,
		RawJSON:        rawJSON,
	}
}

func parseAssistantMessage(raw map[string]json.RawMessage, sequence int, timestamp string, modelCounts map[string]int, rawJSON string) *ParsedMessage {
	var content string
	var thinking string
	var model string
	var uuid string
	var parentUUID string
	var toolName string
	var toolUseID string
	var toolInput string

	if uuidRaw, ok := raw["uuid"]; ok {
		json.Unmarshal(uuidRaw, &uuid)
	}
	if parentRaw, ok := raw["parentUuid"]; ok {
		json.Unmarshal(parentRaw, &parentUUID)
	}

	// Get model from message or costUSD.modelId
	if msgRaw, ok := raw["message"]; ok {
		var msg struct {
			Model   string            `json:"model"`
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msgRaw, &msg); err == nil {
			model = msg.Model
			modelCounts[model]++

			// Parse content
			for _, c := range msg.Content {
				var contentItem struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
					Name     string `json:"name"`
					ID       string `json:"id"`
					Input    json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(c, &contentItem); err == nil {
					switch contentItem.Type {
					case "text":
						content += contentItem.Text
					case "thinking":
						thinking = contentItem.Thinking
					case "tool_use":
						toolName = contentItem.Name
						toolUseID = contentItem.ID
						if contentItem.Input != nil {
							toolInput = string(contentItem.Input)
						}
					}
				}
			}
		}
	}

	if uuid == "" {
		uuid = fmt.Sprintf("assistant-%d", sequence)
	}

	return &ParsedMessage{
		UUID:           uuid,
		ParentUUID:     parentUUID,
		Type:           "assistant",
		Role:           "assistant",
		Content:        content,
		Model:          model,
		Thinking:       thinking,
		ToolName:       toolName,
		ToolUseID:      toolUseID,
		ToolInput:      toolInput,
		Timestamp:      timestamp,
		SequenceNumber: sequence,
		RawJSON:        rawJSON,
	}
}

func parseFileSnapshot(raw map[string]json.RawMessage, sequence int, timestamp string, rawJSON string) *ParsedMessage {
	var uuid string
	if uuidRaw, ok := raw["uuid"]; ok {
		json.Unmarshal(uuidRaw, &uuid)
	}
	if uuid == "" {
		uuid = fmt.Sprintf("snapshot-%d", sequence)
	}

	return &ParsedMessage{
		UUID:           uuid,
		Type:           "file_snapshot",
		Timestamp:      timestamp,
		SequenceNumber: sequence,
		RawJSON:        rawJSON,
	}
}

func parseGenericMessage(raw map[string]json.RawMessage, msgType string, sequence int, timestamp string, rawJSON string) *ParsedMessage {
	var uuid string
	var content string

	if uuidRaw, ok := raw["uuid"]; ok {
		json.Unmarshal(uuidRaw, &uuid)
	}
	if uuid == "" {
		uuid = fmt.Sprintf("%s-%d", msgType, sequence)
	}

	// Try to get any text content
	if contentRaw, ok := raw["content"]; ok {
		json.Unmarshal(contentRaw, &content)
	}

	return &ParsedMessage{
		UUID:           uuid,
		Type:           msgType,
		Content:        content,
		Timestamp:      timestamp,
		SequenceNumber: sequence,
		RawJSON:        rawJSON,
	}
}

// FindTranscriptBySessionID locates the transcript file for a given session ID
// Claude Code transcript files are named with the session ID (e.g., ~/.claude/projects/foo/sessionid.jsonl)
func FindTranscriptBySessionID(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session ID is empty")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	projectsDir := filepath.Join(homeDir, ".claude", "projects")

	// Check if projects directory exists
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		return "", fmt.Errorf("projects directory does not exist: %s", projectsDir)
	}

	expectedFilename := sessionID + ".jsonl"
	var foundPath string

	err = filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip files with errors
		}
		if !info.IsDir() && filepath.Base(path) == expectedFilename {
			foundPath = path
			return filepath.SkipAll // Found it, stop walking
		}
		return nil
	})

	if err != nil {
		return "", fmt.Errorf("error searching for transcript: %w", err)
	}

	if foundPath == "" {
		return "", fmt.Errorf("transcript file not found for session ID: %s", sessionID)
	}

	return foundPath, nil
}

func calculateFileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// CalculateFileHash calculates the SHA256 hash of a file (exported for fast-path sync checks)
func CalculateFileHash(path string) (string, error) {
	return calculateFileHash(path)
}
