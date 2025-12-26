package envelope

import (
	"encoding/json"
	"time"
)

// RawEventEnvelope is the wrapper sent to the platform API.
// The platform handles all transformation to canonical format.
type RawEventEnvelope struct {
	// Envelope metadata
	EnvelopeVersion string `json:"envelope_version"` // Currently "1.0"
	CliVersion      string `json:"cli_version"`      // CLI semver

	// Tool identification
	Tool      string `json:"tool"`       // claude-code, cursor, gemini-cli, etc.
	HookEvent string `json:"hook_event"` // Hook event name from the tool

	// Timing
	CapturedAt string `json:"captured_at"` // ISO8601 timestamp

	// Git context (extracted by CLI)
	Git *GitContext `json:"git,omitempty"`

	// Raw native payload (passed through untouched)
	NativePayload json.RawMessage `json:"native_payload"`

	// Attachment metadata (binary data sent separately in multipart)
	Attachments []AttachmentMetadata `json:"attachments,omitempty"`
}

// AttachmentMetadata describes an attachment sent with the envelope.
// The actual binary data is sent as a separate multipart field.
type AttachmentMetadata struct {
	AttachmentID string `json:"attachment_id"` // UUID for correlation
	Filename     string `json:"filename"`
	ContentType  string `json:"content_type"`
	SizeBytes    int64  `json:"size_bytes"`
	Type         string `json:"type"` // "image", "document", "file"
}

// GitContext contains git repository state at event time.
// Extracted by the CLI since it has file system access.
type GitContext struct {
	RepoName         string `json:"repo_name,omitempty"`
	RepoPath         string `json:"repo_path,omitempty"`
	Branch           string `json:"branch,omitempty"`
	CommitHash       string `json:"commit_hash,omitempty"`
	CommitMessage    string `json:"commit_message,omitempty"`
	CommitAuthor     string `json:"commit_author,omitempty"`
	IsDirty          bool   `json:"is_dirty,omitempty"`
	StagedCount      int    `json:"staged_count,omitempty"`
	UnstagedCount    int    `json:"unstaged_count,omitempty"`
	UntrackedCount   int    `json:"untracked_count,omitempty"`
	AheadCount       int    `json:"ahead_count,omitempty"`
	BehindCount      int    `json:"behind_count,omitempty"`
	RemoteURL        string `json:"remote_url,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	IsDetachedHead   bool   `json:"is_detached_head,omitempty"`
}

// New creates a new RawEventEnvelope
func New(cliVersion, tool, hookEvent string, nativePayload []byte, git *GitContext) *RawEventEnvelope {
	return &RawEventEnvelope{
		EnvelopeVersion: "1.0",
		CliVersion:      cliVersion,
		Tool:            tool,
		HookEvent:       hookEvent,
		CapturedAt:      time.Now().UTC().Format(time.RFC3339),
		Git:             git,
		NativePayload:   nativePayload,
	}
}

// NewWithAttachments creates a new RawEventEnvelope with attachment metadata
func NewWithAttachments(cliVersion, tool, hookEvent string, nativePayload []byte, git *GitContext, attachments []AttachmentMetadata) *RawEventEnvelope {
	return &RawEventEnvelope{
		EnvelopeVersion: "1.0",
		CliVersion:      cliVersion,
		Tool:            tool,
		HookEvent:       hookEvent,
		CapturedAt:      time.Now().UTC().Format(time.RFC3339),
		Git:             git,
		NativePayload:   nativePayload,
		Attachments:     attachments,
	}
}

// ToJSON serializes the envelope to JSON
func (e *RawEventEnvelope) ToJSON() ([]byte, error) {
	return json.Marshal(e)
}

// SupportedTools returns a list of supported tool names
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
