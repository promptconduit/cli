package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTranscript creates a fake Claude Code transcript at
// <projectsRoot>/<folder>/<sessionID>.jsonl with the given raw lines.
func writeTranscript(t *testing.T, projectsRoot, folder, sessionID string, lines ...string) {
	t.Helper()
	dir := filepath.Join(projectsRoot, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveLaunchDir_ReadsFirstCwd(t *testing.T) {
	root := t.TempDir()
	// The folder name is Claude Code's mangled encoding; the launch dir must come
	// from the transcript's recorded cwd, not from decoding the folder.
	writeTranscript(t, root, "-Users-x-GitHub-promptconduit", "sid-1",
		`{"type":"summary","summary":"no cwd here"}`,
		`{"type":"user","cwd":"/Users/x/GitHub/promptconduit","message":{}}`,
	)
	if got := resolveLaunchDir(root, "sid-1"); got != "/Users/x/GitHub/promptconduit" {
		t.Errorf("resolveLaunchDir = %q, want the launch dir from the transcript", got)
	}
}

func TestResolveLaunchDir_MissingReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	if got := resolveLaunchDir(root, "nope"); got != "" {
		t.Errorf("resolveLaunchDir for missing session = %q, want empty", got)
	}
	if got := resolveLaunchDir("", "sid"); got != "" {
		t.Errorf("resolveLaunchDir with empty root = %q, want empty", got)
	}
}

func TestEnrichLaunchDirs_OverridesToolCwd_KeepsFallback(t *testing.T) {
	root := t.TempDir()
	// s1 has a transcript whose launch dir (repo root) differs from the tool cwd
	// the event log recorded (an --add-dir subdir). s2 has no transcript.
	writeTranscript(t, root, "-Users-x-GitHub-promptconduit", "s1",
		`{"type":"user","cwd":"/Users/x/GitHub/promptconduit"}`,
	)

	list := []Session{
		{SessionID: "s1", Cwd: "/Users/x/GitHub/promptconduit/editor-extension"},
		{SessionID: "s2", Cwd: "/Users/x/GitHub/other"},
	}
	enrichLaunchDirsFrom(root, list)

	if list[0].Cwd != "/Users/x/GitHub/promptconduit" {
		t.Errorf("s1 cwd = %q, want the launch dir (transcript wins over event cwd)", list[0].Cwd)
	}
	if list[1].Cwd != "/Users/x/GitHub/other" {
		t.Errorf("s2 cwd = %q, want the event cwd retained when no transcript", list[1].Cwd)
	}
}
