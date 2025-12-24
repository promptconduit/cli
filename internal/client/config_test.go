package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPath(t *testing.T) {
	// Save original env vars
	origXDG := os.Getenv(EnvXDGConfigHome)
	defer os.Setenv(EnvXDGConfigHome, origXDG)

	// Test 1: No XDG_CONFIG_HOME set - should return ~/.config/promptconduit/config.json
	os.Unsetenv(EnvXDGConfigHome)
	path := ConfigPath()
	home, _ := os.UserHomeDir()
	expectedDefault := filepath.Join(home, ".config", ConfigDirName, ConfigFileName)
	if path != expectedDefault {
		t.Errorf("Expected default %s, got %s", expectedDefault, path)
	}

	// Test 2: XDG_CONFIG_HOME set - should use custom location
	tmpDir := t.TempDir()
	xdgDir := filepath.Join(tmpDir, "xdg-config")
	os.Setenv(EnvXDGConfigHome, xdgDir)

	path = ConfigPath()
	expectedCustom := filepath.Join(xdgDir, ConfigDirName, ConfigFileName)
	if path != expectedCustom {
		t.Errorf("Expected custom %s, got %s", expectedCustom, path)
	}
}

func TestAllConfigPaths(t *testing.T) {
	// Save original env vars
	origXDG := os.Getenv(EnvXDGConfigHome)
	defer os.Setenv(EnvXDGConfigHome, origXDG)

	os.Unsetenv(EnvXDGConfigHome)

	paths := AllConfigPaths()
	if len(paths) != 1 {
		t.Errorf("Expected exactly 1 path, got %d", len(paths))
	}

	// Should contain XDG default
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".config", ConfigDirName, ConfigFileName)
	if paths[0] != expected {
		t.Errorf("Expected %s, got %s", expected, paths[0])
	}
}
