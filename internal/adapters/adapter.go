package adapters

import (
	"path/filepath"

	"github.com/promptconduit/cli/internal/git"
	"github.com/promptconduit/cli/internal/schema"
)

// Version is set at build time
var Version = "dev"

// Adapter interface for translating native events to canonical format
type Adapter interface {
	// TranslateEvent converts a native event to canonical format
	// Returns nil if the event should be skipped
	TranslateEvent(nativeEvent map[string]interface{}) *schema.CanonicalEvent

	// GetTool returns the tool this adapter handles
	GetTool() schema.Tool

	// GetEventType maps native event names to canonical event types
	GetEventType(nativeEventName string) (schema.EventType, bool)
}

// BaseAdapter provides common functionality for all adapters
type BaseAdapter struct {
	Tool            schema.Tool
	EventMapping    map[string]schema.EventType
	IncludeRawEvent bool
}

// GetTool returns the tool this adapter handles
func (b *BaseAdapter) GetTool() schema.Tool {
	return b.Tool
}

// GetEventType maps native event names to canonical event types
func (b *BaseAdapter) GetEventType(nativeEventName string) (schema.EventType, bool) {
	eventType, ok := b.EventMapping[nativeEventName]
	return eventType, ok
}

// CreateBaseEvent creates a new canonical event with common fields populated
func (b *BaseAdapter) CreateBaseEvent(eventType schema.EventType, nativeEvent map[string]interface{}, nativeEventName string) *schema.CanonicalEvent {
	event := schema.NewCanonicalEvent(b.Tool, eventType, Version)

	// Set raw event type
	event.RawEventType = &nativeEventName

	// Include raw event if debug mode
	if b.IncludeRawEvent {
		event.RawEvent = nativeEvent
	}

	// Extract working directory
	workingDir := b.extractWorkingDir(nativeEvent)

	// Extract git context
	if workingDir != "" {
		event.Git = git.ExtractContext(workingDir)
		event.Workspace = b.extractWorkspace(nativeEvent, workingDir)
	}

	return event
}

// extractWorkingDir extracts the working directory from the event
func (b *BaseAdapter) extractWorkingDir(nativeEvent map[string]interface{}) string {
	// Try common field names
	if cwd, ok := nativeEvent["cwd"].(string); ok && cwd != "" {
		return cwd
	}

	// Try nested context
	if ctx, ok := nativeEvent["context"].(map[string]interface{}); ok {
		if cwd, ok := ctx["cwd"].(string); ok && cwd != "" {
			return cwd
		}
		if workDir, ok := ctx["working_directory"].(string); ok && workDir != "" {
			return workDir
		}
	}

	return ""
}

// extractWorkspace extracts workspace context from the event
func (b *BaseAdapter) extractWorkspace(nativeEvent map[string]interface{}, workingDir string) *schema.WorkspaceContext {
	workspace := &schema.WorkspaceContext{
		WorkingDirectory: &workingDir,
	}

	// Get repo name from git or directory
	repoName := git.GetRepoName(workingDir)
	if repoName != "" {
		workspace.RepoName = &repoName
	}

	// Get repo root path
	if repoPath := filepath.Dir(workingDir); repoPath != "" {
		workspace.RepoPath = &repoPath
	}

	return workspace
}

// GetString safely extracts a string from a map
func GetString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// GetStringPtr safely extracts a string pointer from a map
func GetStringPtr(m map[string]interface{}, key string) *string {
	if v, ok := m[key].(string); ok {
		return &v
	}
	return nil
}

// GetMap safely extracts a nested map from a map
func GetMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return nil
}

// GetBool safely extracts a bool from a map
func GetBool(m map[string]interface{}, key string) *bool {
	if v, ok := m[key].(bool); ok {
		return &v
	}
	return nil
}
