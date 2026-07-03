package enrich

import "time"

// TurnEnrichment is the "turn" slug, attached to turn-close events (Stop,
// StopFailure): how long the turn took, wall-clock, from the prompt that
// opened it. Combined with the cost and diff slugs on the same event this
// completes the headline metric — "this turn took 4m, cost $0.31, changed
// +120/−40".
//
// The turn clock starts at UserPromptSubmit (recorded by the prompt enricher
// in the per-session state). An interrupting prompt restarts the clock, so an
// interrupted turn's duration measures from its LAST prompt.
type TurnEnrichment struct {
	DurationMs int64 `json:"duration_ms"`
	// PromptID of the prompt that closed with this Stop (from the envelope).
	PromptID string `json:"prompt_id,omitempty"`
}

type turnEnricher struct{}

func init() { Register(turnEnricher{}) }

func (turnEnricher) Slug() string { return "turn" }

func (turnEnricher) Applies(ctx *Context) bool {
	if ctx.Tool != "claude-code" || ctx.SessionID == "" {
		return false
	}
	return ctx.HookEvent == "Stop" || ctx.HookEvent == "StopFailure"
}

func (turnEnricher) Enrich(ctx *Context) (any, error) {
	st := loadState(ctx.SessionID)
	if st.TurnStartedAt == "" {
		return nil, nil // no open turn (e.g. Stop after a fresh install)
	}
	started, err := time.Parse(time.RFC3339, st.TurnStartedAt)

	// Close the turn regardless — a corrupt timestamp must not leave the turn
	// permanently open (which would mark every future prompt an interrupt).
	st.TurnStartedAt = ""
	saveState(ctx.SessionID, st)

	if err != nil {
		return nil, nil
	}
	return TurnEnrichment{
		DurationMs: time.Since(started).Milliseconds(),
		PromptID:   ctx.PromptID,
	}, nil
}
