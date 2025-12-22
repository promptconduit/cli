package adapters

import (
	"github.com/promptconduit/cli/internal/schema"
)

// GeminiAdapter translates Gemini CLI hook events to canonical format
type GeminiAdapter struct {
	BaseAdapter
}

// Tool name normalization mapping for Gemini CLI
var geminiToolNameMapping = map[string]string{
	"read_file":         "Read",
	"write_file":        "Write",
	"replace":           "Edit",
	"run_shell_command": "Bash",
	"google_web_search": "WebSearch",
	"list_directory":    "Glob",
	"find_files":        "Glob",
	"grep":              "Grep",
	"memory_tool":       "Memory",
}

// NewGeminiAdapter creates a new Gemini CLI adapter
func NewGeminiAdapter(includeRawEvent bool) *GeminiAdapter {
	return &GeminiAdapter{
		BaseAdapter: BaseAdapter{
			Tool:            schema.ToolGeminiCLI,
			IncludeRawEvent: includeRawEvent,
			EventMapping: map[string]schema.EventType{
				"BeforeAgent":  schema.EventPromptSubmit,
				"AfterAgent":   schema.EventAgentResponse,
				"BeforeTool":   schema.EventToolPre,
				"AfterTool":    schema.EventToolPost,
				"SessionStart": schema.EventSessionStart,
				"SessionEnd":   schema.EventSessionEnd,
			},
		},
	}
}

// TranslateEvent converts a Gemini CLI native event to canonical format
func (a *GeminiAdapter) TranslateEvent(nativeEvent map[string]interface{}) *schema.CanonicalEvent {
	eventName := GetString(nativeEvent, "event")
	if eventName == "" {
		return nil
	}

	eventType, ok := a.GetEventType(eventName)
	if !ok {
		return nil
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
		// AfterAgent events - could extract response if needed
	}

	return event
}

// translatePrompt extracts prompt payload from BeforeAgent events
func (a *GeminiAdapter) translatePrompt(nativeEvent map[string]interface{}) *schema.PromptPayload {
	payload := &schema.PromptPayload{}

	if prompt := GetString(nativeEvent, "prompt"); prompt != "" {
		payload.Prompt = prompt
	}

	// Check for user_message as alternative
	if payload.Prompt == "" {
		if userMsg := GetString(nativeEvent, "user_message"); userMsg != "" {
			payload.Prompt = userMsg
		}
	}

	return payload
}

// translateTool extracts tool payload from BeforeTool/AfterTool events
func (a *GeminiAdapter) translateTool(nativeEvent map[string]interface{}, eventType schema.EventType) *schema.ToolPayload {
	payload := &schema.ToolPayload{}

	// Get and normalize tool name
	toolName := GetString(nativeEvent, "tool_name")
	if normalized, ok := geminiToolNameMapping[toolName]; ok {
		payload.ToolName = normalized
	} else if toolName != "" {
		payload.ToolName = toolName
	}

	// Get tool use ID
	payload.ToolUseID = GetStringPtr(nativeEvent, "tool_use_id")

	// Get tool input/parameters
	if input := GetMap(nativeEvent, "tool_input"); input != nil {
		payload.Input = input
	} else if params := GetMap(nativeEvent, "parameters"); params != nil {
		payload.Input = params
	}

	// For AfterTool, get response and success status
	if eventType == schema.EventToolPost {
		if response := GetMap(nativeEvent, "tool_response"); response != nil {
			payload.Output = response
		} else if result := GetMap(nativeEvent, "result"); result != nil {
			payload.Output = result
		}

		// Check for success/error
		if success := GetBool(nativeEvent, "success"); success != nil {
			payload.Success = success
		} else {
			// Default to success if no error
			success := true
			if _, hasError := nativeEvent["error"]; hasError {
				success = false
			}
			payload.Success = &success
		}

		// Duration
		if durationMs, ok := nativeEvent["duration_ms"].(float64); ok {
			dur := int(durationMs)
			payload.DurationMs = &dur
		}
	}

	return payload
}

// translateSession extracts session payload from SessionStart/SessionEnd events
func (a *GeminiAdapter) translateSession(nativeEvent map[string]interface{}, eventType schema.EventType) *schema.SessionPayload {
	payload := &schema.SessionPayload{}

	if eventType == schema.EventSessionStart {
		payload.Source = GetStringPtr(nativeEvent, "source")
	} else {
		payload.Reason = GetStringPtr(nativeEvent, "reason")
	}

	return payload
}

// GeminiEvents returns the set of known Gemini CLI event names
var GeminiEvents = map[string]bool{
	"BeforeAgent":  true,
	"AfterAgent":   true,
	"BeforeTool":   true,
	"AfterTool":    true,
	"SessionStart": true,
	"SessionEnd":   true,
}
