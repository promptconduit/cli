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

	"github.com/promptconduit/cli/internal/envelope"
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
