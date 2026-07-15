package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The async sender is a three-process story: this test spawns the CLI's `hook`
// command (the parent), which spawns a detached `hook --send-event` child that
// does the actual POST. The bug in #124 only reproduces when the PARENT EXITS
// right after Start()+Release(), so an in-process unit test cannot see it —
// the copy goroutine survives as long as the test binary does. Hence the real
// binary and the real hook path.

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// buildCLI builds the promptconduit binary once per test run and returns its
// path.
func buildCLI(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			buildErr = fmt.Errorf("cannot resolve test file path")
			return
		}
		moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

		outDir, err := os.MkdirTemp("", "pc-asyncsend-bin")
		if err != nil {
			buildErr = err
			return
		}
		bin := filepath.Join(outDir, "promptconduit")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = moduleRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build failed: %v\n%s", err, out)
			return
		}
		builtBin = bin
	})
	if buildErr != nil {
		t.Fatalf("building CLI: %v", buildErr)
	}
	return builtBin
}

// asyncHookEnv returns a hermetic environment for the hook subprocess: its own
// HOME (so the event log lands in a temp dir) and a config pointing at srvURL.
func asyncHookEnv(t *testing.T, srvURL string) (home string, env []string) {
	t.Helper()
	home = t.TempDir()

	xdg := filepath.Join(home, ".config")
	cfgDir := filepath.Join(xdg, ConfigDirName)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := FileConfig{
		APIKey:            "test-key",
		APIURL:            srvURL,
		Timeout:           30,
		DisableAutoUpdate: true,
	}
	data, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, ConfigFileName), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Minimal env: no inherited PROMPTCONDUIT_* vars can leak in and point the
	// subprocess at a real API.
	env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + xdg,
		"PATH=" + os.Getenv("PATH"),
	}
	return home, env
}

// runHookWithPayload runs `promptconduit hook` with payload on stdin and waits
// for it to exit — mirroring what an AI tool's hook does. When it returns, the
// parent is gone and only the detached sender child remains.
func runHookWithPayload(t *testing.T, bin string, env []string, dir string, payload []byte) {
	t.Helper()
	cmd := exec.Command(bin, "hook")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook exited with error: %v\n%s", err, out)
	}
}

// bigHookPayload builds a native hook payload whose serialized envelope is far
// larger than the 64KB OS pipe buffer.
func bigHookPayload(t *testing.T, filler int) []byte {
	t.Helper()
	payload := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "async-send-test-session",
		"cwd":             "/tmp",
		// A single large field is enough; the envelope carries raw_event verbatim.
		"tool_input": map[string]interface{}{
			"content": strings.Repeat("x", filler),
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return data
}

// TestAsyncSendDeliversEnvelopeLargerThanPipeBuffer is the regression test for
// #124. Before the fix the detached child received only the ~64KB that fit in
// the OS pipe buffer, so the platform saw truncated JSON and returned 400.
func TestAsyncSendDeliversEnvelopeLargerThanPipeBuffer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("async send on Windows takes the sendAsyncWindows path; covered by the same assertions on unix")
	}

	bin := buildCLI(t)

	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case bodies <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	home, env := asyncHookEnv(t, srv.URL)

	const filler = 1 << 20 // 1MB — 16x the 64KB pipe buffer
	runHookWithPayload(t, bin, env, home, bigHookPayload(t, filler))

	var body []byte
	select {
	case body = <-bodies:
	case <-time.After(30 * time.Second):
		t.Fatal("no request reached the server within 30s")
	}

	if !json.Valid(body) {
		t.Fatalf("server received invalid JSON: %d bytes (truncated envelope — #124). "+
			"tail: %q", len(body), tail(body, 80))
	}

	var env2 struct {
		EventID  string          `json:"event_id"`
		RawEvent json.RawMessage `json:"raw_event"`
	}
	if err := json.Unmarshal(body, &env2); err != nil {
		t.Fatalf("received body is not an envelope: %v", err)
	}
	if env2.EventID == "" {
		t.Error("envelope has no event_id")
	}
	var raw struct {
		ToolInput struct {
			Content string `json:"content"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(env2.RawEvent, &raw); err != nil {
		t.Fatalf("raw_event is not valid JSON: %v", err)
	}
	if got := len(raw.ToolInput.Content); got != filler {
		t.Errorf("raw_event content = %d bytes, want %d (payload was truncated in transit)", got, filler)
	}
}

// TestAsyncSendSmallEnvelopeStillWorks guards the path that always worked:
// envelopes that fit in the pipe buffer must keep arriving intact.
func TestAsyncSendSmallEnvelopeStillWorks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered on unix")
	}

	bin := buildCLI(t)

	bodies := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case bodies <- body:
		default:
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	home, env := asyncHookEnv(t, srv.URL)
	runHookWithPayload(t, bin, env, home, bigHookPayload(t, 16))

	select {
	case body := <-bodies:
		if !json.Valid(body) {
			t.Fatalf("small envelope arrived invalid: %q", tail(body, 80))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no request reached the server within 30s")
	}
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "..." + string(b[len(b)-n:])
}
