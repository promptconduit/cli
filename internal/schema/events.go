package schema

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// CanonicalEvent is the normalized event format sent to the API
type CanonicalEvent struct {
	// Required fields
	Tool           Tool      `json:"tool"`
	EventType      EventType `json:"event_type"`
	EventID        string    `json:"event_id"`
	Timestamp      string    `json:"timestamp"`
	AdapterVersion string    `json:"adapter_version"`

	// Optional context
	SessionID *string           `json:"session_id,omitempty"`
	Workspace *WorkspaceContext `json:"workspace,omitempty"`
	Git       *GitContext       `json:"git,omitempty"`

	// Event-specific payloads (only one should be set)
	Prompt    *PromptPayload  `json:"prompt,omitempty"`
	ToolEvent *ToolPayload    `json:"tool_event,omitempty"`
	Session   *SessionPayload `json:"session,omitempty"`

	// Debugging
	RawEventType *string                `json:"raw_event_type,omitempty"`
	RawEvent     map[string]interface{} `json:"raw_event,omitempty"`
}

// PromptPayload contains data for prompt_submit events
type PromptPayload struct {
	Prompt          string                   `json:"prompt"`
	ResponseSummary *string                  `json:"response_summary,omitempty"`
	Attachments     []map[string]interface{} `json:"attachments,omitempty"`
}

// ToolPayload contains data for tool_pre, tool_post, shell_pre, shell_post events
type ToolPayload struct {
	ToolName     string                 `json:"tool_name"`
	ToolUseID    *string                `json:"tool_use_id,omitempty"`
	Input        map[string]interface{} `json:"input,omitempty"`
	Output       map[string]interface{} `json:"output,omitempty"`
	Success      *bool                  `json:"success,omitempty"`
	DurationMs   *int                   `json:"duration_ms,omitempty"`
	ErrorMessage *string                `json:"error_message,omitempty"`
}

// SessionPayload contains data for session_start and session_end events
type SessionPayload struct {
	Source *string `json:"source,omitempty"` // For start: "startup", "resume", "clear", "compact"
	Reason *string `json:"reason,omitempty"` // For end: "exit", "logout", "clear", "prompt_input_exit"
}

// NewCanonicalEvent creates a new event with auto-generated ID and timestamp
func NewCanonicalEvent(tool Tool, eventType EventType, adapterVersion string) *CanonicalEvent {
	return &CanonicalEvent{
		Tool:           tool,
		EventType:      eventType,
		EventID:        uuid.New().String(),
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		AdapterVersion: adapterVersion,
	}
}

// ToJSON serializes the event to JSON bytes
func (e *CanonicalEvent) ToJSON() ([]byte, error) {
	return json.Marshal(e)
}

// ToJSONString serializes the event to a JSON string
func (e *CanonicalEvent) ToJSONString() (string, error) {
	data, err := e.ToJSON()
	if err != nil {
		return "", err
	}
	return string(data), nil
}
