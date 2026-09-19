package sync

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// timestampRe extracts the first <timestamp>...</timestamp> value from Cursor
// user-message text. Cursor embeds the turn time in the user JSONL line rather
// than a top-level timestamp field.
var timestampRe = regexp.MustCompile(`<timestamp>([^<]+)</timestamp>`)

// CursorParser parses Cursor agent transcript files from
// ~/.cursor/projects/*/agent-transcripts/.
type CursorParser struct {
	homeDir string
}

// NewCursorParser creates a new Cursor transcript parser.
func NewCursorParser() (*CursorParser, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return &CursorParser{homeDir: homeDir}, nil
}

func (p *CursorParser) GetToolName() string {
	return "cursor"
}

// GetTranscriptPaths returns Cursor agent transcript files, newest first.
// Discovers parent session JSONL files and subagent JSONL files under
// ~/.cursor/projects/<slug>/agent-transcripts/. Returns nil (no error) if
// the projects directory does not exist.
func (p *CursorParser) GetTranscriptPaths() ([]string, error) {
	projectsDir := filepath.Join(p.homeDir, ".cursor", "projects")
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []string
	err := filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if !strings.Contains(filepath.ToSlash(path), "/agent-transcripts/") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk Cursor projects directory: %w", err)
	}

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

// ParseFile parses a single Cursor transcript file. Each non-empty JSONL line
// is kept as RawJSON for server-side categorization. Session ID is the
// filename without the .jsonl suffix. Timestamps come from <timestamp> tags
// in user text when present, otherwise the file modification time.
func (p *CursorParser) ParseFile(path string) (*ParsedConversation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	hash, err := calculateFileHash(path)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate hash: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}
	fallbackTS := info.ModTime().UTC().Format(time.RFC3339)

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 100*1024*1024)

	var messages []ParsedMessage
	var firstTimestamp, lastTimestamp string
	sequence := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		ts := extractCursorTimestamp(line)
		if ts == "" {
			ts = fallbackTS
		}
		if firstTimestamp == "" {
			firstTimestamp = ts
		}
		lastTimestamp = ts

		msgType, role := cursorLineTypeAndRole(line)
		uuid := fmt.Sprintf("%s-%d", msgType, sequence)

		messages = append(messages, ParsedMessage{
			UUID:           uuid,
			Type:           msgType,
			Role:           role,
			Timestamp:      ts,
			SequenceNumber: sequence,
			RawJSON:        line,
		})
		sequence++
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	return &ParsedConversation{
		SessionID:      sessionID,
		Tool:           "cursor",
		StartedAt:      firstTimestamp,
		EndedAt:        lastTimestamp,
		SourceFilePath: path,
		SourceFileHash: hash,
		Messages:       messages,
	}, nil
}

func extractCursorTimestamp(line string) string {
	m := timestampRe.FindStringSubmatch(line)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func cursorLineTypeAndRole(line string) (msgType, role string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return "unknown", ""
	}

	if typeRaw, ok := raw["type"]; ok {
		_ = json.Unmarshal(typeRaw, &msgType)
	}
	if roleRaw, ok := raw["role"]; ok {
		_ = json.Unmarshal(roleRaw, &role)
	}
	if msgType == "" {
		msgType = role
	}
	if msgType == "" {
		msgType = "unknown"
	}
	if role == "" {
		role = msgType
	}
	return msgType, role
}

// NewParserForPath returns the transcript parser for a file based on its path.
// Cursor agent transcripts live under ~/.cursor/projects/.../agent-transcripts/;
// everything else uses the Claude Code parser.
func NewParserForPath(path string) (Parser, error) {
	slash := filepath.ToSlash(path)
	if strings.Contains(slash, ".cursor/projects") && strings.Contains(slash, "agent-transcripts") {
		return NewCursorParser()
	}
	return NewClaudeCodeParser()
}

var _ Parser = (*CursorParser)(nil)
