package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPath(t *testing.T) {
	// Save original env vars
	origXDG := os.Getenv(EnvXDGConfigHome)
	defer func() { _ = os.Setenv(EnvXDGConfigHome, origXDG) }()

	// Test 1: No XDG_CONFIG_HOME set - should return ~/.config/promptconduit/config.json
	_ = os.Unsetenv(EnvXDGConfigHome)
	path := ConfigPath()
	home, _ := os.UserHomeDir()
	expectedDefault := filepath.Join(home, ".config", ConfigDirName, ConfigFileName)
	if path != expectedDefault {
		t.Errorf("Expected default %s, got %s", expectedDefault, path)
	}

	// Test 2: XDG_CONFIG_HOME set - should use custom location
	tmpDir := t.TempDir()
	xdgDir := filepath.Join(tmpDir, "xdg-config")
	_ = os.Setenv(EnvXDGConfigHome, xdgDir)

	path = ConfigPath()
	expectedCustom := filepath.Join(xdgDir, ConfigDirName, ConfigFileName)
	if path != expectedCustom {
		t.Errorf("Expected custom %s, got %s", expectedCustom, path)
	}
}

func TestShouldSend(t *testing.T) {
	cases := []struct {
		name      string
		apiKey    string
		localOnly bool
		want      bool
	}{
		{"key set, cloud mode", "sk_x", false, true},
		{"key set, local-only", "sk_x", true, false},
		{"no key (implicit local-only)", "", false, false},
		{"no key, local-only", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{APIKey: tc.apiKey, LocalOnly: tc.localOnly}
			if got := c.ShouldSend(); got != tc.want {
				t.Errorf("ShouldSend() = %v, want %v", got, tc.want)
			}
		})
	}
}

// withIsolatedConfig points config resolution at a throwaway XDG dir and clears
// any PROMPTCONDUIT_* env vars that would otherwise leak from the host, so
// LoadConfig is deterministic. Restores prior env on cleanup.
func withIsolatedConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	keys := []string{EnvXDGConfigHome, EnvAPIKey, EnvAPIURL, EnvLocalOnly, EnvEventLog, EnvAutoUpdate, EnvDebug, EnvTimeout}
	saved := make(map[string]string, len(keys))
	for _, k := range keys {
		saved[k] = os.Getenv(k)
		_ = os.Unsetenv(k)
	}
	_ = os.Setenv(EnvXDGConfigHome, dir)
	t.Cleanup(func() {
		for k, v := range saved {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	})
	return dir
}

func TestLoadConfigLocalOnlyFromEnv(t *testing.T) {
	withIsolatedConfig(t)
	_ = os.Setenv(EnvLocalOnly, "1")

	if cfg := LoadConfig(); !cfg.LocalOnly {
		t.Error("LocalOnly should be true when PROMPTCONDUIT_LOCAL_ONLY=1")
	}
}

func TestLoadConfigLocalOnlyFromFile(t *testing.T) {
	withIsolatedConfig(t)

	if err := SaveFileConfig(&FileConfig{LocalOnly: true}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if cfg := LoadConfig(); !cfg.LocalOnly {
		t.Error("LocalOnly should be true when local_only:true is set in the config file")
	}
}

func TestLoadConfigDefaultsToCloudMode(t *testing.T) {
	withIsolatedConfig(t)

	cfg := LoadConfig()
	if cfg.LocalOnly {
		t.Error("LocalOnly should default to false with no env/file override")
	}
}

func TestAllConfigPaths(t *testing.T) {
	// Save original env vars
	origXDG := os.Getenv(EnvXDGConfigHome)
	defer func() { _ = os.Setenv(EnvXDGConfigHome, origXDG) }()

	_ = os.Unsetenv(EnvXDGConfigHome)

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

func TestLoadConfig_AutoUpdateOptOut(t *testing.T) {
	// Isolate the config file to a temp dir via XDG_CONFIG_HOME and clear the
	// env opt-out so each subtest starts from a known state.
	cases := []struct {
		name        string
		fileDisable bool   // disable_auto_update in the config file
		env         string // PROMPTCONDUIT_AUTO_UPDATE value ("" = unset)
		setEnv      bool
		want        bool // expected cfg.DisableAutoUpdate
	}{
		{name: "default enabled", want: false},
		{name: "file config disables", fileDisable: true, want: true},
		{name: "env 0 disables", env: "0", setEnv: true, want: true},
		{name: "env false disables", env: "false", setEnv: true, want: true},
		{name: "env no disables", env: "no", setEnv: true, want: true},
		{name: "env 1 leaves enabled", env: "1", setEnv: true, want: false},
		// Env=0 overrides a file config that left it enabled.
		{name: "env 0 overrides file-enabled", env: "0", setEnv: true, want: true},
		// File-disabled stays disabled when env is some other value.
		{name: "file disables, env unrelated", fileDisable: true, env: "1", setEnv: true, want: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			xdg := t.TempDir()
			t.Setenv(EnvXDGConfigHome, xdg)
			// Clear any stray env opt-out from the outer environment so each
			// subtest starts from a known state ("" is treated as unset).
			t.Setenv(EnvAutoUpdate, "")
			if c.setEnv {
				t.Setenv(EnvAutoUpdate, c.env)
			}

			fc := &FileConfig{
				APIKey:            "test-key", // ensure GetCurrentConfig returns a config
				DisableAutoUpdate: c.fileDisable,
			}
			if err := SaveFileConfig(fc); err != nil {
				t.Fatalf("SaveFileConfig: %v", err)
			}

			cfg := LoadConfig()
			if cfg.DisableAutoUpdate != c.want {
				t.Errorf("DisableAutoUpdate = %v, want %v", cfg.DisableAutoUpdate, c.want)
			}
		})
	}
}

// Regression: in a multi-environment config, top-level opt-out flags (where
// `config set --disable-auto-update/--disable-event-log/--local-only` writes
// them) were ignored because GetCurrentConfig returned the active env's config
// without merging the globals — so the toggle was a silent no-op.
func TestGetCurrentConfig_RootOptOutsApplyToActiveEnv(t *testing.T) {
	mk := func() *FileConfig {
		return &FileConfig{
			CurrentEnv: "prod",
			Environments: map[string]*Config{
				"prod": {APIKey: "sk_prod", APIURL: "https://api.example.com"},
				"dev":  {APIKey: "sk_dev"},
			},
		}
	}

	t.Run("root globals apply to the active env", func(t *testing.T) {
		fc := mk()
		fc.DisableAutoUpdate = true
		fc.DisableEventLog = true
		fc.LocalOnly = true
		got := fc.GetCurrentConfig()
		if got == nil {
			t.Fatal("GetCurrentConfig returned nil")
		}
		if !got.DisableAutoUpdate || !got.DisableEventLog || !got.LocalOnly {
			t.Errorf("root globals not applied: auto=%v eventlog=%v local=%v",
				got.DisableAutoUpdate, got.DisableEventLog, got.LocalOnly)
		}
		if got.APIKey != "sk_prod" {
			t.Errorf("active env not preserved: APIKey = %q, want sk_prod", got.APIKey)
		}
	})

	t.Run("per-env true wins even when root is false", func(t *testing.T) {
		fc := mk()
		fc.Environments["prod"].DisableAutoUpdate = true
		if got := fc.GetCurrentConfig(); !got.DisableAutoUpdate {
			t.Error("per-env DisableAutoUpdate should be honored")
		}
	})

	t.Run("all false when neither root nor env set them", func(t *testing.T) {
		got := mk().GetCurrentConfig()
		if got.DisableAutoUpdate || got.DisableEventLog || got.LocalOnly {
			t.Error("expected all opt-outs false")
		}
	})

	t.Run("does not mutate the stored env config", func(t *testing.T) {
		fc := mk()
		fc.DisableAutoUpdate = true
		_ = fc.GetCurrentConfig()
		if fc.Environments["prod"].DisableAutoUpdate {
			t.Error("root global leaked into the stored env Config (should return a copy)")
		}
	})
}
