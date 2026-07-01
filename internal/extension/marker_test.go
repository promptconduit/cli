package extension

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/promptconduit/cli/internal/eventlog"
)

func TestWriteUpdateMarker(t *testing.T) {
	dir := t.TempDir()
	eventlog.SetDirForTest(dir)
	t.Cleanup(func() { eventlog.SetDirForTest("") })

	if err := WriteUpdateMarker("1.2.3", "Cursor"); err != nil {
		t.Fatalf("WriteUpdateMarker: %v", err)
	}

	data, err := os.ReadFile(MarkerPath())
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m UpdateMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("marker is not valid JSON: %v", err)
	}
	if m.Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", m.Version)
	}
	if m.Editor != "Cursor" {
		t.Errorf("editor = %q, want Cursor", m.Editor)
	}
	if m.UpdatedAt == "" {
		t.Error("updated_at is empty; want an RFC3339 timestamp")
	}
}

func TestWriteUpdateMarkerOverwrites(t *testing.T) {
	dir := t.TempDir()
	eventlog.SetDirForTest(dir)
	t.Cleanup(func() { eventlog.SetDirForTest("") })

	if err := WriteUpdateMarker("1.0.0", "VS Code"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteUpdateMarker("2.0.0", "Cursor"); err != nil {
		t.Fatalf("second write: %v", err)
	}

	data, err := os.ReadFile(MarkerPath())
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m UpdateMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Version != "2.0.0" || m.Editor != "Cursor" {
		t.Errorf("latest write not reflected: got version=%q editor=%q", m.Version, m.Editor)
	}

	// A single marker file, not a pile of leftover temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly one file (the marker), got %d: %v", len(entries), names)
	}
}
