package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DeviceCodeResponse is returned by POST /v1/auth/device/code.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceTokenResponse is returned on successful device authorization.
type DeviceTokenResponse struct {
	APIKey    string `json:"api_key"`
	TokenType string `json:"token_type"`
}

// DeviceTokenError is an OAuth-style polling error.
type DeviceTokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval,omitempty"`
}

// RequestDeviceCode starts the RFC 8628 device authorization flow.
func RequestDeviceCode(apiURL, version string) (*DeviceCodeResponse, error) {
	body, err := postJSON(apiURL+"/v1/auth/device/code", map[string]string{
		"client_id": "cli",
	}, version, "")
	if err != nil {
		return nil, err
	}

	var resp DeviceCodeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid device code response: %w", err)
	}
	if resp.DeviceCode == "" || resp.UserCode == "" {
		return nil, fmt.Errorf("incomplete device code response")
	}
	return &resp, nil
}

// PollDeviceToken polls POST /v1/auth/device/token until authorized or timeout.
func PollDeviceToken(apiURL, version, deviceCode string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	interval := 5 * time.Second

	for time.Now().Before(deadline) {
		body, status, err := postJSONWithStatus(apiURL+"/v1/auth/device/token", map[string]string{
			"device_code": deviceCode,
			"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
		}, version, "")
		if err != nil {
			return "", err
		}

		// OAuth-style pending/slow_down responses use HTTP 400
		if status == 400 {
			var tokenErr DeviceTokenError
			if err := json.Unmarshal(body, &tokenErr); err == nil && tokenErr.Error != "" {
				switch tokenErr.Error {
				case "authorization_pending":
					if tokenErr.Interval > 0 {
						interval = time.Duration(tokenErr.Interval) * time.Second
					}
					time.Sleep(interval)
					continue
				case "slow_down":
					if tokenErr.Interval > 0 {
						interval = time.Duration(tokenErr.Interval) * time.Second
					} else {
						interval += 5 * time.Second
					}
					time.Sleep(interval)
					continue
				case "expired_token":
					return "", fmt.Errorf("device code expired — run `promptconduit login` again")
				case "access_denied":
					return "", fmt.Errorf("authorization denied")
				default:
					return "", fmt.Errorf("authorization failed: %s", tokenErr.ErrorDescription)
				}
			}
			return "", fmt.Errorf("HTTP %d: %s", status, string(body))
		}

		if status < 200 || status >= 300 {
			return "", fmt.Errorf("HTTP %d: %s", status, string(body))
		}

		var tokenResp DeviceTokenResponse
		if err := json.Unmarshal(body, &tokenResp); err != nil {
			return "", fmt.Errorf("invalid token response: %w", err)
		}
		if tokenResp.APIKey != "" {
			return tokenResp.APIKey, nil
		}

		time.Sleep(interval)
	}

	return "", fmt.Errorf("authorization timed out — complete sign-in in your browser and run `promptconduit login` again")
}

func postJSON(url string, payload interface{}, version, bearerToken string) ([]byte, error) {
	body, status, err := postJSONWithStatus(url, payload, version, bearerToken)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return body, fmt.Errorf("HTTP %d: %s", status, string(body))
	}
	return body, nil
}

func postJSONWithStatus(url string, payload interface{}, version, bearerToken string) ([]byte, int, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonData))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("PromptConduit-CLI/%s", version))
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read response: %w", err)
	}

	return body, resp.StatusCode, nil
}

// SaveAPIKey persists the API key to config and disables local-only mode.
func SaveAPIKey(apiKey string) error {
	fc, err := LoadFileConfig()
	if err != nil {
		return err
	}
	if fc == nil {
		fc = &FileConfig{}
	}
	fc.APIKey = apiKey
	fc.LocalOnly = false
	return SaveFileConfig(fc)
}
