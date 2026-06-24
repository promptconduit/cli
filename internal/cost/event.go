// Package cost computes real-time token-spend for AI coding sessions entirely
// on-device. It is a deliberate exception to the CLI's "wrap raw events, let
// the platform normalize" design (see internal/envelope): all transcript
// parsing and pricing happen here in Go, and nothing this package produces is
// ever sent to the PromptConduit platform. The watcher tails local transcripts,
// prices each assistant turn against a bundled rate table, and emits cost
// events the editor extension renders in the status bar.
package cost

import "strings"

const (
	// SchemaVersion is stamped on every emitted record so the extension can
	// reject shapes it doesn't understand rather than mis-render them.
	//
	// v2 is the cost drill-down revision. It adds, together:
	//   - CostEvent.Tools   — a content-free per-request tool-call summary (#70)
	//   - CostEvent.Signals — derived cost-reduction metrics (#71)
	// Both ship under the SAME version bump (1 -> 2); #71 deliberately does NOT
	// bump again. The editor extension's SCHEMA_VERSION (src/types.ts, tracked in
	// extension issue 1.3) MUST be bumped to 2 to match before it reads either.
	SchemaVersion = 2

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

// Model tier labels (the `tier` signal). A coarse, content-free bucket derived
// from the model name so the UI can group spend without a price lookup and so
// cost-reduction tips can say "you're on a premium model" without the extension
// re-deriving it. Unpriced/unknown models get TierUnknown.
const (
	TierPremium  = "premium"  // top-end frontier models (e.g. Opus, GPT-5/4.x flagship)
	TierStandard = "standard" // mid-tier workhorse models (e.g. Sonnet, Composer)
	TierEconomy  = "economy"  // small/cheap models (e.g. Haiku, mini/nano/flash)
	TierUnknown  = "unknown"  // not priced / model name not recognized
)

// Signals is a content-free bundle of DERIVED cost-reduction metrics computed
// from a turn's token counts and dollar costs. It exists so cost-saving tips in
// the UI are driven by structured numbers the CLI emits, not re-derived (and
// possibly diverging) inside the editor extension. Like every other field in
// this package it carries NUMBERS ONLY — no prompt content, inputs, or paths.
//
// On a CostEvent these describe a single request; on a SessionSummary they are
// recomputed from the session's accumulated totals (not averaged), so a session
// signal is the same formula applied to summed tokens/costs.
type Signals struct {
	// CacheHitRate is the share of input-side tokens served from the prompt
	// cache: cache_read / (cache_read + cache_creation + input). Range [0,1].
	// Higher is cheaper — cached reads are ~10x cheaper than fresh input. A low
	// rate on a long session is the headline "you could cache more" signal.
	// 0 when there are no input-side tokens at all. Formula is implemented
	// locally here (see cacheHitRate); it is not imported from any other code.
	CacheHitRate float64 `json:"cache_hit_rate"`

	// CacheMissCostShare is the fraction of this turn's TOTAL dollar cost spent
	// on tokens that were NOT cache hits — i.e. fresh input plus cache-creation
	// (cache_write) cost, over total cost. Range [0,1]. High share + low
	// CacheHitRate together point at re-sending uncached context. 0 when total
	// cost is 0 (unpriced model or no spend).
	CacheMissCostShare float64 `json:"cache_miss_cost_share"`

	// InputTokenShare is fresh input tokens as a fraction of all input-side
	// tokens: input / (input + cache_read + cache_creation). Range [0,1]. It is
	// the token-count complement to CacheHitRate (it ignores cache-creation vs
	// read weighting) and answers "how much of what I fed the model was new?".
	// 0 when there are no input-side tokens.
	InputTokenShare float64 `json:"input_token_share"`

	// Tier is the coarse model-cost bucket (premium/standard/economy/unknown).
	// Paired with ModelPriced below so the UI can say "premium, priced" vs
	// "unknown, unpriced" without its own model table.
	Tier string `json:"tier"`

	// ModelPriced mirrors CostEvent.ModelPriced into the signal bundle so a
	// consumer reading only `signals` still knows whether the costs are real
	// (true) or zero-because-unknown (false).
	ModelPriced bool `json:"model_priced"`

	// ToolCalls is the number of tool calls in this turn (== Tools.Total), lifted
	// into the signal bundle as the "tool-call volume" reduction signal. Names
	// live in CostEvent.Tools; this is the count only.
	ToolCalls int `json:"tool_calls"`
}

// cacheHitRate implements the issue's formula locally:
//
//	cache_read / (cache_read + cache_creation + input)
//
// `cache_creation` is the total cache-write tokens (our Tokens.CacheWrite).
// Output tokens are deliberately excluded — this measures input-side cache
// effectiveness only. Returns 0 when the denominator is 0 (no input-side
// tokens), avoiding a divide-by-zero. Implemented from the formula in #71; not
// copied from any platform code.
func cacheHitRate(input, cacheRead, cacheCreation int64) float64 {
	denom := cacheRead + cacheCreation + input
	if denom <= 0 {
		return 0
	}
	return float64(cacheRead) / float64(denom)
}

// inputTokenShare is fresh input over all input-side tokens:
//
//	input / (input + cache_read + cache_creation)
//
// Same denominator as cacheHitRate; returns 0 when there are no input-side
// tokens.
func inputTokenShare(input, cacheRead, cacheCreation int64) float64 {
	denom := input + cacheRead + cacheCreation
	if denom <= 0 {
		return 0
	}
	return float64(input) / float64(denom)
}

// cacheMissCostShare is the dollar share of a turn spent on non-cache-hit
// tokens: (input cost + cache-write cost) / total cost. Returns 0 when total
// cost is 0 (unpriced model or no spend).
func cacheMissCostShare(c Cost) float64 {
	if c.Total <= 0 {
		return 0
	}
	return (c.Input + c.CacheWrite) / c.Total
}

// modelTier buckets a model name into a coarse cost tier (names only — no
// pricing lookup). Unpriced models are always TierUnknown so the UI never
// implies a tier for a model whose cost we couldn't compute. The match is a
// lowercase substring check against well-known family markers; anything
// unrecognized but priced falls through to TierStandard.
func modelTier(model string, priced bool) string {
	if !priced || model == "" {
		return TierUnknown
	}
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "haiku"),
		strings.Contains(m, "mini"),
		strings.Contains(m, "nano"),
		strings.Contains(m, "flash"):
		return TierEconomy
	case strings.Contains(m, "opus"),
		strings.Contains(m, "gpt-5"),
		strings.Contains(m, "gpt-4"),
		strings.Contains(m, "ultra"),
		strings.Contains(m, "-pro"):
		return TierPremium
	default:
		// Sonnet, Composer, and other recognized-but-mid models land here.
		return TierStandard
	}
}

// computeSignals derives the cost-reduction signal bundle from a turn's token
// counts, dollar costs, model, priced-flag, and tool-call count. It is the
// single source of the signal math, used for both per-request CostEvents and
// (re-applied to session totals) the SessionSummary.
func computeSignals(tok Tokens, c Cost, model string, priced bool, toolCalls int) Signals {
	return Signals{
		CacheHitRate:       cacheHitRate(tok.Input, tok.CacheRead, tok.CacheWrite),
		CacheMissCostShare: cacheMissCostShare(c),
		InputTokenShare:    inputTokenShare(tok.Input, tok.CacheRead, tok.CacheWrite),
		Tier:               modelTier(model, priced),
		ModelPriced:        priced,
		ToolCalls:          toolCalls,
	}
}

// ToolSummary is a content-free per-request tool-call summary: how many tool
// calls the assistant made in this turn and a per-tool-name count. It carries
// TOOL NAMES ONLY — never tool inputs, file paths, commands, prompt text, or
// any other content. This is a privacy-first feature in a public/MIT repo, so
// the no-content invariant is load-bearing (see TestPrivacy_* in cost_test.go).
type ToolSummary struct {
	// Total is the number of tool calls in the request (sum of ByName).
	Total int `json:"total"`
	// ByName maps each tool name to how many times it was called this turn,
	// e.g. {"Read": 2, "Bash": 1}. Empty/omitted when no tools were called or
	// when names aren't derivable for the source (see Cursor note in cursor.go).
	ByName map[string]int `json:"by_name,omitempty"`
}

// CostEvent is emitted once per priced assistant turn — the "request cost" the
// status bar shows. It carries only counts, costs, and identifiers; the
// transcript text that produced the counts is never included.
//
// Grouping keys: the editor extension groups cost by "agent tab" using a
// (SessionID, ConversationID) pair and dedups individual turns by RequestID.
// All three are populated from the source's native identifiers where they
// exist (see SessionID/ConversationID/RequestID below); they are the stable
// keys the extension relies on, so do not repurpose or conflate them.
type CostEvent struct {
	V    int    `json:"v"`
	Kind string `json:"kind"` // always "cost_event"
	Tool string `json:"tool"`
	// SessionID is the per-session grouping key: Claude Code's `sessionId`
	// (transcript filename fallback) or Cursor's `session_id`.
	SessionID string `json:"session_id"`
	// ConversationID is Cursor's `conversation_id` — the per-tab grouping key
	// the extension uses alongside SessionID. Claude Code has no separate
	// conversation id, so it is empty there (hence omitempty).
	ConversationID string `json:"conversation_id,omitempty"`
	// RequestID is the per-turn dedup key: Cursor's `generation_id` or Claude
	// Code's `requestId` (UUID fallback). Two hook events for one generation
	// share it, so deduping by RequestID collapses them to one billable turn.
	RequestID   string      `json:"request_id"`
	Timestamp   string      `json:"ts"`
	Model       string      `json:"model"`
	ModelPriced bool        `json:"model_priced"` // false => unknown model, cost is 0
	Source      string      `json:"source"`
	Tokens      Tokens      `json:"tokens"`
	Cost        Cost        `json:"cost"`
	CwdBase     string      `json:"cwd_base"` // basename only — never the full path or prompt
	Tools       ToolSummary `json:"tools"`    // names-only tool-call summary (schema v2+)
	Signals     Signals     `json:"signals"`  // derived cost-reduction signals, numbers only (schema v2+)
}

// summarizeToolNames builds a ToolSummary from a flat list of tool-call names
// (e.g. the `name` of each tool_use block in a Claude Code assistant turn).
// Empty names are ignored. The result holds counts only — by construction it
// can never carry tool inputs or any other content.
func summarizeToolNames(names []string) ToolSummary {
	var s ToolSummary
	for _, n := range names {
		if n == "" {
			continue
		}
		if s.ByName == nil {
			s.ByName = make(map[string]int)
		}
		s.ByName[n]++
		s.Total++
	}
	return s
}

// add folds src into the receiver, summing Total and per-name counts. Used to
// roll per-request tool summaries up into a session aggregate.
func (s *ToolSummary) add(src ToolSummary) {
	if src.Total == 0 && len(src.ByName) == 0 {
		return
	}
	s.Total += src.Total
	for name, n := range src.ByName {
		if s.ByName == nil {
			s.ByName = make(map[string]int)
		}
		s.ByName[name] += n
	}
}

// ModelTotal is one model's contribution to a session. ModelPriced is false
// when the model isn't in the rate table (e.g. Cursor's `composer-*`): tokens
// are still exact, but the cost is 0 and the UI should say "unpriced" rather
// than imply the work was free.
type ModelTotal struct {
	Model       string  `json:"model"`
	ModelPriced bool    `json:"model_priced"`
	Tokens      Tokens  `json:"tokens"`
	CostTotal   float64 `json:"cost_total"`
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
	// Tools is the session's running tool-call total, summed across its cost
	// events (names only; schema v2+). Empty for Cursor sessions, whose cost
	// events don't carry tool names — see cursor.go.
	Tools ToolSummary `json:"tools"`
	// Signals is the session-level cost-reduction signal bundle (schema v2+),
	// recomputed from the session's accumulated totals (not averaged across
	// turns) so e.g. CacheHitRate reflects whole-session cache effectiveness.
	// Tier here is the dominant model's tier (the costliest in ByModel). Numbers
	// only — see Signals.
	Signals Signals `json:"signals"`
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
