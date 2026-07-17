package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// isolateConfig points config resolution at an empty temp dir so the test never
// reads the developer's real ~/.config/promptconduit (API key, URL).
func isolateConfig(t *testing.T, apiURL string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PROMPTCONDUIT_API_URL", apiURL)
	t.Setenv("PROMPTCONDUIT_API_KEY", "")
}

func TestFeedback_PostsMessage(t *testing.T) {
	var got map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	isolateConfig(t, srv.URL)

	feedbackCategory = "idea"
	t.Cleanup(func() { feedbackCategory = "" })

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runFeedback(cmd, []string{"add", "filtering"}); err != nil {
		t.Fatalf("runFeedback: %v", err)
	}

	if got["source"] != "cli" || got["message"] != "add filtering" || got["category"] != "idea" {
		t.Fatalf("server received %+v", got)
	}
	ctx, _ := got["context"].(map[string]any)
	if ctx == nil || ctx["os"] == "" || ctx["cli_version"] == "" {
		t.Fatalf("missing context: %+v", got["context"])
	}
	if gotAuth != "" {
		t.Fatalf("expected no auth header for keyless config, got %q", gotAuth)
	}
	if !strings.Contains(out.String(), "Thanks") {
		t.Fatalf("no confirmation printed: %q", out.String())
	}
}

func TestFeedback_NoMessageErrors(t *testing.T) {
	isolateConfig(t, "http://127.0.0.1:0")
	feedbackCategory = ""
	if err := runFeedback(&cobra.Command{}, nil); err == nil {
		t.Fatal("expected an error when no message is given")
	}
}

func TestFeedback_TooLongErrors(t *testing.T) {
	isolateConfig(t, "http://127.0.0.1:0")
	feedbackCategory = ""
	err := runFeedback(&cobra.Command{}, []string{strings.Repeat("x", 4001)})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected too-long error, got %v", err)
	}
}
