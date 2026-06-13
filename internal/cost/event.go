// Package cost computes real-time token-spend for AI coding sessions entirely
// on-device. It is a deliberate exception to the CLI's "wrap raw events, let
// the platform normalize" design (see internal/envelope): all transcript
// parsing and pricing happen here in Go, and nothing this package produces is
// ever sent to the PromptConduit platform. The watcher tails local transcripts,
// prices each assistant turn against a bundled rate table, and emits cost
// events the editor extension renders in the status bar.
package cost

const (
	// SchemaVersion is stamped on every emitted record so the extension can
	// reject shapes it doesn't understand rather than mis-render them.
	SchemaVersion = 1

	// Currency is the only currency v1 prices in; the pricing table is in USD.
	Currency = "USD"

	// Tool identifiers, matching the CLI's existing tool naming (see
	// cmd/hook.go detectTool).
	ToolClaudeCode = "claude-code"
	ToolCursor     = "cursor"

	// Source describes how the token counts were obtained, so the UI can
	// label a session's accuracy.
	SourceExact      = "exact"      // read verbatim from a transcript usage block
	SourceEstimate   = "estimate"   // tokenized locally (Cursor native agent)
	SourceReconciled = "reconciled" // estimate corrected against a provider usage API
)

// Tokens is the per-turn token breakdown. Field names mirror the Claude Code
// transcript usage object so the mapping stays obvious.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"` // total cache-creation (5m + 1h)
}

// Cost is the dollar breakdown for one turn (or a session total). Each field
// is the cost of the corresponding Tokens field; CacheWrite folds the 5m and
// 1h creation costs together since they're priced at different rates.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Total      float64 `json:"total"`
	Currency   string  `json:"currency"`
}

// CostEvent is emitted once per priced assistant turn — the "request cost" the
// status bar shows. It carries only counts, costs, and identifiers; the
// transcript text that produced the counts is never included.
type CostEvent struct {
	V           int    `json:"v"`
	Kind        string `json:"kind"` // always "cost_event"
	Tool        string `json:"tool"`
	SessionID   string `json:"session_id"`
	RequestID   string `json:"request_id"` // dedup key (Claude Code requestId)
	Timestamp   string `json:"ts"`
	Model       string `json:"model"`
	ModelPriced bool   `json:"model_priced"` // false => unknown model, cost is 0
	Source      string `json:"source"`
	Tokens      Tokens `json:"tokens"`
	Cost        Cost   `json:"cost"`
	CwdBase     string `json:"cwd_base"` // basename only — never the full path or prompt
}

// ModelTotal is one model's contribution to a session.
type ModelTotal struct {
	Model     string  `json:"model"`
	Tokens    Tokens  `json:"tokens"`
	CostTotal float64 `json:"cost_total"`
}

// SessionSummary is the rolling per-session aggregate — the "session cost" the
// status bar shows. Emitted on startup (seeded from the transcript) and after
// each new cost event.
type SessionSummary struct {
	V         int          `json:"v"`
	Kind      string       `json:"kind"` // always "session_summary"
	SessionID string       `json:"session_id"`
	Tool      string       `json:"tool"`
	Source    string       `json:"source"` // worst-case across the session's events
	StartedAt string       `json:"started_at"`
	UpdatedAt string       `json:"updated_at"`
	Totals    SessionTotal `json:"totals"`
	ByModel   []ModelTotal `json:"by_model"`
}

// SessionTotal is the flattened token+cost total for a session.
type SessionTotal struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	CostTotal  float64 `json:"cost_total"`
	Currency   string  `json:"currency"`
}
