package adapters

import (
	"github.com/promptconduit/cli/internal/schema"
)

// ClaudeCodeAdapter translates Claude Code hook events to canonical format
type ClaudeCodeAdapter struct {
	BaseAdapter
}

// NewClaudeCodeAdapter creates a new Claude Code adapter
func NewClaudeCodeAdapter(includeRawEvent bool) *ClaudeCodeAdapter {
	return &ClaudeCodeAdapter{
		BaseAdapter: BaseAdapter{
			Tool:            schema.ToolClaudeCode,
			IncludeRawEvent: includeRawEvent,
			EventMapping: map[string]schema.EventType{
				"UserPromptSubmit":  schema.EventPromptSubmit,
				"PreToolUse":        schema.EventToolPre,
				"PostToolUse":       schema.EventToolPost,
				"SessionStart":      schema.EventSessionStart,
				"SessionEnd":        schema.EventSessionEnd,
				"Stop":              schema.EventAgentResponse,
				"SubagentStop":      schema.EventAgentResponse,
				"PermissionRequest": schema.EventToolPre,
				"Notification":      schema.EventAgentThought,
			},
		},
	}
}

// TranslateEvent converts a Claude Code native event to canonical format
func (a *ClaudeCodeAdapter) TranslateEvent(nativeEvent map[string]interface{}) *schema.CanonicalEvent {
	eventName := GetString(nativeEvent, "event")
	if eventName == "" {
		return nil
	}

	eventType, ok := a.GetEventType(eventName)
	if !ok {
		return nil // Unknown event type, skip
	}

	event := a.CreateBaseEvent(eventType, nativeEvent, eventName)

	// Extract session ID
	if sessionID := GetString(nativeEvent, "session_id"); sessionID != "" {
		event.SessionID = &sessionID
	}

	// Handle event-specific payloads
	switch eventType {
	case schema.EventPromptSubmit:
		event.Prompt = a.translatePrompt(nativeEvent)
	case schema.EventToolPre, schema.EventToolPost:
		event.ToolEvent = a.translateTool(nativeEvent, eventType)
	case schema.EventSessionStart, schema.EventSessionEnd:
		event.Session = a.translateSession(nativeEvent, eventType)
	case schema.EventAgentResponse:
		// Stop events don't have specific payload
	case schema.EventAgentThought:
		// Notification events - could extract message if needed
	}

	return event
}

// translatePrompt extracts prompt payload from UserPromptSubmit events
func (a *ClaudeCodeAdapter) translatePrompt(nativeEvent map[string]interface{}) *schema.PromptPayload {
	payload := &schema.PromptPayload{}

	// Get prompt text
	if prompt := GetString(nativeEvent, "prompt"); prompt != "" {
		payload.Prompt = prompt
	}

	// Get response summary if available
	if summary := GetString(nativeEvent, "response_summary"); summary != "" {
		payload.ResponseSummary = &summary
	}

	return payload
}

// translateTool extracts tool payload from PreToolUse/PostToolUse events
func (a *ClaudeCodeAdapter) translateTool(nativeEvent map[string]interface{}, eventType schema.EventType) *schema.ToolPayload {
	payload := &schema.ToolPayload{}

	// Get tool name
	if toolName := GetString(nativeEvent, "tool_name"); toolName != "" {
		payload.ToolName = toolName
	}

	// Get tool use ID
	payload.ToolUseID = GetStringPtr(nativeEvent, "tool_use_id")

	// Get tool input
	if input := GetMap(nativeEvent, "tool_input"); input != nil {
		payload.Input = input
	}

	// For PostToolUse, get response and success status
	if eventType == schema.EventToolPost {
		if response := GetMap(nativeEvent, "tool_response"); response != nil {
			payload.Output = response

			// Check for error in response
			success := true
			if _, hasError := response["error"]; hasError {
				success = false
			}
			if isError, ok := response["is_error"].(bool); ok && isError {
				success = false
			}
			payload.Success = &success
		}
	}

	return payload
}

// translateSession extracts session payload from SessionStart/SessionEnd events
func (a *ClaudeCodeAdapter) translateSession(nativeEvent map[string]interface{}, eventType schema.EventType) *schema.SessionPayload {
	payload := &schema.SessionPayload{}

	if eventType == schema.EventSessionStart {
		payload.Source = GetStringPtr(nativeEvent, "source")
	} else {
		payload.Reason = GetStringPtr(nativeEvent, "reason")
	}

	return payload
}

// ClaudeCodeEvents returns the set of known Claude Code event names
var ClaudeCodeEvents = map[string]bool{
	"UserPromptSubmit":  true,
	"PreToolUse":        true,
	"PostToolUse":       true,
	"SessionStart":      true,
	"SessionEnd":        true,
	"Stop":              true,
	"SubagentStop":      true,
	"PermissionRequest": true,
	"Notification":      true,
}
