package sync

// ParsedConversation represents a fully parsed transcript file
type ParsedConversation struct {
	SessionID        string
	Tool             string
	Title            string
	Summary          string
	StartedAt        string
	EndedAt          string
	RepoName         string
	Branch           string
	WorkingDirectory string
	PrimaryModel     string
	CLIVersion       string
	SourceFilePath   string
	SourceFileHash   string
	Messages         []ParsedMessage
}

// ParsedMessage represents a single message within a conversation
type ParsedMessage struct {
	UUID              string
	ParentUUID        string
	Type              string // user, assistant, system, tool_result, summary, file_snapshot, queue_operation
	Role              string
	Content           string
	Model             string
	Thinking          string
	ToolName          string
	ToolUseID         string
	ToolInput         string
	ToolResult        string
	ToolResultSuccess *bool
	Timestamp         string
	SequenceNumber    int
	GitBranch         string
	GitCommit         string
	Cwd               string
	AttachmentCount   int
	RawJSON           string // Original JSONL line for server-side categorization
}

// SyncState tracks which files have been synced
type SyncState struct {
	SyncedFiles map[string]SyncedFileInfo `json:"synced_files"`
}

// SyncedFileInfo stores information about a synced file
type SyncedFileInfo struct {
	Hash           string `json:"hash"`
	SyncedAt       string `json:"synced_at"`
	ConversationID string `json:"conversation_id,omitempty"`
	MessageCount   int    `json:"message_count"`
}

// SyncResult holds the result of syncing a single file
type SyncResult struct {
	FilePath       string
	ConversationID string
	MessageCount   int
	Status         string // created, updated, skipped, error
	Error          error
}

// Parser is the interface for tool-specific transcript parsers
type Parser interface {
	GetToolName() string
	GetTranscriptPaths() ([]string, error)
	ParseFile(path string) (*ParsedConversation, error)
}
