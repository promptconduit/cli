package extension

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedVSIXIsPresentAndAZip(t *testing.T) {
	b := Bytes()
	if len(b) == 0 {
		t.Fatal("embedded vsix is empty — was `make refresh-extension` run?")
	}
	// .vsix is a zip; every zip starts with the local-file-header magic "PK".
	if len(b) < 2 || b[0] != 'P' || b[1] != 'K' {
		t.Error("embedded vsix doesn't look like a zip (missing PK magic)")
	}
}

func TestVersionReadsFromEmbeddedVSIX(t *testing.T) {
	v, err := Version()
	if err != nil {
		t.Fatalf("Version() error: %v", err)
	}
	if v == "" {
		t.Fatal("Version() returned empty string")
	}
	// Cached call returns the same value.
	if v2, _ := Version(); v2 != v {
		t.Errorf("Version() not stable: %q then %q", v, v2)
	}
}

func TestResolveCLIReturnsEmptyWhenMissing(t *testing.T) {
	e := Editor{Name: "Nope", Command: "promptconduit-no-such-editor-cli"}
	if got := e.resolveCLI(); got != "" {
		t.Errorf("resolveCLI() = %q, want empty for a nonexistent command", got)
	}
}

func TestInstallSkipsWhenEditorNotFound(t *testing.T) {
	e := Editor{Name: "Nope", Command: "promptconduit-no-such-editor-cli"}
	res, err := Install(e)
	if err != nil {
		t.Fatalf("Install() should not error when the editor is just missing: %v", err)
	}
	if res.Installed {
		t.Error("Installed=true for a nonexistent editor")
	}
	if res.SkippedReason == "" {
		t.Error("expected a SkippedReason when the editor CLI isn't found")
	}
	if res.Version == "" {
		t.Error("Version should still be populated even when skipped")
	}
}

func TestWriteTempVSIXRoundTrips(t *testing.T) {
	path, cleanup, err := writeTempVSIX()
	if err != nil {
		t.Fatalf("writeTempVSIX: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp vsix: %v", err)
	}
	if !bytes.Equal(got, Bytes()) {
		t.Error("temp vsix content does not match the embedded bytes")
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove the temp vsix: %v", err)
	}
}
