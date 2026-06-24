package cost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// claudeProjectsDir is where Claude Code stores per-project transcript folders.
func claudeProjectsDir(homeDir string) string {
	return filepath.Join(homeDir, ".claude", "projects")
}

// ResolveDirs returns the transcript directories to watch. When all is true it
// returns every Claude Code project folder; otherwise just the folder for cwd.
func ResolveDirs(cwd string, all bool) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := claudeProjectsDir(home)
	if all {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		var dirs []string
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
		return dirs, nil
	}
	return []string{projectDirForCwd(home, cwd)}, nil
}

// encodeProjectPath mirrors Claude Code's project-folder naming: every
// non-alphanumeric character in the absolute cwd becomes a dash. e.g.
// /Users/x/GitHub/tolken -> -Users-x-GitHub-tolken. Used to scope the watcher
// to a single workspace without reading every transcript.
func encodeProjectPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// projectDirForCwd resolves the transcript directory for a workspace. It tries
// the deterministic encoding first; if that folder doesn't exist (an edge case
// in Claude Code's encoding), it falls back to scanning every project folder
// and matching the cwd recorded inside a transcript.
func projectDirForCwd(homeDir, cwd string) string {
	root := claudeProjectsDir(homeDir)
	encoded := filepath.Join(root, encodeProjectPath(cwd))
	if fi, err := os.Stat(encoded); err == nil && fi.IsDir() {
		return encoded
	}
	if match := scanForCwd(root, cwd); match != "" {
		return match
	}
	return encoded // best-effort: return the computed path even if absent yet
}

// scanForCwd walks project folders and returns the one whose newest transcript
// records the given cwd. Bounded I/O — reads only the first line of the newest
// transcript per folder.
func scanForCwd(root, cwd string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files := jsonlFiles(dir)
		if len(files) == 0 {
			continue
		}
		if firstLineCwd(files[0]) == cwd {
			return dir
		}
	}
	return ""
}

// firstLineCwd reads the cwd field from the first JSON line of a transcript.
func firstLineCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	line := buf[:n]
	if idx := indexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	var probe struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(line, &probe)
	return probe.Cwd
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// jsonlFiles returns the .jsonl files in dir sorted newest-first by mtime.
func jsonlFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type fileMod struct {
		path string
		mod  int64
	}
	var fms []fileMod
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fms = append(fms, fileMod{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	sort.Slice(fms, func(i, j int) bool { return fms[i].mod > fms[j].mod })
	out := make([]string, len(fms))
	for i, fm := range fms {
		out[i] = fm.path
	}
	return out
}

// ccLine is the subset of a Claude Code transcript line the cost watcher reads.
type ccLine struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	UUID      string `json:"uuid"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            struct {
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// parseClaudeCodeLine turns one transcript line into a priced CostEvent. The
// returned dedupKey (requestId, falling back to uuid) lets the caller skip
// re-reads. ok is false for non-assistant lines, lines without usage, or
// zero-token lines (which carry no cost).
func parseClaudeCodeLine(line []byte, table *PriceTable, sessionFallback string) (ev CostEvent, dedupKey string, ok bool) {
	var l ccLine
	if err := json.Unmarshal(line, &l); err != nil {
		return CostEvent{}, "", false
	}
	if l.Type != "assistant" {
		return CostEvent{}, "", false
	}
	u := l.Message.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		return CostEvent{}, "", false
	}

	dedupKey = l.RequestID
	if dedupKey == "" {
		dedupKey = l.UUID
	}

	usage := Usage{
		InputTokens:          u.InputTokens,
		OutputTokens:         u.OutputTokens,
		CacheReadInputTokens: u.CacheReadInputTokens,
		CacheCreationInput:   u.CacheCreationInputTokens,
		Ephemeral5mTokens:    u.CacheCreation.Ephemeral5m,
		Ephemeral1hTokens:    u.CacheCreation.Ephemeral1h,
	}

	mp, priced := table.ResolvePrice(l.Message.Model)
	c, cacheWriteTokens := CostForUsage(usage, mp)

	sessionID := l.SessionID
	if sessionID == "" {
		sessionID = sessionFallback
	}

	ev = CostEvent{
		V:           SchemaVersion,
		Kind:        "cost_event",
		Tool:        ToolClaudeCode,
		SessionID:   sessionID,
		RequestID:   dedupKey,
		Timestamp:   l.Timestamp,
		Model:       l.Message.Model,
		ModelPriced: priced,
		Source:      SourceExact,
		Tokens: Tokens{
			Input:      u.InputTokens,
			Output:     u.OutputTokens,
			CacheRead:  u.CacheReadInputTokens,
			CacheWrite: cacheWriteTokens,
		},
		Cost:    c,
		CwdBase: filepath.Base(l.Cwd),
	}
	return ev, dedupKey, true
}

// sessionIDFromPath derives the session id from a transcript filename
// (Claude Code names files <session-uuid>.jsonl).
func sessionIDFromPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}
