package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/promptconduit/cli/internal/schema"
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

// SendEvent sends a single event to the API (blocking)
func (c *Client) SendEvent(event *schema.CanonicalEvent) *APIResponse {
	return c.sendRequest("/v1/events/ingest", event)
}

// SendEventAsync sends an event asynchronously without blocking
// On Unix systems, it forks a new process. On Windows, it spawns a subprocess.
func (c *Client) SendEventAsync(event *schema.CanonicalEvent) error {
	eventJSON, err := event.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize event: %w", err)
	}

	if runtime.GOOS == "windows" {
		return c.sendAsyncWindows(eventJSON)
	}

	return c.sendAsyncUnix(eventJSON)
}

// sendAsyncUnix uses fork to send event without blocking
func (c *Client) sendAsyncUnix(eventJSON []byte) error {
	// Fork by executing ourselves with a special flag
	exe, err := os.Executable()
	if err != nil {
		// Fall back to blocking send
		return c.sendEventBlocking(eventJSON)
	}

	// Spawn a detached subprocess to send the event
	cmd := exec.Command(exe, "hook", "--send-event")
	cmd.Stdin = bytes.NewReader(eventJSON)
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Start the process but don't wait for it
	if err := cmd.Start(); err != nil {
		// Fall back to blocking send
		return c.sendEventBlocking(eventJSON)
	}

	// Release the process so it runs independently
	if err := cmd.Process.Release(); err != nil {
		// Process already started, ignore error
	}

	return nil
}

// sendAsyncWindows spawns a subprocess on Windows
func (c *Client) sendAsyncWindows(eventJSON []byte) error {
	// On Windows, we can't easily detach, so we just spawn and don't wait
	exe, err := os.Executable()
	if err != nil {
		return c.sendEventBlocking(eventJSON)
	}

	cmd := exec.Command(exe, "hook", "--send-event")
	cmd.Stdin = bytes.NewReader(eventJSON)

	if err := cmd.Start(); err != nil {
		return c.sendEventBlocking(eventJSON)
	}

	// Don't wait for the process
	go func() {
		_ = cmd.Wait()
	}()

	return nil
}

// sendEventBlocking sends the event synchronously (fallback)
func (c *Client) sendEventBlocking(eventJSON []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.TimeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.config.APIURL+"/v1/events/ingest", bytes.NewReader(eventJSON))
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

// SendEventBatch sends multiple events in one request
func (c *Client) SendEventBatch(events []*schema.CanonicalEvent) *APIResponse {
	payload := map[string]interface{}{
		"events": events,
	}
	return c.sendRequest("/v1/events/ingest-batch", payload)
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

// SendEventDirect sends an event directly (used by async subprocess)
func (c *Client) SendEventDirect(eventJSON []byte) error {
	return c.sendEventBlocking(eventJSON)
}
