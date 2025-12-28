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
	scanner.Buffer(buf, 10*1024*1024) // 10MB max line

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
			msg := parseUserMessage(raw, sequence, timestamp)
			if msg != nil {
				messages = append(messages, *msg)
				sequence++
			}

		case "assistant":
			msg := parseAssistantMessage(raw, sequence, timestamp, primaryModelCounts)
			if msg != nil {
				messages = append(messages, *msg)
				sequence++
			}

		case "file-history-snapshot":
			msg := parseFileSnapshot(raw, sequence, timestamp)
			if msg != nil {
				messages = append(messages, *msg)
				sequence++
			}

		case "queue-operation":
			// Skip queue operations
			continue

		default:
			// Handle other types generically
			msg := parseGenericMessage(raw, msgType, sequence, timestamp)
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

func parseUserMessage(raw map[string]json.RawMessage, sequence int, timestamp string) *ParsedMessage {
	var content string
	var uuid string
	var parentUUID string
	var cwd string

	if uuidRaw, ok := raw["uuid"]; ok {
		json.Unmarshal(uuidRaw, &uuid)
	}
	if parentRaw, ok := raw["parentUuid"]; ok {
		json.Unmarshal(parentRaw, &parentUUID)
	}
	if cwdRaw, ok := raw["cwd"]; ok {
		json.Unmarshal(cwdRaw, &cwd)
	}

	// Try to extract content from message.content array
	if msgRaw, ok := raw["message"]; ok {
		var msg struct {
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msgRaw, &msg); err == nil {
			for _, c := range msg.Content {
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

	// Fallback: try direct content field
	if content == "" {
		if contentRaw, ok := raw["content"]; ok {
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

	if uuid == "" {
		uuid = fmt.Sprintf("user-%d", sequence)
	}

	return &ParsedMessage{
		UUID:           uuid,
		ParentUUID:     parentUUID,
		Type:           "user",
		Role:           "user",
		Content:        content,
		Timestamp:      timestamp,
		SequenceNumber: sequence,
		Cwd:            cwd,
	}
}

func parseAssistantMessage(raw map[string]json.RawMessage, sequence int, timestamp string, modelCounts map[string]int) *ParsedMessage {
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
	}
}

func parseFileSnapshot(raw map[string]json.RawMessage, sequence int, timestamp string) *ParsedMessage {
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
	}
}

func parseGenericMessage(raw map[string]json.RawMessage, msgType string, sequence int, timestamp string) *ParsedMessage {
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
	}
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
