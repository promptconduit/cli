package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/telemetry"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledHookTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Installed: hooks reference promptconduit.
	writeFile(t, filepath.Join(home, ".claude", "settings.json"),
		`{"hooks":{"Stop":[{"hooks":[{"command":"/usr/local/bin/promptconduit hook"}]}]}}`)
	writeFile(t, filepath.Join(home, ".copilot", "hooks", "pc.json"),
		`{"command":"promptconduit hook"}`)
	writeFile(t, filepath.Join(home, ".grok", "hooks", GrokHookFile),
		`{"command":"promptconduit hook"}`)
	// Not installed: promptconduit mentioned outside "hooks", or not at all.
	writeFile(t, filepath.Join(home, ".gemini", "settings.json"),
		`{"note":"promptconduit","hooks":{"x":"other-tool"}}`)
	writeFile(t, filepath.Join(home, ".cursor", "hooks.json"), `{not json`)

	got := installedHookTools()
	want := []string{"claude-code", "copilot", "grok"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("installedHookTools() = %v, want %v", got, want)
	}
}

func setupTelemetryEnv(t *testing.T) string {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	for _, k := range []string{"DO_NOT_TRACK", telemetry.EnvTelemetry, "CI", "PROMPTCONDUIT_AUTO_UPDATE_CHILD"} {
		t.Setenv(k, "")
	}
	prev := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = prev })
	return client.ConfigDir()
}

func TestMaybeTelemetryDoesNotShowNoticeWithoutTerminal(t *testing.T) {
	dir := setupTelemetryEnv(t)

	// go test's stderr isn't a terminal, so the notice must not be recorded
	// as shown and nothing may be written or sent.
	maybeTelemetry(statusCmd)

	if _, err := os.Stat(telemetry.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("state file should not exist without a terminal, stat err = %v", err)
	}
}

func TestMaybeSpawnTelemetryWaitsForNotice(t *testing.T) {
	dir := setupTelemetryEnv(t)

	maybeSpawnTelemetry(&client.Config{})

	if _, err := os.Stat(telemetry.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("hook must not claim an attempt before the notice was shown, stat err = %v", err)
	}
}

func TestMaybeSpawnTelemetryRespectsOptOut(t *testing.T) {
	dir := setupTelemetryEnv(t)
	st := &telemetry.State{}
	st.MarkNoticeShown()
	if err := telemetry.Save(dir, st); err != nil {
		t.Fatal(err)
	}

	maybeSpawnTelemetry(&client.Config{DisableTelemetry: true})

	if !telemetry.Load(dir).LastAttemptAt.IsZero() {
		t.Fatal("opted-out hook must not record an attempt")
	}
}

func TestTelemetryFailsClosedOnUnreadableConfig(t *testing.T) {
	dir := setupTelemetryEnv(t)
	if !telemetryEnabled(&client.Config{}) {
		t.Fatal("expected telemetry enabled with no config file")
	}
	writeFile(t, filepath.Join(dir, client.ConfigFileName), `{"disable_telemetry": true,`)
	if telemetryEnabled(&client.Config{}) {
		t.Fatal("a config file that can't be parsed must disable telemetry")
	}
}

func TestTelemetrySkippedForConfigAndInternalCommands(t *testing.T) {
	for _, c := range []string{"config", "hook", "telemetry-ping"} {
		cmd, _, err := rootCmd.Find([]string{c})
		if err != nil {
			t.Fatalf("find %s: %v", c, err)
		}
		if c != "config" && !skipUpdateCheckFor(cmd) {
			t.Fatalf("%s should be in the skip list", c)
		}
		if c == "config" && commandPathRoot(cmd) != "config" {
			t.Fatalf("commandPathRoot(config) = %q", commandPathRoot(cmd))
		}
	}
}
