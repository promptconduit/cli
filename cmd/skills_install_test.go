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

// newTestServer returns an httptest.Server that responds to the endpoints
// install/uninstall/approve/reject/delete hit. `command` is the SKILL.md
// body returned for GET /v1/skills/:id/command. `deleted` is mutated by
// DELETE handlers so tests can assert the call was made.
func newTestServer(t *testing.T, skills []stubSkill, command string, deleted *map[string]bool) *httptest.Server {
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
		case strings.HasPrefix(r.URL.Path, "/v1/skills/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/v1/skills/")
			for _, s := range skills {
				if s.ID == id {
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"id":          s.ID,
						"name":        s.Name,
						"repo_name":   s.RepoName,
						"is_approved": true,
					})
					return
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/skills/") && r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/v1/skills/")
			if deleted != nil {
				(*deleted)[id] = true
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/v1/skills/") && r.Method == http.MethodPatch:
			// approve/reject — accept and echo back is_approved
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
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
	skillsDeleteYes = false

	// The promptconduit config dir is derived from XDG_CONFIG_HOME/promptconduit.
	// Confirm with the real helper so the test assertions match production.
	if got := client.ConfigDir(); !strings.HasPrefix(got, configDir) {
		t.Fatalf("ConfigDir = %q, expected to live under %q", got, configDir)
	}
	return configDir, homeDir
}

func TestInstall_WritesFile_AndRecordsManifest(t *testing.T) {
	body := "# SKILL.md\nfoo\n"
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "shipping-features"}}, body, nil)
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
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body, nil)
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
	}, body, nil)
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
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body, nil)
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
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body, nil)
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
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body, nil)
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
	srv := newTestServer(t, nil, "", nil)
	setupSkillsEnv(t, srv.URL)
	// No install happened; manifest is empty.
	if err := runSkillsUninstall(skillsUninstallCmd, []string{"nope"}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
}

func TestInstall_MutexFlags(t *testing.T) {
	srv := newTestServer(t, nil, "", nil)
	setupSkillsEnv(t, srv.URL)
	skillsInstallAll = true
	if err := runSkillsInstall(skillsInstallCmd, []string{"name"}); err == nil {
		t.Error("expected mutex error when --all + positional arg, got nil")
	}
}

func TestInstall_InvalidName(t *testing.T) {
	srv := newTestServer(t, nil, "", nil)
	setupSkillsEnv(t, srv.URL)
	if err := runSkillsInstall(skillsInstallCmd, []string{"BAD NAME"}); err == nil {
		t.Error("expected validation error, got nil")
	}
}

func TestInstall_SkillNotOnServer(t *testing.T) {
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, "", nil)
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
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, body, nil)
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

// ============================================================================
// resolveSkill — UUID vs name dispatch + ambiguity handling
// ============================================================================

const validUUID = "cc9f1eff-487c-4ff6-b542-5814ba815d45"

func TestResolveSkill_ByName_SingleMatch(t *testing.T) {
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, "body", nil)
	setupSkillsEnv(t, srv.URL)
	cli := client.NewClient(client.LoadConfig(), Version)
	got, err := resolveSkill(cli, "alpha")
	if err != nil {
		t.Fatalf("resolveSkill(alpha): %v", err)
	}
	if id, _ := got["id"].(string); id != "id-1" {
		t.Errorf("resolved id = %q, want id-1", id)
	}
}

func TestResolveSkill_ByName_AmbiguousErrorsWithIDs(t *testing.T) {
	srv := newTestServer(t, []stubSkill{
		{ID: "abc1", Name: "alpha"},
		{ID: "def2", Name: "alpha"},
	}, "body", nil)
	setupSkillsEnv(t, srv.URL)
	cli := client.NewClient(client.LoadConfig(), Version)
	_, err := resolveSkill(cli, "alpha")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"abc1", "def2", "alpha"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}
}

func TestResolveSkill_ByUUID_Hits(t *testing.T) {
	srv := newTestServer(t, []stubSkill{{ID: validUUID, Name: "alpha"}}, "body", nil)
	setupSkillsEnv(t, srv.URL)
	cli := client.NewClient(client.LoadConfig(), Version)
	got, err := resolveSkill(cli, validUUID)
	if err != nil {
		t.Fatalf("resolveSkill(uuid): %v", err)
	}
	if id, _ := got["id"].(string); id != validUUID {
		t.Errorf("resolved id = %q, want %q", id, validUUID)
	}
}

func TestResolveSkill_ByUUID_NotFound(t *testing.T) {
	srv := newTestServer(t, nil, "", nil)
	setupSkillsEnv(t, srv.URL)
	cli := client.NewClient(client.LoadConfig(), Version)
	_, err := resolveSkill(cli, validUUID)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found', got %v", err)
	}
}

func TestResolveSkill_EmptyString(t *testing.T) {
	srv := newTestServer(t, nil, "", nil)
	setupSkillsEnv(t, srv.URL)
	cli := client.NewClient(client.LoadConfig(), Version)
	if _, err := resolveSkill(cli, ""); err == nil {
		t.Error("expected error on empty identifier")
	}
}

func TestInstall_DisambiguatesByUUID(t *testing.T) {
	// Two skills share a name; install with the UUID picks the right one
	// and writes its content.
	other := "11111111-2222-3333-4444-555555555555"
	srv := newTestServer(t, []stubSkill{
		{ID: validUUID, Name: "alpha"},
		{ID: other, Name: "alpha"},
	}, "# from validUUID\n", nil)
	_, homeDir := setupSkillsEnv(t, srv.URL)

	if err := runSkillsInstall(skillsInstallCmd, []string{validUUID}); err != nil {
		t.Fatalf("install by uuid: %v", err)
	}
	target := filepath.Join(homeDir, ".claude", "skills", "alpha", "SKILL.md")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if !strings.Contains(string(got), "from validUUID") {
		t.Errorf("wrong content installed: %q", string(got))
	}
}

func TestInstall_AmbiguousNameSurfacesIDs(t *testing.T) {
	srv := newTestServer(t, []stubSkill{
		{ID: "abc1", Name: "alpha"},
		{ID: "def2", Name: "alpha"},
	}, "body", nil)
	setupSkillsEnv(t, srv.URL)
	err := runSkillsInstall(skillsInstallCmd, []string{"alpha"})
	if err == nil {
		t.Fatal("expected ambiguity error from install")
	}
	if !strings.Contains(err.Error(), "abc1") || !strings.Contains(err.Error(), "def2") {
		t.Errorf("install error should list both UUIDs: %v", err)
	}
}

// ============================================================================
// delete
// ============================================================================

func TestDelete_CallsDeleteEndpoint(t *testing.T) {
	deleted := map[string]bool{}
	srv := newTestServer(t, []stubSkill{{ID: "to-remove", Name: "alpha"}}, "", &deleted)
	setupSkillsEnv(t, srv.URL)
	skillsDeleteYes = true

	if err := runSkillsDelete(skillsDeleteCmd, []string{"alpha"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted["to-remove"] {
		t.Errorf("DELETE was not called for to-remove: %+v", deleted)
	}
}

func TestDelete_RefusesWithoutYesOnNonTTY(t *testing.T) {
	srv := newTestServer(t, []stubSkill{{ID: "id-1", Name: "alpha"}}, "", nil)
	setupSkillsEnv(t, srv.URL)
	skillsDeleteYes = false // explicit

	// Simulate piped/non-interactive stdin. Restore the real check after.
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = orig })

	err := runSkillsDelete(skillsDeleteCmd, []string{"alpha"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("expected non-TTY refusal, got %v", err)
	}
}

func TestDelete_AmbiguousNameSurfacesIDs(t *testing.T) {
	srv := newTestServer(t, []stubSkill{
		{ID: "abc1", Name: "alpha"},
		{ID: "def2", Name: "alpha"},
	}, "", nil)
	setupSkillsEnv(t, srv.URL)
	skillsDeleteYes = true
	err := runSkillsDelete(skillsDeleteCmd, []string{"alpha"})
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "abc1") {
		t.Errorf("ambiguity error missing UUID: %v", err)
	}
}

func TestDelete_ByUUID(t *testing.T) {
	deleted := map[string]bool{}
	srv := newTestServer(t, []stubSkill{{ID: validUUID, Name: "alpha"}}, "", &deleted)
	setupSkillsEnv(t, srv.URL)
	skillsDeleteYes = true
	if err := runSkillsDelete(skillsDeleteCmd, []string{validUUID}); err != nil {
		t.Fatalf("delete by uuid: %v", err)
	}
	if !deleted[validUUID] {
		t.Errorf("DELETE was not called for %s", validUUID)
	}
}
