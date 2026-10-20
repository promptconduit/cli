package cmd

import (
	"io"
	"testing"

	"github.com/promptconduit/cli/internal/client"
	"github.com/spf13/pflag"
)

// runConfigSet runs `config set` with the given flags against an isolated
// config dir, resetting the package-level flag state before and after.
func runConfigSet(t *testing.T, flags map[string]string) {
	t.Helper()
	reset := func() {
		configSetCmd.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
	reset()
	t.Cleanup(reset)
	for k, v := range flags {
		if err := configSetCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set --%s=%s: %v", k, v, err)
		}
	}
	configSetCmd.SetOut(io.Discard)
	if err := configSetCmd.RunE(configSetCmd, nil); err != nil {
		t.Fatalf("config set: %v", err)
	}
}

func isolateConfigSet(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(client.EnvAPIKey, "")
	t.Setenv(client.EnvAPIURL, "")
	t.Setenv(client.EnvDebug, "")
}

func saveFileConfig(t *testing.T, fc *client.FileConfig) {
	t.Helper()
	if err := client.SaveFileConfig(fc); err != nil {
		t.Fatalf("save config: %v", err)
	}
}

func TestConfigSet_ConnectionSettingsTargetActiveEnv(t *testing.T) {
	isolateConfigSet(t)
	saveFileConfig(t, &client.FileConfig{
		CurrentEnv: "prod",
		Environments: map[string]*client.Config{
			"prod": {APIKey: "sk_prod", APIURL: "https://api.example.com", Debug: true},
			"dev":  {APIKey: "sk_dev", Debug: true},
		},
	})

	runConfigSet(t, map[string]string{"debug": "false"})
	if client.LoadConfig().Debug {
		t.Fatal("debug still on for the active env after --debug=false")
	}

	runConfigSet(t, map[string]string{"api-url": "https://api2.example.com", "api-key": "sk_new"})
	cfg := client.LoadConfig()
	if cfg.APIURL != "https://api2.example.com" || cfg.APIKey != "sk_new" {
		t.Fatalf("active env not updated: url=%q key=%q", cfg.APIURL, cfg.APIKey)
	}

	runConfigSet(t, map[string]string{"debug": "true"})
	if !client.LoadConfig().Debug {
		t.Fatal("debug not on for the active env after --debug")
	}

	fc, err := client.LoadFileConfig()
	if err != nil {
		t.Fatal(err)
	}
	if fc.APIKey != "" || fc.APIURL != "" || fc.Debug {
		t.Errorf("top-level fields written: key=%q url=%q debug=%v", fc.APIKey, fc.APIURL, fc.Debug)
	}
	if dev := fc.Environments["dev"]; dev.APIKey != "sk_dev" || !dev.Debug {
		t.Errorf("inactive env changed: %+v", dev)
	}
}

func TestConfigSet_LegacyFlatConfig(t *testing.T) {
	isolateConfigSet(t)
	saveFileConfig(t, &client.FileConfig{APIKey: "sk_flat", Debug: true})

	runConfigSet(t, map[string]string{"debug": "false"})
	cfg := client.LoadConfig()
	if cfg.Debug {
		t.Fatal("debug still on after --debug=false with flat config")
	}
	if cfg.APIKey != "sk_flat" {
		t.Errorf("api key lost: %q", cfg.APIKey)
	}

	runConfigSet(t, map[string]string{"api-key": "sk_flat2"})
	fc, err := client.LoadFileConfig()
	if err != nil {
		t.Fatal(err)
	}
	if fc.APIKey != "sk_flat2" || fc.Environments != nil {
		t.Errorf("flat config not updated in place: %+v", fc)
	}
}
