package enrich

import (
	"bufio"
	"os"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

// CostEnrichment is the "cost" slug, attached to end-of-turn events (Claude
// Code Stop, Cursor stop/afterAgentResponse). It carries the priced requests
// this turn produced — numbers and identifiers only, never content.
//
// Claude Code turns can span several API requests (the tool-use loop), so
// Requests is a list; readers dedup individual requests by RequestID, exactly
// as the old cost-feed consumers did. Cursor events carry one request.
type CostEnrichment struct {
	Requests []CostRequest `json:"requests"`
	Totals   CostTotals    `json:"totals"`
}

// CostRequest is one priced API request.
type CostRequest struct {
	// RequestID is the per-request dedup key (Claude Code requestId / Cursor
	// generation_id).
	RequestID string `json:"request_id"`
	// ConversationID is Cursor's per-tab key; empty for Claude Code.
	ConversationID string `json:"conversation_id,omitempty"`
	Model          string `json:"model"`
	// ModelPriced is false when the model isn't in the rate table: tokens are
	// exact but USD is zero-because-unknown, not free.
	ModelPriced bool   `json:"model_priced"`
	Source      string `json:"source"` // exact | estimate | reconciled
	Timestamp   string `json:"ts,omitempty"`

	Tokens cost.Tokens `json:"tokens"`
	USD    cost.Cost   `json:"usd"`

	// Tools is the names-only tool-call summary (empty for Cursor).
	Tools *cost.ToolSummary `json:"tools,omitempty"`
	// Signals are the derived cost-reduction metrics.
	Signals cost.Signals `json:"signals"`
}

// CostTotals sums the requests in this enrichment.
type CostTotals struct {
	USD      float64     `json:"usd"`
	Currency string      `json:"currency"`
	Tokens   cost.Tokens `json:"tokens"`
}

// maxRequestsPerEvent bounds a single event's request list. The offset state
// normally keeps this to one turn's worth; the cap only matters on the first
// Stop of a long pre-existing session, where the whole transcript backfills.
const maxRequestsPerEvent = 100

type costEnricher struct{}

func init() { Register(costEnricher{}) }

func (costEnricher) Slug() string { return "cost" }

func (costEnricher) Applies(ctx *Context) bool {
	switch ctx.Tool {
	case "claude-code":
		return ctx.HookEvent == "Stop" && ctx.TranscriptPath != ""
	case "cursor":
		return ctx.HookEvent == "stop" || ctx.HookEvent == "afterAgentResponse"
	}
	return false
}

func (costEnricher) Enrich(ctx *Context) (any, error) {
	table, err := cost.LoadPriceTable()
	if err != nil {
		return nil, err
	}
	switch ctx.Tool {
	case "cursor":
		return cursorCost(ctx, table)
	default:
		return claudeCodeCost(ctx, table)
	}
}

// cursorCost prices a Cursor stop/afterAgentResponse payload (exact tokens).
func cursorCost(ctx *Context, table *cost.PriceTable) (any, error) {
	ev, _, ok := cost.ParseCursorHookPayload(ctx.RawJSON, table)
	if !ok {
		return nil, nil // not a token-bearing payload
	}
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339)
	req := toRequest(ev)
	return &CostEnrichment{
		Requests: []CostRequest{req},
		Totals:   totalsOf([]CostRequest{req}),
	}, nil
}

// claudeCodeCost prices the transcript lines appended since the last Stop we
// processed for this session, tracked via a per-session byte offset. The
// offset only ever advances over fully parsed lines, so anything missed at one
// Stop (e.g. a still-flushing write) is picked up at the next.
func claudeCodeCost(ctx *Context, table *cost.PriceTable) (any, error) {
	st := loadState(ctx.SessionID)

	f, err := os.Open(ctx.TranscriptPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := st.TranscriptOffset
	if offset > info.Size() {
		offset = 0 // transcript replaced/truncated — reprice from the top
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}

	var requests []CostRequest
	seen := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	for scanner.Scan() {
		lineLen := int64(len(scanner.Bytes())) + 1 // + newline
		ev, dedupKey, ok := cost.ParseTranscriptLine(scanner.Bytes(), table, ctx.SessionID)
		offset += lineLen
		if !ok || seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		requests = append(requests, toRequest(ev))
		if len(requests) > maxRequestsPerEvent {
			requests = requests[1:] // keep the newest; oldest backfill drops
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if ctx.SessionID != "" {
		st.TranscriptOffset = offset
		saveState(ctx.SessionID, st)
	}
	if len(requests) == 0 {
		return nil, nil
	}
	return &CostEnrichment{Requests: requests, Totals: totalsOf(requests)}, nil
}

func toRequest(ev cost.CostEvent) CostRequest {
	req := CostRequest{
		RequestID:      ev.RequestID,
		ConversationID: ev.ConversationID,
		Model:          ev.Model,
		ModelPriced:    ev.ModelPriced,
		Source:         ev.Source,
		Timestamp:      ev.Timestamp,
		Tokens:         ev.Tokens,
		USD:            ev.Cost,
		Signals:        ev.Signals,
	}
	if ev.Tools.Total > 0 {
		tools := ev.Tools
		req.Tools = &tools
	}
	return req
}

func totalsOf(reqs []CostRequest) CostTotals {
	t := CostTotals{Currency: cost.Currency}
	for _, r := range reqs {
		t.USD += r.USD.Total
		t.Tokens.Input += r.Tokens.Input
		t.Tokens.Output += r.Tokens.Output
		t.Tokens.CacheRead += r.Tokens.CacheRead
		t.Tokens.CacheWrite += r.Tokens.CacheWrite
	}
	return t
}
