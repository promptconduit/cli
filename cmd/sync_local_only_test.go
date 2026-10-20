package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptconduit/cli/internal/client"
)

func TestCheckSyncAllowed(t *testing.T) {
	tests := []struct {
		name    string
		cfg     client.Config
		wantErr string
	}{
		{"local-only with key refuses", client.Config{APIKey: "sk_test", LocalOnly: true}, "local-only mode"},
		{"local-only without key refuses as local-only", client.Config{LocalOnly: true}, "--local-only=false"},
		{"no key refuses", client.Config{}, "API key not configured"},
		{"key and cloud mode allowed", client.Config{APIKey: "sk_test"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSyncAllowed(&tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// runSync must refuse before touching any transcript when local-only comes
// from the environment, even though an API key is configured.
func TestRunSyncRefusesInLocalOnlyMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(client.EnvAPIKey, "")
	t.Setenv(client.EnvLocalOnly, "")

	cfgDir := filepath.Join(home, client.ConfigDirName)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, client.ConfigFileName), []byte(`{"api_key":"sk_test"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("env", func(t *testing.T) {
		t.Setenv(client.EnvLocalOnly, "1")
		err := runSync(syncCmd, nil)
		if err == nil || !strings.Contains(err.Error(), "local-only mode") {
			t.Fatalf("err = %v, want local-only refusal", err)
		}
	})

	t.Run("config", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(cfgDir, client.ConfigFileName), []byte(`{"api_key":"sk_test","local_only":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runSync(syncCmd, nil)
		if err == nil || !strings.Contains(err.Error(), "local-only mode") {
			t.Fatalf("err = %v, want local-only refusal", err)
		}
	})
}

func TestLocalSkillDir(t *testing.T) {
	home := "/home/me"
	root := "/src/proj"
	if got, want := localSkillDir(home, root, "proj", "ci"), filepath.Join(root, ".claude", "skills", "ci"); got != want {
		t.Errorf("repo-scoped in git repo = %q, want %q", got, want)
	}
	if got, want := localSkillDir(home, root, "", "ci"), filepath.Join(home, ".claude", "skills", "ci"); got != want {
		t.Errorf("global = %q, want %q", got, want)
	}
	if got, want := localSkillDir(home, "", "dirname", "ci"), filepath.Join(home, ".claude", "skills", "ci"); got != want {
		t.Errorf("no git root = %q, want %q", got, want)
	}
}
