package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/promptconduit/cli/internal/client"
)

// TestTestFailureNamesNextStep: `promptconduit test` used to end with the raw
// reply ("HTTP 401: {...json...}") and no way forward.
func TestTestFailureNamesNextStep(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"revoked key", 401, `{"title":"Unauthorized","status":401,"detail":"Invalid API key"}`,
			[]string{"HTTP 401: Invalid API key", "promptconduit login"}},
		{"forbidden", 403, `{"title":"Forbidden","status":403}`,
			[]string{"HTTP 403: Forbidden", "promptconduit login"}},
		{"server error", 500, `{"title":"Internal Server Error","status":500}`,
			[]string{"HTTP 500", "Try again"}},
		{"rate limited", 429, `slow down`,
			[]string{"HTTP 429: slow down", "Rate limited"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			cfg := &client.Config{APIKey: "k", APIURL: srv.URL, TimeoutSeconds: 5}
			err := testFailure(client.NewClient(cfg, "test").TestConnection(), srv.URL)
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestTestFailureUnreachable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	url := "http://127.0.0.1:1"
	cfg := &client.Config{APIKey: "k", APIURL: url, TimeoutSeconds: 2}
	err := testFailure(client.NewClient(cfg, "test").TestConnection(), url)
	for _, w := range []string{"Could not reach " + url, "promptconduit config show"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not contain %q", err, w)
		}
	}
}

func TestAuthFailureHint(t *testing.T) {
	const auth = `API error: 401 - {"title":"Unauthorized"}`
	cases := []struct {
		name               string
		lastErr, errAt, ok string
		want               bool
	}{
		{"still failing on auth", auth, "2026-11-05T10:00:00Z", "2026-11-05T09:00:00Z", true},
		{"never succeeded", auth, "2026-11-05T10:00:00Z", "", true},
		{"retried send", auth + " (after 3 attempts)", "2026-11-05T10:00:00Z", "", true},
		{"forbidden", `API error: 403 - {}`, "2026-11-05T10:00:00Z", "", true},
		{"recovered since", auth, "2026-11-05T10:00:00Z", "2026-11-05T11:00:00Z", false},
		{"not an auth error", `API error: 500 - {}`, "2026-11-05T10:00:00Z", "", false},
		{"network error", `Post "https://x/v1/events/raw": net/http: TLS handshake timeout`, "2026-11-05T10:00:00Z", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := authFailureHint(tc.lastErr, tc.errAt, tc.ok)
			if (got != "") != tc.want {
				t.Fatalf("hint = %q, want hint: %v", got, tc.want)
			}
			if got != "" && !strings.Contains(got, "promptconduit login") {
				t.Errorf("hint %q does not say how to fix it", got)
			}
		})
	}
}
