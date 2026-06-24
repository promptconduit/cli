package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRunUpgrade_DevBuildSkips covers verification item 6 from
// https://github.com/promptconduit/cli/issues/73: a development build
// (Version == "dev") must not attempt a network check or a self-replace. It
// prints a skip notice and returns nil — without --force it never reaches
// updater.CheckLatest, so this test makes no network calls.
func TestRunUpgrade_DevBuildSkips(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })
	Version = "dev"

	// Ensure the package-level flags are in their default state.
	origForce, origCheck := upgradeForce, upgradeCheckOnly
	t.Cleanup(func() { upgradeForce, upgradeCheckOnly = origForce, origCheck })
	upgradeForce = false
	upgradeCheckOnly = false

	cmd := &cobra.Command{Use: "upgrade"}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := runUpgrade(cmd, nil); err != nil {
		t.Fatalf("runUpgrade on dev build returned error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "development build") {
		t.Errorf("expected a dev-build skip notice, got: %q", got)
	}
	if !strings.Contains(got, "skipping upgrade") {
		t.Errorf("expected 'skipping upgrade' in output, got: %q", got)
	}
	// It must not have started a real check/upgrade flow.
	if strings.Contains(got, "Checking") || strings.Contains(got, "Downloading") {
		t.Errorf("dev build should not check or download; output: %q", got)
	}
}
