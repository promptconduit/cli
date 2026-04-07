package sync

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CopilotParser parses GitHub Copilot CLI transcript files from ~/.copilot/
type CopilotParser struct {
	homeDir string
}

// NewCopilotParser creates a new Copilot CLI parser.
func NewCopilotParser() (*CopilotParser, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &CopilotParser{homeDir: homeDir}, nil
}

func (p *CopilotParser) GetToolName() string { return "copilot" }

// GetTranscriptPaths returns Copilot CLI transcript files, newest first.
// Returns nil (no error) if ~/.copilot/ doesn't exist (tool not installed).
// Looks in session-state/ first, then history-session-state/ as fallback.
func (p *CopilotParser) GetTranscriptPaths() ([]string, error) {
	copilotDir := filepath.Join(p.homeDir, ".copilot")
	if _, err := os.Stat(copilotDir); os.IsNotExist(err) {
		return nil, nil
	}

	searchDirs := []string{
		filepath.Join(copilotDir, "session-state"),
		filepath.Join(copilotDir, "history-session-state"),
	}

	var files []string
	for _, dir := range searchDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".json") {
				files = append(files, path)
			}
			return nil
		})
	}

	sort.Slice(files, func(i, j int) bool {
		ii, ei := os.Stat(files[i])
		ij, ej := os.Stat(files[j])
		if ei != nil || ej != nil {
			return false
		}
		return ii.ModTime().After(ij.ModTime())
	})

	return files, nil
}

// ParseFile parses a single Copilot CLI transcript file.
// Copilot CLI stores sessions as JSONL with one message object per line.
func (p *CopilotParser) ParseFile(path string) (*ParsedConversation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hash, err := calculateFileHash(path)
	if err != nil {
		hash = ""
	}

	conv := &ParsedConversation{
		Tool:           "copilot",
		SourceFilePath: path,
		SourceFileHash: hash,
	}

	// Copilot CLI message schema:
	// {"type":"user","content":"...","timestamp":"...","sessionId":"..."}
	// {"type":"assistant","content":"...","timestamp":"..."}
	// Some versions use "role" instead of "type"
	type copilotMsg struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Timestamp string          `json:"timestamp"`
		SessionID string          `json:"sessionId"`
		Title     string          `json:"title"`
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)

	seq := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}

		var msg copilotMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		if msg.SessionID != "" && conv.SessionID == "" {
			conv.SessionID = msg.SessionID
		}
		if msg.Title != "" && conv.Title == "" {
			conv.Title = msg.Title
		}
		if msg.Timestamp != "" {
			if conv.StartedAt == "" {
				conv.StartedAt = msg.Timestamp
			}
			conv.EndedAt = msg.Timestamp
		}

		// Normalize role: Copilot may use either "type" or "role"
		role := msg.Type
		if role == "" {
			role = msg.Role
		}
		if role == "" {
			continue
		}

		content := extractTextContent(msg.Content)
		pm := ParsedMessage{
			Type:           role,
			Role:           role,
			Content:        content,
			Timestamp:      msg.Timestamp,
			SequenceNumber: seq,
			RawJSON:        line,
		}
		seq++

		conv.Messages = append(conv.Messages, pm)
	}

	if conv.SessionID == "" {
		conv.SessionID = filepath.Base(path)
	}

	return conv, nil
}

// ensure CopilotParser implements Parser at compile time
var _ Parser = (*CopilotParser)(nil)
