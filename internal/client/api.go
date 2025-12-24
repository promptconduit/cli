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

// SendPromptWithImages sends a prompt with images using multipart/form-data
func (c *Client) SendPromptWithImages(metadata *PromptMetadata, images []transcript.Image) *APIResponse {
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

	// Add images as file attachments
	for _, img := range images {
		part, err := writer.CreateFormFile("attachments[]", img.Filename)
		if err != nil {
			continue
		}
		part.Write(img.Data)
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

// SendPromptWithImagesAsync sends a prompt with images asynchronously
func (c *Client) SendPromptWithImagesAsync(metadata *PromptMetadata, images []transcript.Image) error {
	// For multipart, we need to serialize everything
	data := struct {
		Metadata *PromptMetadata     `json:"metadata"`
		Images   []SerializedImage   `json:"images"`
	}{
		Metadata: metadata,
		Images:   make([]SerializedImage, len(images)),
	}

	for i, img := range images {
		data.Images[i] = SerializedImage{
			Data:      img.Data,
			MediaType: img.MediaType,
			Filename:  img.Filename,
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

// SerializedImage is used for JSON serialization of images
type SerializedImage struct {
	Data      []byte `json:"data"`
	MediaType string `json:"media_type"`
	Filename  string `json:"filename"`
}

// sendPromptAsyncUnix spawns a subprocess to send the prompt
func (c *Client) sendPromptAsyncUnix(jsonData []byte) error {
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
		Metadata *PromptMetadata   `json:"metadata"`
		Images   []SerializedImage `json:"images"`
	}
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return err
	}

	images := make([]transcript.Image, len(data.Images))
	for i, img := range data.Images {
		images[i] = transcript.Image{
			Data:      img.Data,
			MediaType: img.MediaType,
			Filename:  img.Filename,
		}
	}

	result := c.SendPromptWithImages(data.Metadata, images)
	if !result.Success {
		return fmt.Errorf("API error: %s", result.Error)
	}
	return nil
}

// SendPromptDirect sends a serialized prompt directly (used by async subprocess)
func (c *Client) SendPromptDirect(jsonData []byte) error {
	return c.sendPromptBlocking(jsonData)
}
