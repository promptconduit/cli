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
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/promptconduit/cli/internal/logger"
	"github.com/promptconduit/cli/internal/outbound"
)

// eventsEndpoint is the path every raw-event send targets. Centralized so the
// event-log records and the request URLs can't drift apart.
const eventsEndpoint = "/v1/events/raw"

// recordEventSend updates the rolling send counters (status.json, and
// errors.log on failure) after an outbound event send. The payload itself was
// already captured to events.jsonl at hook time; full HTTP diagnostics live in
// outbound.ndjson. Best-effort and gated by config; never blocks the send.
func recordEventSend(envJSON []byte, status int, latency time.Duration, _ int, sendErr error) {
	var probe struct {
		HookEvent string `json:"hook_event"`
	}
	_ = json.Unmarshal(envJSON, &probe)
	eventlog.RecordSendOutcome(probe.HookEvent, status, latency.Milliseconds(), sendErr)
}

// APIResponse represents a response from the API
type APIResponse struct {
	Success    bool
	StatusCode int
	Data       map[string]interface{}
	Error      string
}

// Client is the HTTP client for the PromptConduit API
type Client struct {
	config         *Config
	httpClient     *http.Client
	longHttpClient *http.Client // For large operations like transcript sync
	version        string
}

// NewClient creates a new API client. Every outbound HTTP request is
// mirrored to ~/.config/promptconduit/outbound.ndjson so users can run
// `promptconduit watch` to see what the CLI is uploading in real time.
func NewClient(config *Config, version string) *Client {
	mirror := outbound.New(filepath.Join(ConfigDir(), outbound.MirrorFileName), http.DefaultTransport)
	return &Client{
		config: config,
		httpClient: &http.Client{
			Timeout:   time.Duration(config.TimeoutSeconds) * time.Second,
			Transport: mirror,
		},
		longHttpClient: &http.Client{
			Timeout:   600 * time.Second, // 10 min for large transcript sync (chunked complete can be slow)
			Transport: mirror,
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
		_, _ = part.Write(att.Data)
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

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// envJSON holds the logical envelope (the multipart body also carries
		// binary attachments, but the event log records the envelope we sent).
		recordEventSend(envJSON, 0, time.Since(start), 1, err)
		return &APIResponse{
			Success: false,
			Error:   fmt.Sprintf("request failed: %v", err),
		}
	}
	defer func() { _ = resp.Body.Close() }()

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

	var sendErr error
	if !result.Success {
		result.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))
		sendErr = fmt.Errorf("API error: %d - %s", resp.StatusCode, string(respBody))
	}
	recordEventSend(envJSON, resp.StatusCode, time.Since(start), 1, sendErr)

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

// sendAsyncUnix uses fork to send envelope without blocking. The subprocess
// inherits a file descriptor pointing at the persistent log file as stderr,
// so any crash or panic — which the in-process logger can't catch — still
// leaves a trace on disk. Previously stderr was discarded, which made
// failed sends invisible.
func (c *Client) sendAsyncUnix(envJSON []byte) error {
	exe, err := os.Executable()
	if err != nil {
		logger.Error("async send: cannot resolve executable path: %v (falling back to blocking)", err)
		return c.sendEnvelopeBlocking(envJSON)
	}

	cmd := exec.Command(exe, "hook", "--send-event")
	cmd.Stdin = bytes.NewReader(envJSON)
	cmd.Stdout = nil
	cmd.Stderr = openLogForStderr()

	if err := cmd.Start(); err != nil {
		logger.Error("async send: cmd.Start failed: %v (falling back to blocking)", err)
		return c.sendEnvelopeBlocking(envJSON)
	}

	// Process already started, ignore error
	_ = cmd.Process.Release()

	return nil
}

// sendAsyncWindows spawns a subprocess on Windows
func (c *Client) sendAsyncWindows(envJSON []byte) error {
	exe, err := os.Executable()
	if err != nil {
		logger.Error("async send: cannot resolve executable path: %v (falling back to blocking)", err)
		return c.sendEnvelopeBlocking(envJSON)
	}

	cmd := exec.Command(exe, "hook", "--send-event")
	cmd.Stdin = bytes.NewReader(envJSON)
	cmd.Stderr = openLogForStderr()

	if err := cmd.Start(); err != nil {
		logger.Error("async send: cmd.Start failed: %v (falling back to blocking)", err)
		return c.sendEnvelopeBlocking(envJSON)
	}

	go func() {
		_ = cmd.Wait()
	}()

	return nil
}

// openLogForStderr returns a file handle suitable to assign to cmd.Stderr.
// The subprocess's runtime (panics, fatal errors that bypass the logger)
// gets routed into the same persistent log so a crashed sender isn't
// silent. Returns nil on any failure — exec then discards stderr, matching
// the prior behavior, so we never make things worse than before.
func openLogForStderr() *os.File {
	dir := logger.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(logger.Path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// sendEnvelopeBlocking sends the envelope synchronously. This is the single
// chokepoint every event send funnels through — the async subprocess
// (SendEnvelopeDirect) and the in-process fallback in sendAsync{Unix,Windows}
// both land here — so it's where we record the full outgoing payload and the
// HTTP outcome to the local event log.
func (c *Client) sendEnvelopeBlocking(envJSON []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+eventsEndpoint, bytes.NewReader(envJSON))
	if err != nil {
		recordEventSend(envJSON, 0, 0, 1, err)
		return err
	}

	c.setHeaders(req)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		recordEventSend(envJSON, 0, time.Since(start), 1, err)
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		apiErr := fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
		recordEventSend(envJSON, resp.StatusCode, time.Since(start), 1, apiErr)
		return apiErr
	}

	recordEventSend(envJSON, resp.StatusCode, time.Since(start), 1, nil)
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
	defer func() { _ = resp.Body.Close() }()

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
		"", // session id
		"", // prompt id
		[]byte(`{"test": true}`),
		nil,
	)
	return c.SendEnvelope(testEnv)
}

// TranscriptSyncResponse represents the API response for sync
type TranscriptSyncResponse struct {
	ConversationID string `json:"conversation_id"`
	MessageCount   int    `json:"message_count"`
	Status         string `json:"status"` // created, updated, skipped
	Message        string `json:"message,omitempty"`
}

// RawTranscriptSyncRequest represents the request body for raw transcript sync
// Platform performs message categorization server-side
type RawTranscriptSyncRequest struct {
	SessionID      string                 `json:"session_id"`
	Tool           string                 `json:"tool"`
	SourceFileHash string                 `json:"source_file_hash"`
	SourceFilePath string                 `json:"source_file_path,omitempty"`
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

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second) // 3 min for large transcripts
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/transcripts/sync/raw", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setHeaders(httpReq)

	resp, err := c.longHttpClient.Do(httpReq) // Use long timeout client for sync
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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

// ============================================================================
// Plan Sync
// ============================================================================

// PlanSyncRequest uploads one Claude Code plan-mode plan file.
type PlanSyncRequest struct {
	SessionID      string `json:"session_id,omitempty"` // owning session, when resolved
	Tool           string `json:"tool"`
	Filename       string `json:"filename"`
	Content        string `json:"content"`
	SourceFileHash string `json:"source_file_hash"`
	ModifiedAt     string `json:"modified_at,omitempty"`
}

// PlanSyncResponse is the platform's acknowledgement.
type PlanSyncResponse struct {
	PlanID string `json:"plan_id"`
	Status string `json:"status"` // created, updated, skipped
}

// SyncPlan uploads a plan file to POST /v1/plans/sync.
func (c *Client) SyncPlan(req *PlanSyncRequest) (*PlanSyncResponse, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/plans/sync", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var planResp PlanSyncResponse
	if err := json.Unmarshal(body, &planResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &planResp, nil
}

// ============================================================================
// Chunked Upload for Large Transcripts
// ============================================================================

// ChunkedInitRequest represents the request to initialize a chunked upload
type ChunkedInitRequest struct {
	SessionID      string `json:"session_id"`
	Tool           string `json:"tool"`
	SourceFileHash string `json:"source_file_hash"`
	SourceFilePath string `json:"source_file_path,omitempty"`
	TotalChunks    int    `json:"total_chunks"`
	TotalMessages  int    `json:"total_messages"`
}

// ChunkedInitResponse represents the response from initializing a chunked upload
type ChunkedInitResponse struct {
	UploadID string `json:"upload_id"`
}

// ChunkedUploadRequest represents a single chunk upload
type ChunkedUploadRequest struct {
	UploadID    string                 `json:"upload_id"`
	ChunkIndex  int                    `json:"chunk_index"`
	RawMessages []RawTranscriptMessage `json:"raw_messages"`
}

// ChunkedUploadResponse represents the response from uploading a chunk
type ChunkedUploadResponse struct {
	Received   bool `json:"received"`
	ChunkIndex int  `json:"chunk_index"`
}

// ChunkedCompleteRequest represents the request to complete a chunked upload
type ChunkedCompleteRequest struct {
	UploadID string `json:"upload_id"`
}

// InitChunkedUpload initializes a chunked upload session
func (c *Client) InitChunkedUpload(req *ChunkedInitRequest) (*ChunkedInitResponse, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/transcripts/sync/chunked/init", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var initResp ChunkedInitResponse
	if err := json.Unmarshal(body, &initResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &initResp, nil
}

// UploadChunk uploads a single chunk of messages
func (c *Client) UploadChunk(req *ChunkedUploadRequest) (*ChunkedUploadResponse, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // 60s per chunk
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/transcripts/sync/chunked/upload", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var uploadResp ChunkedUploadResponse
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &uploadResp, nil
}

// CompleteChunkedUpload completes a chunked upload and triggers processing
func (c *Client) CompleteChunkedUpload(req *ChunkedCompleteRequest) (*TranscriptSyncResponse, error) {
	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second) // 10 min for assembly
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/transcripts/sync/chunked/complete", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setHeaders(httpReq)

	resp, err := c.longHttpClient.Do(httpReq) // Use long timeout client for completion
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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

// ============================================================================
// GET Request Methods for Insights API
// ============================================================================

// Get performs a GET request to the API and returns the response
func (c *Client) Get(path string, query map[string]string) *APIResponse {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	url := c.config.APIURL + path
	if len(query) > 0 {
		params := make([]string, 0, len(query))
		for k, v := range query {
			params = append(params, k+"="+v)
		}
		url += "?" + strings.Join(params, "&")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
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
	defer func() { _ = resp.Body.Close() }()

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

// GetInsights retrieves the user's insights summary
func (c *Client) GetInsights(period string, repo string) *APIResponse {
	query := make(map[string]string)
	if period != "" {
		query["period"] = period
	}
	if repo != "" {
		query["repo"] = repo
	}
	return c.Get("/v1/me/insights", query)
}

// GetInsightsTools retrieves tool usage breakdown
func (c *Client) GetInsightsTools(period string, repo string) *APIResponse {
	query := make(map[string]string)
	if period != "" {
		query["period"] = period
	}
	if repo != "" {
		query["repo"] = repo
	}
	return c.Get("/v1/me/insights/tools", query)
}

// GetInsightsErrors retrieves error patterns
func (c *Client) GetInsightsErrors(period string, repo string) *APIResponse {
	query := make(map[string]string)
	if period != "" {
		query["period"] = period
	}
	if repo != "" {
		query["repo"] = repo
	}
	return c.Get("/v1/me/insights/errors", query)
}

// GetSessions retrieves recent sessions list
func (c *Client) GetSessions(limit int, offset int, repo string) *APIResponse {
	query := make(map[string]string)
	if limit > 0 {
		query["limit"] = fmt.Sprintf("%d", limit)
	}
	if offset > 0 {
		query["offset"] = fmt.Sprintf("%d", offset)
	}
	if repo != "" {
		query["repo"] = repo
	}
	return c.Get("/v1/me/sessions", query)
}

// GetSkills retrieves the user's detected skills with optional filters.
// approved: "true", "false", or "" (all); skillType: "workflow", "command", etc.
// repoName: filter to a specific project (empty = all scopes).
func (c *Client) GetSkills(approved string, skillType string, limit int, repoName string) *APIResponse {
	query := make(map[string]string)
	if approved != "" {
		query["approved"] = approved
	}
	if skillType != "" {
		query["skill_type"] = skillType
	}
	if limit > 0 {
		query["limit"] = fmt.Sprintf("%d", limit)
	}
	if repoName != "" {
		query["repo"] = repoName
	}
	return c.Get("/v1/skills", query)
}

// GenerateSkills triggers skill detection on the platform.
// repoName: scope to a specific project (empty = global).
// Uses the long HTTP client (10 min) since AI analysis can take > 30s.
func (c *Client) GenerateSkills(force bool, repoName string) *APIResponse {
	body := map[string]interface{}{"force": force}
	if repoName != "" {
		body["repo"] = repoName
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("failed to marshal payload: %v", err)}
	}

	req, err := http.NewRequest("POST", c.config.APIURL+"/v1/skills/generate", bytes.NewReader(jsonData))
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("failed to create request: %v", err)}
	}

	c.setHeaders(req)

	resp, err := c.longHttpClient.Do(req)
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body2, err := io.ReadAll(resp.Body)
	if err != nil {
		return &APIResponse{Success: false, StatusCode: resp.StatusCode, Error: fmt.Sprintf("failed to read response: %v", err)}
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body2, &data); err != nil {
		return &APIResponse{Success: false, StatusCode: resp.StatusCode, Error: fmt.Sprintf("failed to parse response: %v", err)}
	}

	if resp.StatusCode >= 400 {
		var errMsg string
		if detail, ok := data["detail"].(string); ok {
			errMsg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, detail)
		} else {
			errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &APIResponse{Success: false, StatusCode: resp.StatusCode, Error: errMsg, Data: data}
	}

	return &APIResponse{Success: true, StatusCode: resp.StatusCode, Data: data}
}

// GetSkillPatterns returns detected prompt clusters for the user.
// repoName: scope to a specific project (empty = global).
func (c *Client) GetSkillPatterns(repoName string) *APIResponse {
	if repoName == "" {
		return c.Get("/v1/skills/patterns", nil)
	}
	return c.Get("/v1/skills/patterns", map[string]string{"repo": repoName})
}

// ApproveSkill approves or rejects a skill by ID.
func (c *Client) ApproveSkill(id string, approve bool) *APIResponse {
	return c.patchRequest(fmt.Sprintf("/v1/skills/%s", id), map[string]interface{}{"is_approved": approve})
}

// GetSkill fetches a single skill by ID. Returns 404 if not found or
// already soft-deleted.
func (c *Client) GetSkill(id string) *APIResponse {
	return c.Get(fmt.Sprintf("/v1/skills/%s", id), nil)
}

// DeleteSkill soft-deletes a skill by ID (sets is_active=false on the
// platform). The row remains in the DB for audit but disappears from
// listings. Wrapped in a method so we can later add confirmation hooks
// or soft-vs-hard-delete options without touching command code.
func (c *Client) DeleteSkill(id string) *APIResponse {
	return c.deleteRequest(fmt.Sprintf("/v1/skills/%s", id))
}

// GetSkillCommandFile fetches the raw markdown command file for a skill.
// Returns the file content suitable for writing to ~/.claude/commands/<name>.md
func (c *Client) GetSkillCommandFile(id string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", c.config.APIURL+fmt.Sprintf("/v1/skills/%s/command", id), nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return string(body), nil
}

// deleteRequest sends a DELETE request with no body.
func (c *Client) deleteRequest(path string) *APIResponse {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "DELETE", c.config.APIURL+path, nil)
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("failed to create request: %v", err)}
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	result := &APIResponse{StatusCode: resp.StatusCode, Success: resp.StatusCode >= 200 && resp.StatusCode < 300}
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

// patchRequest sends a PATCH request with a JSON payload
func (c *Client) patchRequest(path string, payload interface{}) *APIResponse {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("failed to marshal payload: %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "PATCH", c.config.APIURL+path, bytes.NewReader(jsonData))
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("failed to create request: %v", err)}
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &APIResponse{Success: false, Error: fmt.Sprintf("request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	result := &APIResponse{StatusCode: resp.StatusCode, Success: resp.StatusCode >= 200 && resp.StatusCode < 300}
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
