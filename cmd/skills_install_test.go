package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptconduit/cli/internal/client"
	skillspkg "github.com/promptconduit/cli/internal/skills"
)

// stubSkill is the minimum shape installOne needs from the platform.
type stubSkill struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RepoName string `json:"repo_name,omitempty"`
}

// newTestServer returns an httptest.Server that responds to the two
// endpoints install/uninstall hit: GET /v1/skills and GET /v1/skills/:id/command.
// `command` is the SKILL.md body returned for the latter.
func newTestServer(t *testing.T, skills []stubSkill, command string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/skills" && r.Method == http.MethodGet:
			out := map[string]interface{}{
				"skills": toAnySlice(skills),
				"total":  float64(len(skills)),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/v1/skills/") && strings.HasSuffix(r.URL.Path, "/command"):
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte(command))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func toAnySlice(skills []stubSkill) []interface{} {
	out := make([]interface{}, 0, len(skills))
	for _, s := range skills {
		// Marshal/unmarshal so the test server returns the same map shape
		// production code expects (with is_approved missing → "new").
		buf, _ := json.Marshal(map[string]interface{}{
			"id":          s.ID,
			"name":        s.Name,
			"repo_name":   s.RepoName,
			"is_approved": true, // tests assume approved set; install --all relies on it
		})
		var m map[string]interface{}
		_ = json.Unmarshal(buf, &m)
		out = append(out, m)
	}
	return out
}

// setupSkillsEnv redirects config, HOME, and the API URL into temp dirs
// for the duration of the test. It also resets all package-level command
// flag variables to their defaults so tests can be run in any order.
func setupSkillsEnv(t *testing.T, apiURL string) (configDir, homeDir string) {
	t.Helper()
	configDir = t.TempDir()
	homeDir = t.TempDir()

	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", homeDir)
	t.Setenv(client.EnvAPIKey, "test-key")
	t.Setenv(client.EnvAPIURL, apiURL)
	t.Setenv(client.EnvAutoUpdate, "0")

	// Reset flags so previous t.Run cases don't bleed state.
	skillsInstallAll = false
	skillsInstallScope = "global" // default to global in tests; project requires a git repo
	skillsInstallForce = false
	skillsUninstallAll = false
	skillsUninstallForce = false

	// The promptconduit config dir is derived from XDG_CONFIG_HOME/promptconduit.
	// Confirm with the real helper so the test assertions match production.
	if got := client.ConfigDir(); !strings.HasPrefix(got, configDir) {
		t.Fatalf("ConfigDir = %q, expected to live under %q", got, configDir)
	}
	return configDir, homeDir
}

func TestInstall_WritesFile_AndRecordsManifest(t *testing.T) {
	body := "# SKILL.md\nfoo\n"
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "shipping-features"}}, body)
	_, homeDir := setupSkillsEnv(t, srv.URL)

	if err := runSkillsInstall(skillsInstallCmd, []string{"shipping-features"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	target := filepath.Join(homeDir, ".claude", "skills", "shipping-features", "SKILL.md")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if string(got) != body {
		t.Errorf("file contents = %q, want %q", string(got), body)
	}

	manifest, err := skillspkg.Load(client.ConfigDir())
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	entry := manifest.Find("shipping-features")
	if entry == nil {
		t.Fatalf("manifest missing entry for shipping-features: %+v", manifest)
	}
	if entry.Scope != skillspkg.ScopeGlobal {
		t.Errorf("entry.Scope = %q, want global", entry.Scope)
	}
	if entry.PlatformSHA256 != skillspkg.HashContent([]byte(body)) {
		t.Errorf("entry.PlatformSHA256 mismatch")
	}
	if len(entry.Files) != 1 || entry.Files[0].Path != target {
		t.Errorf("entry.Files = %+v", entry.Files)
	}
}

func TestInstall_NoOp_WhenAlreadyCurrent(t *testing.T) {
	body := "# SKILL.md\nv1\n"
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body)
	setupSkillsEnv(t, srv.URL)

	if err := runSkillsInstall(skillsInstallCmd, []string{"alpha"}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := runSkillsInstall(skillsInstallCmd, []string{"alpha"}); err != nil {
		t.Fatalf("second install (should be no-op): %v", err)
	}
}

func TestInstall_AllInstallsEveryApprovedSkill(t *testing.T) {
	body := "# SKILL.md\nbody\n"
	srv := newTestServer(t, []stubSkill{
		{ID: "id-1", Name: "alpha"},
		{ID: "id-2", Name: "beta"},
	}, body)
	_, homeDir := setupSkillsEnv(t, srv.URL)
	skillsInstallAll = true

	if err := runSkillsInstall(skillsInstallCmd, nil); err != nil {
		t.Fatalf("install --all: %v", err)
	}
	for _, name := range []string{"alpha", "beta"} {
		p := filepath.Join(homeDir, ".claude", "skills", name, "SKILL.md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
}

func TestUninstall_RemovesFileAndManifestEntry(t *testing.T) {
	body := "# SKILL.md\nbody\n"
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body)
	_, homeDir := setupSkillsEnv(t, srv.URL)

	if err := runSkillsInstall(skillsInstallCmd, []string{"alpha"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := runSkillsUninstall(skillsUninstallCmd, []string{"alpha"}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".claude", "skills", "alpha", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("file still present, err=%v", err)
	}

	m, _ := skillspkg.Load(client.ConfigDir())
	if m.Find("alpha") != nil {
		t.Errorf("manifest still has entry for alpha")
	}
}

func TestUninstall_RefusesOnLocalEdits(t *testing.T) {
	body := "# SKILL.md\noriginal\n"
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body)
	_, homeDir := setupSkillsEnv(t, srv.URL)

	if err := runSkillsInstall(skillsInstallCmd, []string{"alpha"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Hand-edit the file
	target := filepath.Join(homeDir, ".claude", "skills", "alpha", "SKILL.md")
	if err := os.WriteFile(target, []byte("# I edited this\n"), 0o644); err != nil {
		t.Fatalf("hand-edit: %v", err)
	}

	if err := runSkillsUninstall(skillsUninstallCmd, []string{"alpha"}); err != nil {
		// runSkillsUninstall returns nil unless 1+ failed; refusal is a "skipped" path.
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("file should still exist after refused uninstall, stat err=%v", err)
	}

	m, _ := skillspkg.Load(client.ConfigDir())
	if m.Find("alpha") == nil {
		t.Errorf("manifest should still have entry after refused uninstall")
	}
}

func TestUninstall_ForceRemovesLocalEdits(t *testing.T) {
	body := "# SKILL.md\noriginal\n"
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body)
	_, homeDir := setupSkillsEnv(t, srv.URL)

	if err := runSkillsInstall(skillsInstallCmd, []string{"alpha"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	target := filepath.Join(homeDir, ".claude", "skills", "alpha", "SKILL.md")
	_ = os.WriteFile(target, []byte("# edited\n"), 0o644)

	skillsUninstallForce = true
	if err := runSkillsUninstall(skillsUninstallCmd, []string{"alpha"}); err != nil {
		t.Fatalf("uninstall --force: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("file should be gone, err=%v", err)
	}
}

func TestUninstall_UntrackedSkill_DoesNothing(t *testing.T) {
	srv := newTestServer(t, nil, "")
	setupSkillsEnv(t, srv.URL)
	// No install happened; manifest is empty.
	if err := runSkillsUninstall(skillsUninstallCmd, []string{"nope"}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
}

func TestInstall_MutexFlags(t *testing.T) {
	srv := newTestServer(t, nil, "")
	setupSkillsEnv(t, srv.URL)
	skillsInstallAll = true
	if err := runSkillsInstall(skillsInstallCmd, []string{"name"}); err == nil {
		t.Error("expected mutex error when --all + positional arg, got nil")
	}
}

func TestInstall_InvalidName(t *testing.T) {
	srv := newTestServer(t, nil, "")
	setupSkillsEnv(t, srv.URL)
	if err := runSkillsInstall(skillsInstallCmd, []string{"BAD NAME"}); err == nil {
		t.Error("expected validation error, got nil")
	}
}

func TestInstall_SkillNotOnServer(t *testing.T) {
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, "")
	setupSkillsEnv(t, srv.URL)
	err := runSkillsInstall(skillsInstallCmd, []string{"missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got %v", err)
	}
}

// Sanity: ensure the test server route patterns match what the client hits.
// If this breaks, the platform shape changed and the test stubs need updates.
func TestServerRoutes_AreReachable(t *testing.T) {
	body := "body"
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/skills/id-1/command")
	if err != nil {
		t.Fatalf("GET /command: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// Helpful failure message if ConfigDir somehow can't resolve.
var _ = fmt.Sprintf
