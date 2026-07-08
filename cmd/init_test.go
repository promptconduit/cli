package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectInstalledTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create cursor marker
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create claude marker
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := detectInstalledTools()
	if len(got) != 2 {
		t.Fatalf("expected 2 detected tools, got %v", got)
	}

	seen := map[string]bool{}
	for _, tool := range got {
		seen[tool] = true
	}
	if !seen["cursor"] || !seen["claude-code"] {
		t.Fatalf("expected cursor and claude-code, got %v", got)
	}
}

func TestDetectInstalledTools_Empty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := detectInstalledTools()
	if len(got) != 0 {
		t.Fatalf("expected no tools, got %v", got)
	}
}
