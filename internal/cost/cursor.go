package cost

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Cursor turns out to be an EXACT source, not an estimated one: its `stop` and
// `afterAgentResponse` hooks carry `input_tokens`, `output_tokens`,
// `cache_read_tokens`, `cache_write_tokens`, and `model` directly (verified
// 2026-06-13 via the M0 probe). So there's no tokenizer or usage-API
// reconciliation — Cursor cost is computed the same exact way as Claude Code,
// just sourced from a hook payload instead of a transcript line.
//
// Both `stop` and `afterAgentResponse` fire per generation with the SAME
// generation_id and identical final tokens, so dedup by generation_id collapses
// them to one billable unit regardless of which hook(s) are installed.

const cursorFeedSubdir = "cursor"

// cursorHookPayload is the subset of a Cursor agent-hook payload the cost
// feature reads. Token counts live at the top level.
type cursorHookPayload struct {
	HookEventName    string   `json:"hook_event_name"`
	Model            string   `json:"model"`
	ConversationID   string   `json:"conversation_id"`
	GenerationID     string   `json:"generation_id"`
	SessionID        string   `json:"session_id"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	WorkspaceRoots   []string `json:"workspace_roots"`
}

// ParseCursorHookPayload turns a Cursor hook payload into a priced CostEvent.
// It accepts the token-bearing events (`stop`, `afterAgentResponse`); other
// events and zero-token payloads return ok=false. The returned cwd is the
// workspace root (used to route the event to the right per-workspace feed
// file); the CostEvent itself stores only the basename. Timestamp is left empty
// for the caller to stamp.
func ParseCursorHookPayload(raw []byte, table *PriceTable) (ev CostEvent, cwd string, ok bool) {
	var p cursorHookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return CostEvent{}, "", false
	}
	switch p.HookEventName {
	case "stop", "afterAgentResponse":
		// token-bearing events
	default:
		return CostEvent{}, "", false
	}
	if p.InputTokens == 0 && p.OutputTokens == 0 && p.CacheReadTokens == 0 && p.CacheWriteTokens == 0 {
		return CostEvent{}, "", false
	}

	dedupKey := p.GenerationID
	if dedupKey == "" {
		dedupKey = p.ConversationID
	}
	sessionID := p.ConversationID
	if sessionID == "" {
		sessionID = p.SessionID
	}
	if len(p.WorkspaceRoots) > 0 {
		cwd = p.WorkspaceRoots[0]
	}

	usage := Usage{
		InputTokens:          p.InputTokens,
		OutputTokens:         p.OutputTokens,
		CacheReadInputTokens: p.CacheReadTokens,
		CacheCreationInput:   p.CacheWriteTokens, // Cursor reports no TTL split -> 5m rate
	}
	mp, priced := table.ResolvePrice(p.Model)
	c, cacheWriteTokens := CostForUsage(usage, mp)

	ev = CostEvent{
		V:           SchemaVersion,
		Kind:        "cost_event",
		Tool:        ToolCursor,
		SessionID:   sessionID,
		RequestID:   dedupKey,
		Model:       p.Model,
		ModelPriced: priced,
		Source:      SourceExact,
		Tokens: Tokens{
			Input:      p.InputTokens,
			Output:     p.OutputTokens,
			CacheRead:  p.CacheReadTokens,
			CacheWrite: cacheWriteTokens,
		},
		Cost:    c,
		CwdBase: filepath.Base(cwd),
	}
	return ev, cwd, true
}

// CursorFeedDir is where per-workspace Cursor cost feeds live
// (~/.config/promptconduit/cost/cursor/). The `cost hook` command appends to
// these; `cost watch` tails them.
func CursorFeedDir() string {
	return filepath.Join(StoreDir(), cursorFeedSubdir)
}

// CursorFeedPath returns the feed file for a workspace, named by the same
// encoding Claude Code uses for project folders so `cost watch --cwd` can find
// it deterministically.
func CursorFeedPath(cwd string) string {
	return filepath.Join(CursorFeedDir(), encodeProjectPath(cwd)+".ndjson")
}

// AppendCursorEvent appends a CostEvent to the per-workspace Cursor feed. This
// is a local-only write — the cost feature never sends Cursor data anywhere.
func AppendCursorEvent(ev CostEvent, cwd string) error {
	dir := CursorFeedDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(CursorFeedPath(cwd), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// CursorFeedPaths returns the feed files to watch: the one for cwd, or every
// feed when all is true.
func CursorFeedPaths(cwd string, all bool) []string {
	if all {
		entries, err := os.ReadDir(CursorFeedDir())
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".ndjson" {
				out = append(out, filepath.Join(CursorFeedDir(), e.Name()))
			}
		}
		return out
	}
	if cwd == "" {
		return nil
	}
	return []string{CursorFeedPath(cwd)}
}
