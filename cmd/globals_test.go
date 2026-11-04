package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptconduit/cli/internal/logger"
	"github.com/promptconduit/cli/internal/outbound"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// runCLI executes the real command tree with args against an isolated HOME
// and returns stdout, stderr and the error Execute returned.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PROMPTCONDUIT_AUTO_UPDATE", "0")
	t.Setenv("DO_NOT_TRACK", "1")
	t.Setenv("NO_COLOR", "")
	t.Setenv("PROMPTCONDUIT_DEBUG", "")
	t.Setenv(envNoInput, "")
	t.Setenv(envQuiet, "")

	var out, errb bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errb)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		resetFlags(rootCmd)
	})
	err := Execute()
	return out.String(), errb.String(), err
}

// resetFlags puts every flag back to its default so tests don't leak state
// through cobra's package-level command tree.
func resetFlags(c *cobra.Command) {
	reset := func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	}
	c.Flags().VisitAll(reset)
	c.PersistentFlags().VisitAll(reset)
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

func TestVersionFlag(t *testing.T) {
	out, _, err := runCLI(t, "--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if want := "promptconduit " + Version + "\n"; out != want {
		t.Fatalf("--version stdout = %q, want %q", out, want)
	}
}

func TestVersionCommandWritesStdout(t *testing.T) {
	out, errOut, err := runCLI(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "promptconduit "+Version) || errOut != "" {
		t.Fatalf("version: stdout=%q stderr=%q", out, errOut)
	}
}

func TestUsageErrorsPrintHintNotUsage(t *testing.T) {
	cases := [][]string{
		{"sessions", "--bogus"},           // flag error
		{"uninstall"},                     // wrong arg count
		{"sessions", "resolve", "--json"}, // missing required flag
	}
	for _, args := range cases {
		_, errOut, err := runCLI(t, args...)
		if err == nil {
			t.Fatalf("%v: expected an error", args)
		}
		if !strings.Contains(errOut, "--help' for usage.") {
			t.Errorf("%v: stderr lacks --help hint:\n%s", args, errOut)
		}
		if strings.Contains(errOut, "Usage:") {
			t.Errorf("%v: stderr dumps full usage:\n%s", args, errOut)
		}
	}
}

func TestRuntimeErrorHasNoUsageOrHint(t *testing.T) {
	_, errOut, err := runCLI(t, "uninstall", "bogus-tool")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(errOut, "Usage:") || strings.Contains(errOut, "for usage") {
		t.Fatalf("runtime error should print only the message:\n%s", errOut)
	}
	if !strings.Contains(errOut, `unknown tool "bogus-tool"`) {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestRootHelpListsOnlySupportedTools(t *testing.T) {
	out, _, err := runCLI(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Supported tools: Claude Code and Cursor.") {
		t.Fatalf("root help doesn't state the supported tools:\n%s", out)
	}
	if !strings.Contains(out, "Experimental (install with --experimental)") {
		t.Fatalf("root help doesn't mark experimental tools:\n%s", out)
	}
}

func TestVisibleCommandsHaveExamples(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if c.Example == "" && !strings.Contains(c.Long, "  promptconduit "+c.Name()) {
			t.Errorf("%q has no examples (set Example in examples.go)", c.Name())
		}
	}
}

func TestGlobalFlagsRegistered(t *testing.T) {
	for _, name := range []string{"no-input", "no-color", "quiet", "debug"} {
		if rootCmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("missing global flag --%s", name)
		}
	}
}

func TestCanPromptHonorsNoInput(t *testing.T) {
	t.Setenv(envNoInput, "1")
	if canPrompt() {
		t.Fatal("canPrompt() = true with PROMPTCONDUIT_NO_INPUT=1")
	}
}

func TestColorEnabledHonorsNoColor(t *testing.T) {
	// A pipe is never a terminal, so this only proves NO_COLOR can't turn
	// color on; the env check runs first either way.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	t.Setenv("NO_COLOR", "1")
	if outbound.ColorEnabled(w) {
		t.Fatal("ColorEnabled with NO_COLOR set")
	}
}

func TestReadSecret(t *testing.T) {
	got, err := readSecret(strings.NewReader("  sk_test_123 \nignored\n"), "API key")
	if err != nil || got != "sk_test_123" {
		t.Fatalf("readSecret = %q, %v", got, err)
	}
	if _, err := readSecret(strings.NewReader(""), "API key"); err == nil {
		t.Fatal("readSecret on empty input should fail")
	}
}

func TestConfigSetAPIKeyStdin(t *testing.T) {
	prev := secretInput
	secretInput = strings.NewReader("sk_from_stdin\n")
	t.Cleanup(func() { secretInput = prev })

	_, errOut, err := runCLI(t, "config", "set", "--api-key-stdin")
	if err != nil {
		t.Fatalf("config set --api-key-stdin: %v\n%s", err, errOut)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "promptconduit", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sk_from_stdin") {
		t.Fatalf("API key not saved: %s", data)
	}
}

func TestAPIKeyFlagsMutuallyExclusive(t *testing.T) {
	_, _, err := runCLI(t, "config", "set", "--api-key", "x", "--api-key-stdin")
	if err == nil {
		t.Fatal("expected --api-key and --api-key-stdin to conflict")
	}
}

func TestStatusJSONAndExitCode(t *testing.T) {
	out, _, err := runCLI(t, "status", "--json")
	if err != errNothingInstalled {
		t.Fatalf("status with nothing installed: err = %v, want errNothingInstalled", err)
	}
	var r statusReport
	if jerr := json.Unmarshal([]byte(out), &r); jerr != nil {
		t.Fatalf("status --json isn't JSON: %v\n%s", jerr, out)
	}
	if r.Ready || r.Mode != "local-only" || len(r.Tools) != 6 {
		t.Fatalf("unexpected report: %+v", r)
	}
	if r.Tools[0].Tool != "claude-code" || r.Tools[0].Experimental {
		t.Fatalf("claude-code first and supported, got %+v", r.Tools[0])
	}
	if !r.Tools[2].Experimental {
		t.Fatalf("gemini-cli should be experimental, got %+v", r.Tools[2])
	}
}

func TestStatusReadyWhenHooksInstalled(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/promptconduit hook"}]}]}}`
	if err := os.WriteFile(settings, []byte(hooks), 0o644); err != nil {
		t.Fatal(err)
	}
	tools := inspectToolInstalls(home)
	if !tools[0].Installed || tools[0].Hooks != 1 {
		t.Fatalf("claude-code = %+v", tools[0])
	}
	if got := tools[0].describe(); got != "Installed (1 hooks)" {
		t.Fatalf("describe = %q", got)
	}
}

// A global --debug on `config set` must not persist debug mode (it used to be
// shadowed by config set's own --debug flag); saving it is --debug-mode.
func TestGlobalDebugDoesNotPersistDebugMode(t *testing.T) {
	t.Cleanup(func() { logger.SetEcho(nil); logger.SetDebug(false); flagDebug = false })
	_, _, err := runCLI(t, "--debug", "config", "set", "--api-url=http://localhost:8787")
	if err != nil {
		t.Fatalf("config set: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "promptconduit", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"debug": true`) || strings.Contains(string(data), `"debug":true`) {
		t.Fatalf("global --debug saved debug mode: %s", data)
	}
}

func TestGlobalDebugAloneOnConfigSetHints(t *testing.T) {
	t.Cleanup(func() { logger.SetEcho(nil); logger.SetDebug(false); flagDebug = false })
	_, errOut, err := runCLI(t, "config", "set", "--debug")
	if err == nil || !strings.Contains(errOut, "--debug-mode") {
		t.Fatalf("want a --debug-mode hint, got err=%v stderr=%q", err, errOut)
	}
}

// Global flags must not leak into child processes (resume execs the AI tool).
func TestGlobalFlagsDoNotExportEnv(t *testing.T) {
	t.Cleanup(func() { logger.SetEcho(nil); logger.SetDebug(false) })
	_, _, _ = runCLI(t, "--debug", "--no-color", "--no-input", "-q", "version")
	for _, name := range []string{"PROMPTCONDUIT_DEBUG", "NO_COLOR", envNoInput, envQuiet} {
		if v := os.Getenv(name); v != "" {
			t.Errorf("%s=%q exported by a global flag", name, v)
		}
	}
}
