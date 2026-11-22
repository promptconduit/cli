package transcriptcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissing(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(present, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Missing(present) {
		t.Error("existing transcript reported missing")
	}
	if !Missing(filepath.Join(dir, "nope.jsonl")) {
		t.Error("absent transcript not reported missing")
	}
	if !Missing("") {
		t.Error("empty transcript path not reported missing")
	}
}

func TestObserveRecordsOnceAndListsNewestFirst(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)
	absent := filepath.Join(base, "projects", "a.jsonl")

	missing, first := Observe(base, "sess-a", absent, "/repo/a", now)
	if !missing || !first {
		t.Fatalf("first Stop: missing=%v first=%v, want true,true", missing, first)
	}
	// A later Stop in the same session is still missing but not "first", and
	// keeps the original FirstSeen.
	missing, first = Observe(base, "sess-a", absent, "/repo/a", now.Add(time.Hour))
	if !missing || first {
		t.Fatalf("second Stop: missing=%v first=%v, want true,false", missing, first)
	}
	Observe(base, "sess-b", "", "/repo/b", now.Add(time.Minute))

	got := List(base, now.Add(2*time.Hour))
	if len(got) != 2 {
		t.Fatalf("List = %+v, want 2 entries", got)
	}
	if got[0].SessionID != "sess-b" || got[1].SessionID != "sess-a" {
		t.Errorf("order = %s, %s; want newest first", got[0].SessionID, got[1].SessionID)
	}
	if !got[1].FirstSeen.Equal(now) || got[1].Cwd != "/repo/a" || got[1].TranscriptPath != absent {
		t.Errorf("entry = %+v", got[1])
	}
	if msg := got[1].Message(); !strings.Contains(msg, "sess-a") || !strings.Contains(msg, "CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1") {
		t.Errorf("Message = %q", msg)
	}
}

func TestObserveClearsWhenTranscriptExists(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	path := filepath.Join(base, "s.jsonl")
	Observe(base, "sess", path, "", now)
	if len(List(base, now)) != 1 {
		t.Fatal("not recorded")
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if missing, _ := Observe(base, "sess", path, "", now); missing {
		t.Error("existing transcript reported missing")
	}
	if got := List(base, now); len(got) != 0 {
		t.Errorf("List after transcript appeared = %+v", got)
	}
}

func TestListDropsTranscriptsThatAppearedAndOldEntries(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	late := filepath.Join(base, "late.jsonl")
	Observe(base, "late", late, "", now)
	Observe(base, "old", "", "", now.Add(-MaxAge-time.Hour))
	Observe(base, "fresh", "", "", now)
	if err := os.WriteFile(late, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := List(base, now)
	if len(got) != 1 || got[0].SessionID != "fresh" {
		t.Fatalf("List = %+v, want only fresh", got)
	}
	// Pruned records are removed from disk, not just hidden.
	files, _ := os.ReadDir(filepath.Join(base, subdir))
	if len(files) != 1 {
		t.Errorf("%d record files left, want 1", len(files))
	}
}

func TestObserveIgnoresEmptySessionAndHostileIDs(t *testing.T) {
	base := t.TempDir()
	if missing, first := Observe(base, "", "", "", time.Now()); missing || first {
		t.Error("empty session id recorded")
	}
	Observe(base, "../../escape", "", "", time.Now())
	files, err := os.ReadDir(filepath.Join(base, subdir))
	if err != nil || len(files) != 1 {
		t.Fatalf("record files = %v, err = %v", files, err)
	}
	if strings.Contains(files[0].Name(), "..") {
		t.Errorf("hostile id used as file name: %s", files[0].Name())
	}
	if got := List(base, time.Now()); len(got) != 1 || got[0].SessionID != "../../escape" {
		t.Errorf("List = %+v", got)
	}
}

func TestEmptyBaseDir(t *testing.T) {
	if missing, first := Observe("", "s", "", "", time.Now()); !missing || first {
		t.Errorf("Observe with no base dir: missing=%v first=%v", missing, first)
	}
	if got := List("", time.Now()); got != nil {
		t.Errorf("List with no base dir = %+v", got)
	}
}
