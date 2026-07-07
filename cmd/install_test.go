package cmd

import (
	"os"
	"path/filepath"
	"reflect"
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
		{"all", []string{"claude-code", "cursor", "gemini-cli", "codex", "copilot"}, false},
		{"*", []string{"claude-code", "cursor", "gemini-cli", "codex", "copilot"}, false},
		{"1,2", []string{"claude-code", "cursor"}, false},
		{"cursor, gemini", []string{"cursor", "gemini-cli"}, false}, // alias normalized
		{"1,cursor,1", []string{"claude-code", "cursor"}, false},    // dedupe
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

func TestResolveInstallTools_Args(t *testing.T) {
	cmd := &cobra.Command{}
	// Multiple explicit tools, with an alias and a duplicate.
	got, err := resolveInstallTools(cmd, []string{"cursor", "gemini", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"cursor", "gemini-cli"}) {
		t.Fatalf("got %v", got)
	}
	// "all" expands.
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
