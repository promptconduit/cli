package enrich

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

// CostEnrichment is the "cost" slug, attached to end-of-turn events (Claude
// Code Stop, plus SessionEnd when it catches late lines; Cursor
// stop/afterAgentResponse). It carries the priced requests this turn produced
// — numbers and identifiers only, never content.
//
// Claude Code turns can span several API requests (the tool-use loop), so
// Requests is a list; readers dedup individual requests by RequestID, exactly
// as the old cost-feed consumers did. Cursor events carry one request.
type CostEnrichment struct {
	Requests []CostRequest `json:"requests"`
	Totals   CostTotals    `json:"totals"`
	// Error says why this event's requests could not be priced (transcript
	// missing, state busy, pricing failed). Empty when pricing ran; a turn
	// that made no API request then carries an empty Requests list. A Claude
	// Code Stop always carries the slug, so a missing slug never hides a gap.
	Error string `json:"error,omitempty"`
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

// costError is the slug payload for a Stop whose requests could not be priced.
func costError(reason string) *CostEnrichment {
	return &CostEnrichment{
		Requests: []CostRequest{},
		Totals:   CostTotals{Currency: cost.Currency},
		Error:    reason,
	}
}

type costEnricher struct{}

func init() { Register(costEnricher{}) }

func (costEnricher) Slug() string { return "cost" }

func (costEnricher) Applies(ctx *Context) bool {
	switch ctx.Tool {
	case "claude-code":
		// Stop prices the turn (and reports why when it can't). SessionEnd
		// catches whatever reached the transcript after the last Stop read
		// it: a session's final request has no later Stop to pick it up.
		return ctx.HookEvent == "Stop" || (ctx.HookEvent == "SessionEnd" && ctx.TranscriptPath != "")
	case "cursor":
		return ctx.HookEvent == "stop" || ctx.HookEvent == "afterAgentResponse"
	}
	return false
}

// failurePayload makes a Claude Code Stop whose enricher errored, panicked or
// timed out carry an explicit error instead of losing the slug.
func (costEnricher) failurePayload(ctx *Context, reason string) any {
	if ctx.Tool == "claude-code" && ctx.HookEvent == "Stop" {
		return costError(reason)
	}
	return nil
}

func (costEnricher) Enrich(ctx *Context) (any, error) {
	table, err := cost.LoadPriceTable()
	if err != nil {
		return nil, err // failurePayload turns this into an explicit error on Stop
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
	ce, ok := CursorCostFromRaw(ctx.RawJSON, table, time.Now().UTC().Format(time.RFC3339))
	if !ok {
		return nil, nil // not a token-bearing payload
	}
	return ce, nil
}

// CursorCostFromRaw builds the `cost` enrichment for a raw Cursor hook payload,
// stamping the request with ts. It is what capture records, so re-running it
// on a stored envelope's raw_event (`cost reprice`) yields exactly what a
// fresh capture with the current parser and rate table would.
func CursorCostFromRaw(raw []byte, table *cost.PriceTable, ts string) (*CostEnrichment, bool) {
	ev, _, ok := cost.ParseCursorHookPayload(raw, table)
	if !ok {
		return nil, false
	}
	ev.Timestamp = ts
	req := toRequest(ev)
	return &CostEnrichment{
		Requests: []CostRequest{req},
		Totals:   totalsOf([]CostRequest{req}),
	}, true
}

// claudeCodeCost prices the transcript lines appended since the last Stop (or
// SessionEnd) processed for this session, tracked via a per-session byte
// offset. The offset only ever advances over complete, newline-terminated
// lines, so a line still being written is priced by the next read, and every
// request lands in exactly one event. There is no cap on how many requests
// one event carries: a long autonomous turn lists them all.
//
// Claude Code can fire Stop a moment before the turn's last assistant line is
// flushed, so Stop first waits briefly for the transcript to hold the final
// message the payload names (last_assistant_message). Anything that still
// arrives late is priced at the next Stop or at SessionEnd.
//
// The read-price-advance runs under the session state lock (updateState), so
// two concurrent hooks for one session can't both price the same lines.
//
// A Stop always returns the slug: priced requests (possibly none, for a turn
// that made no API request) or an Error naming why it couldn't price. A
// SessionEnd returns it only when it priced something.
func claudeCodeCost(ctx *Context, table *cost.PriceTable) (any, error) {
	isStop := ctx.HookEvent == "Stop"
	fail := func(reason string) (any, error) {
		if !isStop {
			return nil, nil
		}
		return costError(reason), nil
	}
	if ctx.TranscriptPath == "" {
		return fail("no transcript_path in the hook payload")
	}
	if _, err := os.Stat(ctx.TranscriptPath); err != nil {
		if os.IsNotExist(err) {
			return fail("transcript not found: Claude Code saved no transcript for this session")
		}
		return fail("transcript unreadable: " + err.Error())
	}

	var (
		requests []CostRequest
		err      error
	)
	if ctx.SessionID == "" {
		requests, _, err = priceTranscriptFrom(ctx, table, 0)
	} else {
		if isStop {
			waitForFinalMessage(ctx, loadState(ctx.SessionID).TranscriptOffset)
		}
		if !updateState(ctx.SessionID, func(st *sessionState) bool {
			var next int64
			requests, next, err = priceTranscriptFrom(ctx, table, st.TranscriptOffset)
			if err != nil {
				return false
			}
			st.TranscriptOffset = next
			return true
		}) {
			return fail("session state busy (another hook held the lock); these requests are priced at the next Stop or SessionEnd")
		}
	}
	if err != nil {
		return fail("read transcript: " + err.Error())
	}
	if len(requests) == 0 && !isStop {
		return nil, nil
	}
	return &CostEnrichment{Requests: requests, Totals: totalsOf(requests)}, nil
}

// finalMessageWait bounds how long Stop waits for the turn's last assistant
// line to reach the transcript. A var so tests can shorten it.
var finalMessageWait = 2 * time.Second

const finalMessagePoll = 50 * time.Millisecond

// waitForFinalMessage polls the transcript (from offset) until it holds the
// assistant text the Stop payload reports as last_assistant_message, or the
// wait runs out. The text is compared in memory only and never stored.
func waitForFinalMessage(ctx *Context, offset int64) {
	want, _ := ctx.RawEvent["last_assistant_message"].(string)
	want = messageKey(want)
	if want == "" {
		return
	}
	deadline := time.Now().Add(finalMessageWait)
	for {
		if transcriptHasText(ctx.TranscriptPath, offset, want) || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(finalMessagePoll)
	}
}

// messageKey normalizes assistant text for comparison: trimmed and capped at a
// prefix, so a payload that shortens the message still matches.
func messageKey(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// transcriptHasText reports whether a complete assistant line after offset has
// a text block matching want (a messageKey).
func transcriptHasText(path string, offset int64, want string) bool {
	found := false
	_, _ = readCompleteLines(path, offset, func(line []byte) {
		if found || !bytes.Contains(line, []byte(`"assistant"`)) {
			return
		}
		var l struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "assistant" {
			return
		}
		for _, b := range l.Message.Content {
			if b.Type == "text" && messageKey(b.Text) == want {
				found = true
				return
			}
		}
	})
	return found
}

// readCompleteLines calls fn for each newline-terminated line after offset and
// returns the offset just past the last one. A trailing partial line (still
// being written) is left for the next read.
func readCompleteLines(path string, offset int64, fn func(line []byte)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if offset > info.Size() {
		offset = 0 // transcript replaced/truncated: reprice from the top
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReaderSize(f, 1024*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return offset, nil // a non-empty line here is partial: not consumed
		}
		if err != nil {
			return offset, err
		}
		offset += int64(len(line))
		fn(bytes.TrimRight(line, "\r\n"))
	}
}

// priceTranscriptFrom prices the complete transcript lines after offset and
// returns them with the offset just past the last complete line.
func priceTranscriptFrom(ctx *Context, table *cost.PriceTable, offset int64) ([]CostRequest, int64, error) {
	// Claude Code writes one line per content block, all sharing the request's
	// requestId. Tokens and cost come from the first line; every line's
	// tool_use blocks are folded into that request (each once, by id).
	var order []*CostRequest
	byKey := map[string]*CostRequest{}
	var counter cost.ToolCounter
	next, err := readCompleteLines(ctx.TranscriptPath, offset, func(line []byte) {
		ev, dedupKey, ok := cost.ParseTranscriptLine(line, table, ctx.SessionID)
		if !ok {
			return
		}
		req := byKey[dedupKey]
		if req == nil {
			r := toRequest(ev)
			r.Tools = nil // rebuilt below via the counter
			req = &r
			byKey[dedupKey] = req
			order = append(order, req)
		}
		var tools cost.ToolSummary
		if req.Tools != nil {
			tools = *req.Tools
		}
		if counter.Fold(&tools, ev) > 0 {
			req.Tools = &tools
			req.Signals.ToolCalls = tools.Total
		}
	})
	if err != nil {
		return nil, 0, err
	}
	requests := make([]CostRequest, len(order))
	for i, r := range order {
		requests[i] = *r
	}
	return requests, next, nil
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
