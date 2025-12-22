package adapters

import (
	"os"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/schema"
)

// Registry holds all available adapters
var registry = map[schema.Tool]func(bool) Adapter{
	schema.ToolClaudeCode: func(debug bool) Adapter { return NewClaudeCodeAdapter(debug) },
	schema.ToolCursor:     func(debug bool) Adapter { return NewCursorAdapter(debug) },
	schema.ToolGeminiCLI:  func(debug bool) Adapter { return NewGeminiAdapter(debug) },
}

// GetAdapter returns an adapter for the specified tool
func GetAdapter(tool schema.Tool, debug bool) Adapter {
	if factory, ok := registry[tool]; ok {
		return factory(debug)
	}
	return nil
}

// DetectTool auto-detects which tool generated the event
func DetectTool(nativeEvent map[string]interface{}) schema.Tool {
	// First check environment variable override
	if toolEnv := os.Getenv(client.EnvTool); toolEnv != "" {
		switch toolEnv {
		case "claude-code":
			return schema.ToolClaudeCode
		case "cursor":
			return schema.ToolCursor
		case "gemini-cli", "gemini":
			return schema.ToolGeminiCLI
		}
	}

	// Get event name from the event
	eventName := GetString(nativeEvent, "event")
	if eventName == "" {
		return ""
	}

	// Check which tool's event set contains this event
	if ClaudeCodeEvents[eventName] {
		return schema.ToolClaudeCode
	}
	if CursorEvents[eventName] {
		return schema.ToolCursor
	}
	if GeminiEvents[eventName] {
		return schema.ToolGeminiCLI
	}

	// Check for tool-specific markers
	if _, ok := nativeEvent["cursor_version"]; ok {
		return schema.ToolCursor
	}

	return ""
}

// SupportedTools returns a list of all supported tool names
func SupportedTools() []string {
	return []string{"claude-code", "cursor", "gemini-cli"}
}

// IsValidTool checks if the given tool name is supported
func IsValidTool(toolName string) bool {
	switch toolName {
	case "claude-code", "cursor", "gemini-cli", "gemini":
		return true
	default:
		return false
	}
}

// NormalizeTool converts tool aliases to canonical names
func NormalizeTool(toolName string) schema.Tool {
	switch toolName {
	case "claude-code":
		return schema.ToolClaudeCode
	case "cursor":
		return schema.ToolCursor
	case "gemini-cli", "gemini":
		return schema.ToolGeminiCLI
	default:
		return ""
	}
}
