package enrich

import "github.com/promptconduit/cli/internal/transcriptcheck"

// TranscriptEnrichment is the "transcript" slug, attached to Claude Code Stop
// events ONLY when the session's transcript file does not exist. Claude Code
// writes no transcript when a session inherits CLAUDE_CODE_CHILD_SESSION
// ("Transcript saving is off"), so the session's cost cannot be computed or
// synced. Carrying the fact on the event lets every reader (the service, the
// editor extension) flag that session instead of showing silently low spend.
type TranscriptEnrichment struct {
	// Saved is always false: the slug is omitted when the transcript exists.
	Saved bool `json:"saved"`
	// Path is the transcript path the hook reported ("" when none).
	Path    string `json:"path,omitempty"`
	Warning string `json:"warning"`
}

type transcriptEnricher struct{}

func init() { Register(transcriptEnricher{}) }

func (transcriptEnricher) Slug() string { return "transcript" }

func (transcriptEnricher) Applies(ctx *Context) bool {
	return ctx.Tool == "claude-code" && ctx.HookEvent == "Stop" && ctx.SessionID != ""
}

func (transcriptEnricher) Enrich(ctx *Context) (any, error) {
	if !transcriptcheck.Missing(ctx.TranscriptPath) {
		return nil, nil
	}
	return TranscriptEnrichment{
		Saved:   false,
		Path:    ctx.TranscriptPath,
		Warning: transcriptcheck.Message(ctx.SessionID),
	}, nil
}
