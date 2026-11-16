package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestParseToolSelection(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  bool
	}{
		{"", nil, false},
		{"all", []string{"claude-code", "cursor"}, false},
		{"*", []string{"claude-code", "cursor"}, false},
		{"1,2", []string{"claude-code", "cursor"}, false},
		{"1,cursor,1", []string{"claude-code", "cursor"}, false}, // dedupe
		{"3", nil, true},              // experimental tools aren't numbered without the flag
		{"cursor, gemini", nil, true}, // experimental by name needs the flag
		{"9", nil, true},
		{"bogus", nil, true},
	}
	for _, c := range cases {
		got, err := parseToolSelection(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseToolSelection(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseToolSelection(%q): unexpected error %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseToolSelection(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func withExperimental(t *testing.T, v bool) {
	t.Helper()
	prev := allowExperimental
	allowExperimental = v
	t.Cleanup(func() { allowExperimental = prev })
}

func TestParseToolSelection_Experimental(t *testing.T) {
	withExperimental(t, true)
	cases := []struct {
		in   string
		want []string
	}{
		{"all", []string{"claude-code", "cursor", "gemini-cli", "codex", "copilot", "grok"}},
		{"cursor, gemini", []string{"cursor", "gemini-cli"}}, // alias normalized
		{"grok-build", []string{"grok"}},
		{"3,4", []string{"gemini-cli", "codex"}},
	}
	for _, c := range cases {
		got, err := parseToolSelection(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseToolSelection(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

func TestSplitExperimental(t *testing.T) {
	withExperimental(t, false)
	offered, skipped := splitExperimental([]string{"cursor", "gemini-cli", "claude-code", "codex"})
	if !reflect.DeepEqual(offered, []string{"cursor", "claude-code"}) || !reflect.DeepEqual(skipped, []string{"gemini-cli", "codex"}) {
		t.Fatalf("offered %v skipped %v", offered, skipped)
	}
	withExperimental(t, true)
	offered, skipped = splitExperimental([]string{"cursor", "gemini-cli"})
	if len(offered) != 2 || len(skipped) != 0 {
		t.Fatalf("with --experimental: offered %v skipped %v", offered, skipped)
	}
}

func TestResolveInstallTools_Args(t *testing.T) {
	cmd := &cobra.Command{}
	withExperimental(t, false)
	// An experimental tool without the flag errors and says how to opt in.
	if _, err := resolveInstallTools(cmd, []string{"cursor", "codex"}); err == nil || !strings.Contains(err.Error(), "--experimental") {
		t.Fatalf("codex without --experimental: err %v", err)
	}
	// "all" expands to the supported tools only.
	got, err := resolveInstallTools(cmd, []string{"all"})
	if err != nil || !reflect.DeepEqual(got, []string{"claude-code", "cursor"}) {
		t.Fatalf("all: got %v err %v", got, err)
	}

	withExperimental(t, true)
	// Multiple explicit tools, with an alias and a duplicate.
	got, err = resolveInstallTools(cmd, []string{"cursor", "gemini", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"cursor", "gemini-cli"}) {
		t.Fatalf("got %v", got)
	}
	// "all --experimental" expands to every tool.
	got, err = resolveInstallTools(cmd, []string{"all"})
	if err != nil || len(got) != len(installableTools) {
		t.Fatalf("all: got %v err %v", got, err)
	}
	// Unknown tool errors.
	if _, err := resolveInstallTools(cmd, []string{"emacs"}); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestStableExecutablePath(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "promptconduit")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := stableExecutablePath(bin)
	if err != nil {
		t.Fatal(err)
	}
	if got != bin {
		t.Fatalf("got %q, want %q", got, bin)
	}
}
