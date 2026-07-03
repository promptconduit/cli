package envelope

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the envelope schema generation. v2 is a breaking redesign:
// session_id/prompt_id are lifted to the top level, the native hook payload is
// carried as raw_event, and all CLI-computed context lives in the extensible
// slug-keyed `enrichments` map. There is no v1 back-compat — readers accept
// schema >= 2 only.
const SchemaVersion = 2

// RawEventEnvelope is the single event payload shape: the record appended to
// ~/.promptconduit/events.jsonl at capture time, POSTed verbatim to
// /v1/events/raw, and stored verbatim in the platform's R2 bucket.
//
// The CLI is a thin client: it forwards the tool's raw hook payload untouched
// (`raw_event`) plus an `enrichments` map of locally computed/normalized
// context. Each enrichment is an independent slug — producers add slugs,
// readers ignore slugs they don't know. The platform may append its own
// server-side slugs to the stored copy under the same rules.
type RawEventEnvelope struct {
	// Schema is the envelope generation (SchemaVersion).
	Schema int `json:"schema"`

	// EventID uniquely identifies this event (UUIDv7: time-ordered).
	EventID string `json:"event_id"`

	// SessionID / PromptID are lifted from the tool's raw payload per tool
	// (see ExtractIDs). Empty when the tool doesn't report them.
	SessionID string `json:"session_id,omitempty"`
	PromptID  string `json:"prompt_id,omitempty"`

	// Tool identification.
	Tool      string `json:"tool"`       // claude-code, cursor, gemini-cli, etc.
	HookEvent string `json:"hook_event"` // which hook generated the event

	// Timing + producer version.
	CapturedAt string `json:"captured_at"` // ISO8601 timestamp
	CliVersion string `json:"cli_version"`

	// RawEvent is the tool's native hook payload, passed through untouched.
	RawEvent json.RawMessage `json:"raw_event"`

	// Enrichments maps slug -> enrichment payload (see internal/enrich).
	// Well-known slugs: env, trace, vcs, prompt, cost. Every slug is optional
	// and unknown slugs must be ignored by readers.
	Enrichments map[string]json.RawMessage `json:"enrichments,omitempty"`

	// Attachment metadata (binary data sent separately in multipart).
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

// GitContext contains git repository state at event time. It is the raw
// extraction the vcs enrichment builds on (see internal/enrich/vcs.go).
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
	// IsWorktree is true when the working directory is a linked git worktree
	// (not the main checkout). This is the robust worktree signal for coaching:
	// the WorktreeCreate hook only fires when the agent creates one, missing
	// sessions started inside an existing worktree.
	IsWorktree   bool   `json:"is_worktree,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`
}

// New creates a v2 envelope. rawEvent is the tool's hook payload verbatim;
// enrichments may be nil.
func New(cliVersion, tool, hookEvent, sessionID, promptID string, rawEvent []byte, enrichments map[string]json.RawMessage) *RawEventEnvelope {
	return &RawEventEnvelope{
		Schema:      SchemaVersion,
		EventID:     NewEventID(),
		SessionID:   sessionID,
		PromptID:    promptID,
		Tool:        tool,
		HookEvent:   hookEvent,
		CapturedAt:  time.Now().UTC().Format(time.RFC3339),
		CliVersion:  cliVersion,
		RawEvent:    rawEvent,
		Enrichments: enrichments,
	}
}

// NewWithAttachments creates a v2 envelope carrying attachment metadata.
func NewWithAttachments(cliVersion, tool, hookEvent, sessionID, promptID string, rawEvent []byte, enrichments map[string]json.RawMessage, attachments []AttachmentMetadata) *RawEventEnvelope {
	env := New(cliVersion, tool, hookEvent, sessionID, promptID, rawEvent, enrichments)
	env.Attachments = attachments
	return env
}

// NewEventID returns a time-ordered unique event id (UUIDv7, falling back to
// a random UUIDv4 if the clock-based generator errors).
func NewEventID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.New().String()
}

// ToJSON serializes the envelope to JSON
func (e *RawEventEnvelope) ToJSON() ([]byte, error) {
	return json.Marshal(e)
}

// SupportedTools returns a list of supported tool names
func SupportedTools() []string {
	return []string{"claude-code", "cursor", "gemini-cli", "codex", "copilot"}
}

// IsValidTool checks if the given tool name is supported
func IsValidTool(toolName string) bool {
	switch toolName {
	case "claude-code", "cursor", "gemini-cli", "gemini", "codex", "copilot":
		return true
	default:
		return false
	}
}
