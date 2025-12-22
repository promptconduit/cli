package schema

// Tool represents supported AI coding assistants
type Tool string

const (
	ToolClaudeCode Tool = "claude-code"
	ToolCursor     Tool = "cursor"
	ToolGeminiCLI  Tool = "gemini-cli"
)

// EventType represents the type of event captured
type EventType string

const (
	EventPromptSubmit  EventType = "prompt_submit"
	EventToolPre       EventType = "tool_pre"
	EventToolPost      EventType = "tool_post"
	EventSessionStart  EventType = "session_start"
	EventSessionEnd    EventType = "session_end"
	EventAgentThought  EventType = "agent_thought"
	EventAgentResponse EventType = "agent_response"
	EventFileRead      EventType = "file_read"
	EventFileEdit      EventType = "file_edit"
	EventFileCreate    EventType = "file_create"
	EventShellPre      EventType = "shell_pre"
	EventShellPost     EventType = "shell_post"
)
