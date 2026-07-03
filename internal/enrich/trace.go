package enrich

import (
	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/correlation"
)

// TraceEnrichment is the "trace" slug: W3C Trace Context-compatible IDs so
// events can be stitched into a single per-session trace. Generated locally;
// not OTEL-SDK backed.
type TraceEnrichment struct {
	// TraceID is 32 lowercase hex chars (16 bytes), stable per session.
	TraceID string `json:"trace_id"`
	// SpanID is 16 lowercase hex chars (8 bytes), unique per event.
	SpanID string `json:"span_id"`
	// ParentSpanID is set when this event has a known parent in a defined
	// event chain (tool_post -> tool_pre, Stop -> UserPromptSubmit, etc.).
	ParentSpanID string `json:"parent_span_id,omitempty"`
}

type traceEnricher struct{}

func init() { Register(traceEnricher{}) }

func (traceEnricher) Slug() string              { return "trace" }
func (traceEnricher) Applies(ctx *Context) bool { return true }

func (traceEnricher) Enrich(ctx *Context) (any, error) {
	store := correlation.NewStore(client.ConfigDir())
	store.MaybeGC()

	spanID := correlation.NewSpanID()

	// No session ID: emit an orphan trace for this event only.
	if ctx.SessionID == "" {
		return TraceEnrichment{TraceID: correlation.NewTraceID(), SpanID: spanID}, nil
	}

	rec, err := store.LoadOrCreateTrace(ctx.SessionID)
	if err != nil || rec == nil {
		return TraceEnrichment{TraceID: correlation.NewTraceID(), SpanID: spanID}, nil
	}

	parentSpanID := lookupParentSpan(store, ctx)
	recordSpan(store, ctx, spanID)

	return TraceEnrichment{
		TraceID:      rec.TraceID,
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
	}, nil
}

// lookupParentSpan resolves parent_span_id for known event chains.
// Returns "" if no parent applies or the lookup misses.
//
// Note: Claude Code's hook payloads do NOT currently carry tool_use_id /
// task_id / elicitation_id directly — those fields only appear in the
// transcript JSONL referenced by transcript_path. The chain-keying below
// remains a no-op for those events in real traffic; server-side processing
// can enrich linkage by parsing transcript_path. SubagentStart/Stop do
// carry agent_id and will resolve. session_id-keyed chains (PreCompact,
// SessionEnd, Stop) work everywhere.
func lookupParentSpan(store *correlation.Store, ctx *Context) string {
	if ctx.Tool != "claude-code" {
		return ""
	}
	e, sessionID := ctx.RawEvent, ctx.SessionID
	switch ctx.HookEvent {
	case "PostToolUse", "PostToolUseFailure":
		if id := stringField(e, "tool_use_id"); id != "" {
			return store.LookupParent(sessionID, correlation.SpanKindToolUse, id)
		}
	case "SubagentStop":
		if id := firstStringField(e, "subagent_id", "agent_id"); id != "" {
			return store.LookupParent(sessionID, correlation.SpanKindSubagent, id)
		}
	case "TaskCompleted":
		if id := stringField(e, "task_id"); id != "" {
			return store.LookupParent(sessionID, correlation.SpanKindTask, id)
		}
	case "ElicitationResult":
		if id := stringField(e, "elicitation_id"); id != "" {
			return store.LookupParent(sessionID, correlation.SpanKindElicitation, id)
		}
	case "PostCompact":
		return store.LookupParent(sessionID, correlation.SpanKindContextCompact, sessionID)
	case "Stop", "StopFailure":
		// Agent response: parent is the originating user prompt.
		return store.LookupLastPromptSubmit(sessionID)
	case "SessionEnd":
		return store.LookupRootSpan(sessionID)
	}
	return ""
}

// recordSpan persists span IDs that may become future parents.
func recordSpan(store *correlation.Store, ctx *Context, spanID string) {
	if ctx.Tool != "claude-code" {
		return
	}
	e, sessionID := ctx.RawEvent, ctx.SessionID
	switch ctx.HookEvent {
	case "SessionStart":
		_ = store.RecordRootSpan(sessionID, spanID)
	case "UserPromptSubmit":
		_ = store.RecordLastPromptSubmit(sessionID, spanID)
	case "PreToolUse":
		if id := stringField(e, "tool_use_id"); id != "" {
			_ = store.RecordSpan(sessionID, correlation.SpanKindToolUse, id, spanID)
		}
	case "SubagentStart":
		if id := firstStringField(e, "subagent_id", "agent_id"); id != "" {
			_ = store.RecordSpan(sessionID, correlation.SpanKindSubagent, id, spanID)
		}
	case "TaskCreated":
		if id := stringField(e, "task_id"); id != "" {
			_ = store.RecordSpan(sessionID, correlation.SpanKindTask, id, spanID)
		}
	case "Elicitation":
		if id := stringField(e, "elicitation_id"); id != "" {
			_ = store.RecordSpan(sessionID, correlation.SpanKindElicitation, id, spanID)
		}
	case "PreCompact":
		_ = store.RecordSpan(sessionID, correlation.SpanKindContextCompact, sessionID, spanID)
	}
}

func stringField(e map[string]interface{}, key string) string {
	if v, ok := e[key].(string); ok {
		return v
	}
	return ""
}

func firstStringField(e map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v := stringField(e, k); v != "" {
			return v
		}
	}
	return ""
}
