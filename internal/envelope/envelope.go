package envelope

import (
	"encoding/json"
	"time"
)

// EnvelopeVersion is the current envelope schema version. The platform is
// expected to handle older versions transparently — the CLI is intentionally
// thin and the server normalizes everything.
const EnvelopeVersion = "1.2"

// RawEventEnvelope is the wrapper sent to the platform API.
//
// The CLI is a thin client: it forwards the tool's raw event payload
// untouched (`native_payload`), plus an `enrichment` block of locally
// computed/inferred context (git, host, correlation IDs, etc.) to help the
// server normalize. All canonical-format work happens server-side.
type RawEventEnvelope struct {
	// Envelope metadata
	EnvelopeVersion string `json:"envelope_version"`
	CliVersion      string `json:"cli_version"`

	// Tool identification
	Tool      string `json:"tool"`       // claude-code, cursor, gemini-cli, etc.
	HookEvent string `json:"hook_event"` // Hook event name from the tool

	// Timing
	CapturedAt string `json:"captured_at"` // ISO8601 timestamp

	// Raw native payload (passed through untouched)
	NativePayload json.RawMessage `json:"native_payload"`

	// Attachment metadata (binary data sent separately in multipart)
	Attachments []AttachmentMetadata `json:"attachments,omitempty"`

	// Enrichment is everything the CLI added on top of the raw payload.
	// Optional: the server should treat absence as "no enrichment available"
	// rather than erroring.
	Enrichment *Enrichment `json:"enrichment,omitempty"`
}

// Enrichment carries CLI-computed context that augments the raw payload.
// Add new fields here rather than at the top level so the envelope keeps a
// clean separation between metadata, raw data, and enrichment.
type Enrichment struct {
	// Git context (extracted by walking up from cwd).
	Git *GitContext `json:"git,omitempty"`

	// Source provider derived from the git remote URL: "github", "gitlab",
	// "bitbucket", "azure", or "" when unknown / no remote.
	Source string `json:"source,omitempty"`

	// W3C Trace Context-compatible correlation IDs.
	Correlation *Correlation `json:"correlation,omitempty"`

	// Host is the machine hostname (best-effort; "" if unavailable).
	Host string `json:"host,omitempty"`

	// OS is runtime.GOOS (linux, darwin, windows, ...).
	OS string `json:"os,omitempty"`

	// Arch is runtime.GOARCH (amd64, arm64, ...).
	Arch string `json:"arch,omitempty"`
}

// Correlation carries W3C Trace Context-compatible IDs so events can be
// stitched into a single trace. Generated locally; not OTEL-SDK backed.
type Correlation struct {
	// TraceID is 32 lowercase hex chars (16 bytes), stable per session.
	TraceID string `json:"trace_id"`
	// SpanID is 16 lowercase hex chars (8 bytes), unique per event.
	SpanID string `json:"span_id"`
	// ParentSpanID is 16 lowercase hex chars when this event has a known
	// parent in a defined event-chain (tool_post → tool_pre, etc.).
	ParentSpanID string `json:"parent_span_id,omitempty"`
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

// New creates a new RawEventEnvelope with the given enrichment block.
// Pass nil for enrichment if none is available.
func New(cliVersion, tool, hookEvent string, nativePayload []byte, enr *Enrichment) *RawEventEnvelope {
	return &RawEventEnvelope{
		EnvelopeVersion: EnvelopeVersion,
		CliVersion:      cliVersion,
		Tool:            tool,
		HookEvent:       hookEvent,
		CapturedAt:      time.Now().UTC().Format(time.RFC3339),
		NativePayload:   nativePayload,
		Enrichment:      enr,
	}
}

// NewWithAttachments creates a new RawEventEnvelope with attachment metadata.
func NewWithAttachments(cliVersion, tool, hookEvent string, nativePayload []byte, enr *Enrichment, attachments []AttachmentMetadata) *RawEventEnvelope {
	return &RawEventEnvelope{
		EnvelopeVersion: EnvelopeVersion,
		CliVersion:      cliVersion,
		Tool:            tool,
		HookEvent:       hookEvent,
		CapturedAt:      time.Now().UTC().Format(time.RFC3339),
		NativePayload:   nativePayload,
		Attachments:     attachments,
		Enrichment:      enr,
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
