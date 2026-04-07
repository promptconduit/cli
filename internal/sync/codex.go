package sync

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CodexParser parses OpenAI Codex CLI transcript files from ~/.codex/
type CodexParser struct {
	homeDir string
}

// NewCodexParser creates a new Codex CLI parser.
func NewCodexParser() (*CodexParser, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &CodexParser{homeDir: homeDir}, nil
}

func (p *CodexParser) GetToolName() string { return "codex" }

// GetTranscriptPaths returns all Codex transcript file paths, newest first.
// Returns nil (no error) if ~/.codex/ doesn't exist (tool not installed).
func (p *CodexParser) GetTranscriptPaths() ([]string, error) {
	codexDir := filepath.Join(p.homeDir, ".codex")
	if _, err := os.Stat(codexDir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []string
	_ = filepath.Walk(codexDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".json") {
			files = append(files, path)
		}
		return nil
	})

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

// ParseFile parses a single Codex transcript file.
// Codex uses a JSONL format where each line is a message object.
// Field names follow the OpenAI chat completions schema.
func (p *CodexParser) ParseFile(path string) (*ParsedConversation, error) {
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
		Tool:           "codex",
		SourceFilePath: path,
		SourceFileHash: hash,
		RepoName:       repoNameFromPath(path),
	}

	// Codex message schema (subset we care about):
	// {"role":"user","content":"...","timestamp":"..."}
	// {"role":"assistant","content":"...","timestamp":"..."}
	// OR OpenAI API format:
	// {"type":"message","role":"user","content":[{"type":"text","text":"..."}]}
	type codexMsg struct {
		Role      string          `json:"role"`
		Type      string          `json:"type"`
		Content   json.RawMessage `json:"content"`
		Timestamp string          `json:"timestamp"`
		// session-level fields that sometimes appear on first line
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
		Summary   string `json:"summary"`
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)

	seq := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}

		var msg codexMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		// Capture session metadata from any line that has it
		if msg.SessionID != "" && conv.SessionID == "" {
			conv.SessionID = msg.SessionID
		}
		if msg.Title != "" && conv.Title == "" {
			conv.Title = msg.Title
		}
		if msg.Summary != "" && conv.Summary == "" {
			conv.Summary = msg.Summary
		}

		if msg.Timestamp != "" {
			if conv.StartedAt == "" {
				conv.StartedAt = msg.Timestamp
			}
			conv.EndedAt = msg.Timestamp
		}

		if msg.Role == "" {
			continue
		}

		content := extractTextContent(msg.Content)
		pm := ParsedMessage{
			Type:           msg.Role,
			Role:           msg.Role,
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

// extractTextContent pulls plain text from a Codex content field,
// which can be either a plain string or an array of content blocks.
func extractTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try plain string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of blocks: [{"type":"text","text":"..."}]
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// repoNameFromPath extracts a best-effort repo name from the transcript path.
// Codex doesn't embed repo metadata, so we infer from the working directory
// path embedded in the file path (similar to Claude Code's project dir naming).
func repoNameFromPath(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(dir)
	// Strip leading dashes from path-encoded directory names
	base = strings.TrimLeft(base, "-")
	if strings.Contains(base, "-") {
		parts := strings.Split(base, "-")
		if len(parts) >= 2 {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1]
		}
	}
	return ""
}

// ensure CodexParser implements Parser at compile time
var _ Parser = (*CodexParser)(nil)

// ensure time is used (for future timestamp parsing)
var _ = time.Now
