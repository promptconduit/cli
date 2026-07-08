package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestDeviceCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/device/code" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"device_code":              "dev-code-123",
			"user_code":                "ABCD-EFGH",
			"verification_uri":         "https://app.example.com/device?code=ABCD-EFGH",
			"verification_uri_complete": "https://app.example.com/device?code=ABCD-EFGH",
			"expires_in":               900,
			"interval":                 5,
		})
	}))
	defer srv.Close()

	resp, err := RequestDeviceCode(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if resp.DeviceCode != "dev-code-123" {
		t.Fatalf("device_code = %q", resp.DeviceCode)
	}
	if resp.UserCode != "ABCD-EFGH" {
		t.Fatalf("user_code = %q", resp.UserCode)
	}
}

func TestPollDeviceToken_Success(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.Header().Set("Content-Type", "application/json")
		if polls < 2 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "authorization_pending",
				"error_description": "Authorization pending",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"api_key":    "sk_testkey123",
			"token_type": "Bearer",
		})
	}))
	defer srv.Close()

	key, err := PollDeviceToken(srv.URL, "test", "dev-code", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk_testkey123" {
		t.Fatalf("api_key = %q", key)
	}
	if polls < 2 {
		t.Fatalf("expected at least 2 polls, got %d", polls)
	}
}

func TestSaveAPIKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	if err := SaveAPIKey("sk_test_save_key"); err != nil {
		t.Fatal(err)
	}

	cfg := LoadConfig()
	if cfg.APIKey != "sk_test_save_key" {
		t.Fatalf("api_key = %q", cfg.APIKey)
	}
	if cfg.LocalOnly {
		t.Fatal("expected local_only to be false after login")
	}
}
