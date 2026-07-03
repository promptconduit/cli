package enrich

import "strings"

// PromptEnrichment is the "prompt" slug, attached to prompt-submission events:
// shape metrics about the prompt, never its content.
type PromptEnrichment struct {
	// Count is which prompt this is within the session (1-based).
	Count int `json:"count"`
	// Chars / Words measure the prompt text.
	Chars int `json:"chars"`
	Words int `json:"words"`
	// HasAttachments is true when the message carried attachments.
	HasAttachments bool `json:"has_attachments,omitempty"`
}

type promptEnricher struct{}

func init() { Register(promptEnricher{}) }

func (promptEnricher) Slug() string { return "prompt" }

func (promptEnricher) Applies(ctx *Context) bool {
	return ctx.HookEvent == "UserPromptSubmit"
}

func (promptEnricher) Enrich(ctx *Context) (any, error) {
	prompt, _ := ctx.RawEvent["prompt"].(string)

	count := 1
	if ctx.SessionID != "" {
		st := loadState(ctx.SessionID)
		st.PromptCount++
		count = st.PromptCount
		saveState(ctx.SessionID, st)
	}

	// Attachments are signalled differently per tool; the pasted-content
	// marker is the reliable Claude Code shape. Best-effort.
	hasAttachments := strings.Contains(prompt, "[Image ") ||
		strings.Contains(prompt, "[Pasted text")

	return PromptEnrichment{
		Count:          count,
		Chars:          len([]rune(prompt)),
		Words:          len(strings.Fields(prompt)),
		HasAttachments: hasAttachments,
	}, nil
}
