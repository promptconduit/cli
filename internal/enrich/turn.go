package enrich

import "time"

// TurnEnrichment is the "turn" slug, attached to turn-close events (Stop,
// StopFailure; Cursor stop): how long the turn took, wall-clock, from the
// prompt that opened it. Combined with the cost and diff slugs on the same
// event this completes the headline metric — "this turn took 4m, cost $0.31,
// changed +120/−40".
//
// The turn clock starts at UserPromptSubmit / Cursor beforeSubmitPrompt
// (recorded by the prompt enricher in the per-session state). An interrupting
// prompt restarts the clock, so an interrupted turn's duration measures from
// its LAST prompt.
type TurnEnrichment struct {
	DurationMs int64 `json:"duration_ms"`
	// PromptID of the prompt that closed with this Stop (from the envelope).
	PromptID string `json:"prompt_id,omitempty"`
}

type turnEnricher struct{}

func init() { Register(turnEnricher{}) }

func (turnEnricher) Slug() string { return "turn" }

func (turnEnricher) Applies(ctx *Context) bool {
	if ctx.SessionID == "" {
		return false
	}
	switch ctx.Tool {
	case "claude-code":
		return ctx.HookEvent == "Stop" || ctx.HookEvent == "StopFailure"
	case "cursor":
		// Cursor's stop fires for completed, aborted, and errored turns alike.
		// Without it the turn opened by beforeSubmitPrompt never closes, and
		// every later prompt in the conversation reads as an interrupt.
		return ctx.HookEvent == "stop"
	}
	return false
}

func (turnEnricher) Enrich(ctx *Context) (any, error) {
	var (
		started time.Time
		open    bool
		err     error
	)
	updateState(ctx.SessionID, func(st *sessionState) bool {
		if st.TurnStartedAt == "" {
			return false // no open turn (e.g. Stop after a fresh install)
		}
		open = true
		started, err = time.Parse(time.RFC3339, st.TurnStartedAt)
		// Close the turn regardless — a corrupt timestamp must not leave the
		// turn permanently open (which would mark every future prompt an
		// interrupt).
		st.TurnStartedAt = ""
		return true
	})
	// Lock busy (open stays false): omit the slug rather than close the turn
	// unlocked; the next Stop closes it.
	if !open || err != nil {
		return nil, nil
	}
	return TurnEnrichment{
		DurationMs: time.Since(started).Milliseconds(),
		PromptID:   ctx.PromptID,
	}, nil
}
