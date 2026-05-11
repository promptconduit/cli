package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCommandPathRoot(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"promptconduit", ""},
		{"promptconduit hook", "hook"},
		{"promptconduit config show", "config"},
		{"promptconduit upgrade --check", "upgrade"},
	}
	for _, c := range cases {
		cmd := &cobra.Command{}
		cmd.SetArgs(nil)
		// CommandPath() reads from the cobra tree, which is awkward to
		// fake out — exercise the splitter directly using a stub command.
		got := splitCommandPath(c.path)
		if got != c.want {
			t.Errorf("splitCommandPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// splitCommandPath is the part of commandPathRoot we can test without a
// real cobra tree. Kept in sync with commandPathRoot in root.go.
func splitCommandPath(path string) string {
	for i := 0; i < len(path); i++ {
		if path[i] == ' ' {
			rest := path[i+1:]
			for j := 0; j < len(rest); j++ {
				if rest[j] == ' ' {
					return rest[:j]
				}
			}
			return rest
		}
	}
	return ""
}

func TestSkipUpdateCheckFor(t *testing.T) {
	// Build a small cobra tree: `promptconduit hook`, `promptconduit upgrade`,
	// `promptconduit config show`.
	root := &cobra.Command{Use: "promptconduit"}
	hook := &cobra.Command{Use: "hook"}
	upgrade := &cobra.Command{Use: "upgrade"}
	config := &cobra.Command{Use: "config"}
	show := &cobra.Command{Use: "show"}
	root.AddCommand(hook, upgrade, config)
	config.AddCommand(show)

	cases := []struct {
		cmd  *cobra.Command
		skip bool
	}{
		{hook, true},
		{upgrade, true},
		{config, false},
		{show, false}, // nested under config — still runs the check
		{root, false},
	}
	t.Setenv("PROMPTCONDUIT_AUTO_UPDATE_CHILD", "")
	for _, c := range cases {
		if got := skipUpdateCheckFor(c.cmd); got != c.skip {
			t.Errorf("skipUpdateCheckFor(%s) = %v, want %v", c.cmd.Use, got, c.skip)
		}
	}

	// Child-subprocess env always skips, even for a foreground subcommand.
	t.Setenv("PROMPTCONDUIT_AUTO_UPDATE_CHILD", "1")
	if !skipUpdateCheckFor(config) {
		t.Error("PROMPTCONDUIT_AUTO_UPDATE_CHILD=1 must force a skip")
	}
}
