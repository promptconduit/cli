package adapters

import (
	"github.com/promptconduit/cli/internal/schema"
)

// CursorAdapter translates Cursor hook events to canonical format
type CursorAdapter struct {
	BaseAdapter
}

// NewCursorAdapter creates a new Cursor adapter
func NewCursorAdapter(includeRawEvent bool) *CursorAdapter {
	return &CursorAdapter{
		BaseAdapter: BaseAdapter{
			Tool:            schema.ToolCursor,
			IncludeRawEvent: includeRawEvent,
			EventMapping: map[string]schema.EventType{
				"beforeSubmitPrompt":    schema.EventPromptSubmit,
				"beforeShellExecution":  schema.EventShellPre,
				"afterShellExecution":   schema.EventShellPost,
				"beforeMCPExecution":    schema.EventToolPre,
				"afterMCPExecution":     schema.EventToolPost,
				"beforeReadFile":        schema.EventFileRead,
				"afterFileEdit":         schema.EventFileEdit,
				"afterAgentResponse":    schema.EventAgentResponse,
				"afterAgentThought":     schema.EventAgentThought,
				"stop":                  schema.EventSessionEnd,
			},
		},
	}
}

// TranslateEvent converts a Cursor native event to canonical format
func (a *CursorAdapter) TranslateEvent(nativeEvent map[string]interface{}) *schema.CanonicalEvent {
	eventName := GetString(nativeEvent, "event")
	if eventName == "" {
		return nil
	}

	eventType, ok := a.GetEventType(eventName)
	if !ok {
		return nil
	}

	event := a.CreateBaseEvent(eventType, nativeEvent, eventName)

	// Cursor uses conversation_id or generation_id for session tracking
	if convID := GetString(nativeEvent, "conversation_id"); convID != "" {
		event.SessionID = &convID
	} else if genID := GetString(nativeEvent, "generation_id"); genID != "" {
		event.SessionID = &genID
	}

	// Handle event-specific payloads
	switch eventType {
	case schema.EventPromptSubmit:
		event.Prompt = a.translatePrompt(nativeEvent)
	case schema.EventShellPre, schema.EventShellPost:
		event.ToolEvent = a.translateShell(nativeEvent, eventType)
	case schema.EventToolPre, schema.EventToolPost:
		event.ToolEvent = a.translateMCP(nativeEvent, eventType)
	case schema.EventFileRead, schema.EventFileEdit:
		event.ToolEvent = a.translateFileOp(nativeEvent, eventType)
	case schema.EventSessionEnd:
		event.Session = &schema.SessionPayload{
			Reason: GetStringPtr(nativeEvent, "reason"),
		}
	}

	return event
}

// translatePrompt extracts prompt payload from beforeSubmitPrompt events
func (a *CursorAdapter) translatePrompt(nativeEvent map[string]interface{}) *schema.PromptPayload {
	payload := &schema.PromptPayload{}

	if prompt := GetString(nativeEvent, "prompt"); prompt != "" {
		payload.Prompt = prompt
	}

	return payload
}

// translateShell extracts shell command payload
func (a *CursorAdapter) translateShell(nativeEvent map[string]interface{}, eventType schema.EventType) *schema.ToolPayload {
	payload := &schema.ToolPayload{
		ToolName: "Shell",
	}

	// Input contains command and cwd
	input := make(map[string]interface{})
	if cmd := GetString(nativeEvent, "command"); cmd != "" {
		input["command"] = cmd
	}
	if cwd := GetString(nativeEvent, "cwd"); cwd != "" {
		input["cwd"] = cwd
	}
	if len(input) > 0 {
		payload.Input = input
	}

	// For post events, include output
	if eventType == schema.EventShellPost {
		output := make(map[string]interface{})
		if out := GetString(nativeEvent, "output"); out != "" {
			output["output"] = out
		}
		if exitCode, ok := nativeEvent["exit_code"].(float64); ok {
			output["exit_code"] = int(exitCode)
			success := exitCode == 0
			payload.Success = &success
		}
		if len(output) > 0 {
			payload.Output = output
		}

		// Duration
		if durationMs, ok := nativeEvent["duration_ms"].(float64); ok {
			dur := int(durationMs)
			payload.DurationMs = &dur
		}
	}

	return payload
}

// translateMCP extracts MCP tool payload
func (a *CursorAdapter) translateMCP(nativeEvent map[string]interface{}, eventType schema.EventType) *schema.ToolPayload {
	payload := &schema.ToolPayload{}

	if toolName := GetString(nativeEvent, "tool_name"); toolName != "" {
		payload.ToolName = toolName
	}

	if params := GetMap(nativeEvent, "params"); params != nil {
		payload.Input = params
	}

	if eventType == schema.EventToolPost {
		if result := GetMap(nativeEvent, "result"); result != nil {
			payload.Output = result
		}

		if durationMs, ok := nativeEvent["duration_ms"].(float64); ok {
			dur := int(durationMs)
			payload.DurationMs = &dur
		}

		// Check for success
		if success := GetBool(nativeEvent, "success"); success != nil {
			payload.Success = success
		}
	}

	return payload
}

// translateFileOp extracts file operation payload
func (a *CursorAdapter) translateFileOp(nativeEvent map[string]interface{}, eventType schema.EventType) *schema.ToolPayload {
	var toolName string
	if eventType == schema.EventFileRead {
		toolName = "Read"
	} else {
		toolName = "Edit"
	}

	payload := &schema.ToolPayload{
		ToolName: toolName,
	}

	input := make(map[string]interface{})
	if filePath := GetString(nativeEvent, "file_path"); filePath != "" {
		input["file_path"] = filePath
	}
	if len(input) > 0 {
		payload.Input = input
	}

	// For file edits, include the changes
	if eventType == schema.EventFileEdit {
		output := make(map[string]interface{})
		if edits := GetMap(nativeEvent, "edits"); edits != nil {
			output["edits"] = edits
		}
		if len(output) > 0 {
			payload.Output = output
		}
	}

	return payload
}

// CursorEvents returns the set of known Cursor event names
var CursorEvents = map[string]bool{
	"beforeSubmitPrompt":   true,
	"beforeShellExecution": true,
	"afterShellExecution":  true,
	"beforeMCPExecution":   true,
	"afterMCPExecution":    true,
	"beforeReadFile":       true,
	"afterFileEdit":        true,
	"afterAgentResponse":   true,
	"afterAgentThought":    true,
	"stop":                 true,
}
