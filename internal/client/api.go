package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/transcript"
)

// APIResponse represents a response from the API
type APIResponse struct {
	Success    bool
	StatusCode int
	Data       map[string]interface{}
	Error      string
}

// Client is the HTTP client for the PromptConduit API
type Client struct {
	config     *Config
	httpClient *http.Client
	version    string
}

// NewClient creates a new API client
func NewClient(config *Config, version string) *Client {
	return &Client{
		config: config,
		httpClient: &http.Client{
			Timeout: time.Duration(config.TimeoutSeconds) * time.Second,
		},
		version: version,
	}
}

// SendEnvelope sends a raw event envelope to the API (blocking)
func (c *Client) SendEnvelope(env *envelope.RawEventEnvelope) *APIResponse {
	return c.sendRequest("/v1/events/raw", env)
}

// AttachmentData holds attachment binary data with its metadata
type AttachmentData struct {
	AttachmentID string
	Filename     string
	ContentType  string
	Data         []byte
}

// SendEnvelopeWithAttachments sends an envelope with binary attachments via multipart
func (c *Client) SendEnvelopeWithAttachments(env *envelope.RawEventEnvelope, attachments []AttachmentData) *APIResponse {
	// Create multipart body
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add envelope as JSON field
	envJSON, err := env.ToJSON()
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to serialize envelope: %v", err),
		}
	}

	if err := writer.WriteField("envelope", string(envJSON)); err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to write envelope field: %v", err),
		}
	}

	// Add attachments with format: attachment[uuid]
	for _, att := range attachments {
		fieldName := fmt.Sprintf("attachment[%s]", att.AttachmentID)
		part, err := writer.CreateFormFile(fieldName, att.Filename)
		if err != nil {
			continue
		}
		part.Write(att.Data)
	}

	if err := writer.Close(); err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to close multipart writer: %v", err),
		}
	}

	// Create request to same endpoint but with multipart content type
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/events/raw", body)
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to create request: %v", err),
		}
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("User-Agent", fmt.Sprintf("PromptConduit-CLI/%s", c.version))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	result := &APIResponse{
		StatusCode: resp.StatusCode,
		Success:    resp.StatusCode >= 200 && resp.StatusCode < 300,
	}

	if len(respBody) > 0 {
		var data map[string]interface{}
		if err := json.Unmarshal(respBody, &data); err == nil {
			result.Data = data
		}
	}

	if !result.Success {
		result.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return result
}

// SerializedEnvelopeWithAttachments is used for JSON serialization of envelope + attachments
type SerializedEnvelopeWithAttachments struct {
	Envelope    *envelope.RawEventEnvelope `json:"envelope"`
	Attachments []AttachmentData           `json:"attachments"`
}

// SendEnvelopeWithAttachmentsAsync sends an envelope with attachments asynchronously
func (c *Client) SendEnvelopeWithAttachmentsAsync(env *envelope.RawEventEnvelope, attachments []AttachmentData) error {
	data := SerializedEnvelopeWithAttachments{
		Envelope:    env,
		Attachments: attachments,
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to serialize envelope with attachments: %w", err)
	}

	// For payloads with attachments (typically images), always use blocking mode
	// since subprocess stdin has ~64KB limit and images are often larger
	return c.sendEnvelopeWithAttachmentsBlocking(jsonData)
}

// sendEnvelopeWithAttachmentsBlocking deserializes and sends via multipart
func (c *Client) sendEnvelopeWithAttachmentsBlocking(jsonData []byte) error {
	var data SerializedEnvelopeWithAttachments
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return err
	}

	result := c.SendEnvelopeWithAttachments(data.Envelope, data.Attachments)
	if !result.Success {
		return fmt.Errorf("API error: %s", result.Error)
	}
	return nil
}

// SendEnvelopeWithAttachmentsDirect sends serialized envelope with attachments directly
// (used by async subprocess)
func (c *Client) SendEnvelopeWithAttachmentsDirect(jsonData []byte) error {
	return c.sendEnvelopeWithAttachmentsBlocking(jsonData)
}

// SendEnvelopeAsync sends an envelope asynchronously without blocking
func (c *Client) SendEnvelopeAsync(env *envelope.RawEventEnvelope) error {
	envJSON, err := env.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize envelope: %w", err)
	}

	if runtime.GOOS == "windows" {
		return c.sendAsyncWindows(envJSON)
	}

	return c.sendAsyncUnix(envJSON)
}

// sendAsyncUnix uses fork to send envelope without blocking
func (c *Client) sendAsyncUnix(envJSON []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return c.sendEnvelopeBlocking(envJSON)
	}

	cmd := exec.Command(exe, "hook", "--send-event")
	cmd.Stdin = bytes.NewReader(envJSON)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return c.sendEnvelopeBlocking(envJSON)
	}

	if err := cmd.Process.Release(); err != nil {
		// Process already started, ignore error
	}

	return nil
}

// sendAsyncWindows spawns a subprocess on Windows
func (c *Client) sendAsyncWindows(envJSON []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return c.sendEnvelopeBlocking(envJSON)
	}

	cmd := exec.Command(exe, "hook", "--send-event")
	cmd.Stdin = bytes.NewReader(envJSON)

	if err := cmd.Start(); err != nil {
		return c.sendEnvelopeBlocking(envJSON)
	}

	go func() {
		_ = cmd.Wait()
	}()

	return nil
}

// sendEnvelopeBlocking sends the envelope synchronously (fallback)
func (c *Client) sendEnvelopeBlocking(envJSON []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/events/raw", bytes.NewReader(envJSON))
	if err != nil {
		return err
	}

	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	return nil
}

// sendRequest performs an HTTP request to the API
func (c *Client) sendRequest(path string, payload interface{}) *APIResponse {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to marshal payload: %v", err),
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+path, bytes.NewReader(jsonData))
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to create request: %v", err),
		}
	}

	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	result := &APIResponse{
		StatusCode: resp.StatusCode,
		Success:    resp.StatusCode >= 200 && resp.StatusCode < 300,
	}

	if len(body) > 0 {
		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err == nil {
			result.Data = data
		}
	}

	if !result.Success {
		result.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return result
}

// setHeaders sets common HTTP headers
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("User-Agent", fmt.Sprintf("PromptConduit-CLI/%s", c.version))
}

// SendEnvelopeDirect sends an envelope directly (used by async subprocess)
func (c *Client) SendEnvelopeDirect(envJSON []byte) error {
	return c.sendEnvelopeBlocking(envJSON)
}

// TestConnection sends a test request to verify API connectivity
func (c *Client) TestConnection() *APIResponse {
	// Create a minimal test envelope
	testEnv := envelope.New(
		c.version,
		"test",
		"test",
		[]byte(`{"test": true}`),
		nil,
	)
	return c.SendEnvelope(testEnv)
}

// PromptMetadata contains metadata for prompt ingestion
type PromptMetadata struct {
	Tool            string                 `json:"tool"`
	HookVersion     string                 `json:"hookVersion,omitempty"`
	Prompt          string                 `json:"prompt"`
	ConversationID  string                 `json:"conversationId,omitempty"`
	CapturedAt      string                 `json:"capturedAt,omitempty"`
	Context         *PromptContextMetadata `json:"context,omitempty"`
}

// PromptContextMetadata contains context information for a prompt
type PromptContextMetadata struct {
	RepoName         string       `json:"repoName,omitempty"`
	RepoPath         string       `json:"repoPath,omitempty"`
	Branch           string       `json:"branch,omitempty"`
	WorkingDirectory string       `json:"workingDirectory,omitempty"`
	GitMetadata      *GitMetadata `json:"gitMetadata,omitempty"`
}

// GitMetadata contains git-specific metadata
type GitMetadata struct {
	CommitHash     string `json:"commitHash,omitempty"`
	CommitMessage  string `json:"commitMessage,omitempty"`
	CommitAuthor   string `json:"commitAuthor,omitempty"`
	IsDirty        bool   `json:"isDirty,omitempty"`
	StagedCount    int    `json:"stagedCount,omitempty"`
	UnstagedCount  int    `json:"unstagedCount,omitempty"`
	UntrackedCount int    `json:"untrackedCount,omitempty"`
	AheadCount     int    `json:"aheadCount,omitempty"`
	BehindCount    int    `json:"behindCount,omitempty"`
	RemoteURL      string `json:"remoteUrl,omitempty"`
	IsDetachedHead bool   `json:"isDetachedHead,omitempty"`
}

// SendPromptWithAttachments sends a prompt with attachments using multipart/form-data
func (c *Client) SendPromptWithAttachments(metadata *PromptMetadata, attachments []transcript.Attachment) *APIResponse {
	// Create multipart body
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add metadata as JSON field
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to marshal metadata: %v", err),
		}
	}

	if err := writer.WriteField("metadata", string(metadataJSON)); err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to write metadata field: %v", err),
		}
	}

	// Add attachments as file fields
	for _, att := range attachments {
		part, err := writer.CreateFormFile("attachments[]", att.Filename)
		if err != nil {
			continue
		}
		part.Write(att.Data)
	}

	if err := writer.Close(); err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to close multipart writer: %v", err),
		}
	}

	// Create request
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/prompts/ingest-multipart", body)
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to create request: %v", err),
		}
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("User-Agent", fmt.Sprintf("PromptConduit-CLI/%s", c.version))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	result := &APIResponse{
		StatusCode: resp.StatusCode,
		Success:    resp.StatusCode >= 200 && resp.StatusCode < 300,
	}

	if len(respBody) > 0 {
		var data map[string]interface{}
		if err := json.Unmarshal(respBody, &data); err == nil {
			result.Data = data
		}
	}

	if !result.Success {
		result.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return result
}

// SendPromptWithAttachmentsAsync sends a prompt with attachments asynchronously
func (c *Client) SendPromptWithAttachmentsAsync(metadata *PromptMetadata, attachments []transcript.Attachment) error {
	// For multipart, we need to serialize everything
	data := struct {
		Metadata    *PromptMetadata          `json:"metadata"`
		Attachments []SerializedAttachment   `json:"attachments"`
	}{
		Metadata:    metadata,
		Attachments: make([]SerializedAttachment, len(attachments)),
	}

	for i, att := range attachments {
		data.Attachments[i] = SerializedAttachment{
			Data:      att.Data,
			MediaType: att.MediaType,
			Filename:  att.Filename,
			Type:      att.Type,
		}
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to serialize prompt data: %w", err)
	}

	if runtime.GOOS == "windows" {
		return c.sendPromptAsyncWindows(jsonData)
	}

	return c.sendPromptAsyncUnix(jsonData)
}

// SerializedAttachment is used for JSON serialization of attachments
type SerializedAttachment struct {
	Data      []byte `json:"data"`
	MediaType string `json:"media_type"`
	Filename  string `json:"filename"`
	Type      string `json:"type"`
}

// sendPromptAsyncUnix spawns a subprocess to send the prompt
func (c *Client) sendPromptAsyncUnix(jsonData []byte) error {
	// For large payloads (e.g., with images), use blocking mode to ensure
	// all data is written to stdin before the process is released.
	// The async subprocess stdin pipe gets truncated at ~64KB otherwise.
	const maxAsyncSize = 32 * 1024 // 32KB threshold
	if len(jsonData) > maxAsyncSize {
		return c.sendPromptBlocking(jsonData)
	}

	exe, err := os.Executable()
	if err != nil {
		return c.sendPromptBlocking(jsonData)
	}

	cmd := exec.Command(exe, "hook", "--send-prompt")
	cmd.Stdin = bytes.NewReader(jsonData)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return c.sendPromptBlocking(jsonData)
	}

	if err := cmd.Process.Release(); err != nil {
		// Process already started, ignore error
	}

	return nil
}

// sendPromptAsyncWindows spawns a subprocess on Windows
func (c *Client) sendPromptAsyncWindows(jsonData []byte) error {
	// For large payloads (e.g., with images), use blocking mode to ensure
	// all data is written to stdin before the process completes.
	const maxAsyncSize = 32 * 1024 // 32KB threshold
	if len(jsonData) > maxAsyncSize {
		return c.sendPromptBlocking(jsonData)
	}

	exe, err := os.Executable()
	if err != nil {
		return c.sendPromptBlocking(jsonData)
	}

	cmd := exec.Command(exe, "hook", "--send-prompt")
	cmd.Stdin = bytes.NewReader(jsonData)

	if err := cmd.Start(); err != nil {
		return c.sendPromptBlocking(jsonData)
	}

	go func() {
		_ = cmd.Wait()
	}()

	return nil
}

// sendPromptBlocking sends the prompt synchronously (fallback)
func (c *Client) sendPromptBlocking(jsonData []byte) error {
	var data struct {
		Metadata    *PromptMetadata        `json:"metadata"`
		Attachments []SerializedAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return err
	}

	attachments := make([]transcript.Attachment, len(data.Attachments))
	for i, att := range data.Attachments {
		attachments[i] = transcript.Attachment{
			Data:      att.Data,
			MediaType: att.MediaType,
			Filename:  att.Filename,
			Type:      att.Type,
		}
	}

	result := c.SendPromptWithAttachments(data.Metadata, attachments)
	if !result.Success {
		return fmt.Errorf("API error: %s", result.Error)
	}
	return nil
}

// SendPromptDirect sends a serialized prompt directly (used by async subprocess)
func (c *Client) SendPromptDirect(jsonData []byte) error {
	return c.sendPromptBlocking(jsonData)
}

// TranscriptSyncRequest represents the request body for syncing a transcript
type TranscriptSyncRequest struct {
	Conversation TranscriptConversation `json:"conversation"`
	Messages     []TranscriptMessage    `json:"messages"`
}

// TranscriptConversation represents conversation metadata for sync
type TranscriptConversation struct {
	SessionID        string `json:"session_id"`
	Tool             string `json:"tool"`
	Title            string `json:"title,omitempty"`
	Summary          string `json:"summary,omitempty"`
	StartedAt        string `json:"started_at"`
	EndedAt          string `json:"ended_at,omitempty"`
	RepoName         string `json:"repo_name,omitempty"`
	Branch           string `json:"branch,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	PrimaryModel     string `json:"primary_model,omitempty"`
	CLIVersion       string `json:"cli_version,omitempty"`
	SourceFilePath   string `json:"source_file_path,omitempty"`
	SourceFileHash   string `json:"source_file_hash"`
}

// TranscriptMessage represents a message for sync
type TranscriptMessage struct {
	UUID              string `json:"uuid"`
	ParentUUID        string `json:"parent_uuid,omitempty"`
	Type              string `json:"type"`
	Role              string `json:"role,omitempty"`
	Content           string `json:"content,omitempty"`
	Model             string `json:"model,omitempty"`
	Thinking          string `json:"thinking,omitempty"`
	ToolName          string `json:"tool_name,omitempty"`
	ToolUseID         string `json:"tool_use_id,omitempty"`
	ToolInput         string `json:"tool_input,omitempty"`
	ToolResult        string `json:"tool_result,omitempty"`
	ToolResultSuccess *bool  `json:"tool_result_success,omitempty"`
	Timestamp         string `json:"timestamp"`
	SequenceNumber    int    `json:"sequence_number"`
	GitBranch         string `json:"git_branch,omitempty"`
	GitCommit         string `json:"git_commit,omitempty"`
	Cwd               string `json:"cwd,omitempty"`
	AttachmentCount   int    `json:"attachment_count,omitempty"`
}

// TranscriptSyncResponse represents the API response for sync
type TranscriptSyncResponse struct {
	ConversationID string `json:"conversation_id"`
	MessageCount   int    `json:"message_count"`
	Status         string `json:"status"` // created, updated, skipped
	Message        string `json:"message,omitempty"`
}

// SyncTranscript sends a transcript to the API
func (c *Client) SyncTranscript(req *TranscriptSyncRequest) (*TranscriptSyncResponse, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // Longer timeout for large transcripts
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/transcripts/sync", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var syncResp TranscriptSyncResponse
	if err := json.Unmarshal(body, &syncResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &syncResp, nil
}

// RawTranscriptSyncRequest represents the request body for raw transcript sync
// Platform performs message categorization server-side
type RawTranscriptSyncRequest struct {
	SessionID      string              `json:"session_id"`
	Tool           string              `json:"tool"`
	SourceFileHash string              `json:"source_file_hash"`
	SourceFilePath string              `json:"source_file_path,omitempty"`
	RawMessages    []RawTranscriptMessage `json:"raw_messages"`
}

// RawTranscriptMessage represents a raw JSONL message for server-side categorization
type RawTranscriptMessage struct {
	RawJSON   string `json:"raw_json"`
	Sequence  int    `json:"sequence"`
	Timestamp string `json:"timestamp,omitempty"`
}

// SyncTranscriptRaw sends a transcript with raw JSONL for server-side categorization
func (c *Client) SyncTranscriptRaw(req *RawTranscriptSyncRequest) (*TranscriptSyncResponse, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/transcripts/sync/raw", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var syncResp TranscriptSyncResponse
	if err := json.Unmarshal(body, &syncResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &syncResp, nil
}
