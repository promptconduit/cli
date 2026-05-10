package cmd

import (
	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/correlation"
	"github.com/promptconduit/cli/internal/envelope"
)

// buildCorrelation populates trace/span IDs for an outgoing envelope and
// records the new span for future parent lookups. Failures are silent —
// correlation is best-effort and must never block the hook.
//
// hookEvent is the tool-native event name (Claude Code uses PreToolUse,
// PostToolUse, etc.). nativeEvent is the parsed payload; sessionID has
// already been pulled out by the caller.
func buildCorrelation(tool, hookEvent, sessionID string, nativeEvent map[string]interface{}) *envelope.Correlation {
	store := correlation.NewStore(client.ConfigDir())
	store.MaybeGC()

	spanID := correlation.NewSpanID()

	// No session ID: emit an orphan trace for this event only.
	if sessionID == "" {
		return &envelope.Correlation{
			TraceID: correlation.NewTraceID(),
			SpanID:  spanID,
		}
	}

	rec, err := store.LoadOrCreateTrace(sessionID)
	if err != nil || rec == nil {
		return &envelope.Correlation{
			TraceID: correlation.NewTraceID(),
			SpanID:  spanID,
		}
	}

	parentSpanID := lookupParentSpan(store, tool, hookEvent, sessionID, nativeEvent)
	recordSpan(store, tool, hookEvent, sessionID, spanID, nativeEvent)

	return &envelope.Correlation{
		TraceID:      rec.TraceID,
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
	}
}

// lookupParentSpan resolves parent_span_id for known event chains.
// Returns "" if no parent applies or the lookup misses.
func lookupParentSpan(store *correlation.Store, tool, hookEvent, sessionID string, e map[string]interface{}) string {
	switch tool {
	case "claude-code":
		return lookupParentClaudeCode(store, hookEvent, sessionID, e)
	}
	return ""
}

func lookupParentClaudeCode(store *correlation.Store, hookEvent, sessionID string, e map[string]interface{}) string {
	switch hookEvent {
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
func recordSpan(store *correlation.Store, tool, hookEvent, sessionID, spanID string, e map[string]interface{}) {
	switch tool {
	case "claude-code":
		recordSpanClaudeCode(store, hookEvent, sessionID, spanID, e)
	}
}

func recordSpanClaudeCode(store *correlation.Store, hookEvent, sessionID, spanID string, e map[string]interface{}) {
	switch hookEvent {
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
