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

// brewLayout builds a throwaway Homebrew-style directory tree and returns the
// real Cellar binary path plus the opt/bin symlink paths. makeOpt/makeBin
// control which version-independent symlinks exist.
func brewLayout(t *testing.T, makeOpt, makeBin bool) (realBin, optLink, binLink string) {
	t.Helper()
	// macOS temp dirs live under /var -> /private/var; canonicalize the base so
	// EvalSymlinks results compare equal to the paths we construct.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(base, "Cellar", "promptconduit", "0.4.0")
	cellarBin := filepath.Join(versionDir, "bin")
	if err := os.MkdirAll(cellarBin, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin = filepath.Join(cellarBin, "promptconduit")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	optLink = filepath.Join(base, "opt", "promptconduit", "bin", "promptconduit")
	binLink = filepath.Join(base, "bin", "promptconduit")
	if makeOpt {
		// Mirror real Homebrew: opt/<formula> is a directory symlink to the keg,
		// so the binary is reached at opt/<formula>/bin/<binary> through it.
		optDir := filepath.Join(base, "opt")
		if err := os.MkdirAll(optDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(versionDir, filepath.Join(optDir, "promptconduit")); err != nil {
			t.Fatal(err)
		}
	}
	if makeBin {
		if err := os.MkdirAll(filepath.Dir(binLink), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realBin, binLink); err != nil {
			t.Fatal(err)
		}
	}
	return realBin, optLink, binLink
}

func TestHomebrewStablePath(t *testing.T) {
	t.Run("prefers opt prefix over bin", func(t *testing.T) {
		real, optLink, _ := brewLayout(t, true, true)
		got, ok := homebrewStablePath(real)
		if !ok || got != optLink {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, optLink)
		}
	})

	t.Run("falls back to linked bin symlink", func(t *testing.T) {
		real, _, binLink := brewLayout(t, false, true)
		got, ok := homebrewStablePath(real)
		if !ok || got != binLink {
			t.Fatalf("got (%q, %v), want (%q, true)", got, ok, binLink)
		}
	})

	t.Run("no stable symlink present keeps resolved path", func(t *testing.T) {
		real, _, _ := brewLayout(t, false, false)
		if got, ok := homebrewStablePath(real); ok {
			t.Fatalf("expected ok=false, got %q", got)
		}
	})

	t.Run("non-homebrew path untouched", func(t *testing.T) {
		if got, ok := homebrewStablePath("/usr/local/bin/promptconduit"); ok {
			t.Fatalf("expected ok=false, got %q", got)
		}
	})

	t.Run("rejects symlink resolving to a different binary", func(t *testing.T) {
		real, optLink, _ := brewLayout(t, false, false)
		// An opt symlink that points at some other binary must not be trusted.
		other := filepath.Join(filepath.Dir(real), "other")
		if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(optLink), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, optLink); err != nil {
			t.Fatal(err)
		}
		if got, ok := homebrewStablePath(real); ok {
			t.Fatalf("expected ok=false for stale symlink, got %q", got)
		}
	})
}

func TestStableExecutablePath(t *testing.T) {
	t.Run("homebrew binary maps to stable symlink", func(t *testing.T) {
		real, optLink, _ := brewLayout(t, true, true)
		got, err := stableExecutablePath(real)
		if err != nil {
			t.Fatal(err)
		}
		if got != optLink {
			t.Fatalf("got %q, want %q", got, optLink)
		}
	})

	t.Run("non-homebrew binary returns the resolved path", func(t *testing.T) {
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
	})
}
