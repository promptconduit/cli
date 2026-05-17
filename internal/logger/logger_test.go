package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetDirForTest(dir)
	SetDebug(false)
	t.Cleanup(func() {
		SetDirForTest("")
	})
	return dir
}

func TestError_AlwaysWrites(t *testing.T) {
	dir := setup(t)

	Error("boom: %s", "network down")

	data, err := os.ReadFile(filepath.Join(dir, "promptconduit.log"))
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "ERROR") {
		t.Errorf("expected ERROR level marker, got: %s", s)
	}
	if !strings.Contains(s, "boom: network down") {
		t.Errorf("expected formatted message, got: %s", s)
	}
}

func TestDebug_GatedOnSetDebug(t *testing.T) {
	dir := setup(t)

	Debug("trace-1")

	if _, err := os.Stat(filepath.Join(dir, "promptconduit.log")); !os.IsNotExist(err) {
		t.Errorf("expected no log file when Debug disabled (err=%v)", err)
	}

	SetDebug(true)
	Debug("trace-2")

	data, err := os.ReadFile(filepath.Join(dir, "promptconduit.log"))
	if err != nil {
		t.Fatalf("expected log file after enabling debug: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "trace-1") {
		t.Errorf("trace-1 should not appear (debug off when written): %s", s)
	}
	if !strings.Contains(s, "trace-2") {
		t.Errorf("trace-2 should appear: %s", s)
	}
}

func TestRotation(t *testing.T) {
	dir := setup(t)

	path := filepath.Join(dir, "promptconduit.log")
	// Seed the active file just over MaxBytes so the next write rotates.
	big := strings.Repeat("x", int(MaxBytes)+1)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	Error("post-rotation line")

	if _, err := os.Stat(filepath.Join(dir, "promptconduit.log.1")); err != nil {
		t.Errorf("expected backup file after rotation: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "post-rotation line") {
		t.Errorf("expected new file to start with post-rotation line: %s", string(data))
	}
	// The new file must be much smaller than the original (only the one new line).
	if int64(len(data)) > MaxBytes/2 {
		t.Errorf("new log file should be small after rotation, got %d bytes", len(data))
	}
}

func TestTail(t *testing.T) {
	setup(t)

	for i := 0; i < 5; i++ {
		Error("line-%d", i)
	}

	out, err := Tail(2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line-3") || !strings.Contains(out, "line-4") {
		t.Errorf("expected last two lines, got: %q", out)
	}
	if strings.Contains(out, "line-0") || strings.Contains(out, "line-2") {
		t.Errorf("tail should not include earlier lines, got: %q", out)
	}
}

func TestTail_MissingFile(t *testing.T) {
	setup(t)
	out, err := Tail(10)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output, got: %q", out)
	}
}
