package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/sync"
)

// TestSingleFileSyncClearsStaleFailure: a retry of a transcript that is already
// synced at its current content uploads nothing and drops the stale failure,
// instead of leaving it for every later Stop hook to respawn.
func TestSingleFileSyncClearsStaleFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	path := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := sync.CalculateFileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	sm, _ := sync.NewStateManager()
	sm.AddFailedSync("sess", path, "API error: 500")
	// Synced before the failure was recorded, so MarkSynced keeps the failure.
	sm.MarkSynced(path, sync.SyncedFileInfo{Hash: hash, SyncedAt: "2000-01-01T00:00:00Z"})
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	if !sm.HasFailedSyncForFile(path) {
		t.Fatal("setup: the failure should survive an older MarkSynced")
	}

	// Nothing listens on this URL: an upload attempt would fail.
	cfg := &client.Config{APIKey: "k", APIURL: "http://127.0.0.1:1", TimeoutSeconds: 1}
	if err := runSingleFileSync(cfg, sm, path); err != nil {
		t.Fatalf("runSingleFileSync: %v", err)
	}
	if reloaded, _ := sync.NewStateManager(); reloaded.HasFailedSyncForFile(path) {
		t.Fatalf("stale failure kept: %+v", reloaded.GetFailedSyncs())
	}
}

// TestSingleFileSyncClearsFailureForMissingFile: a failed transcript that has
// since been deleted can never be retried; its record is dropped.
func TestSingleFileSyncClearsFailureForMissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	path := filepath.Join(t.TempDir(), "gone.jsonl")
	sm, _ := sync.NewStateManager()
	sm.AddFailedSync("gone", path, "API error: 500")
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	if err := runSingleFileSync(&client.Config{APIKey: "k"}, sm, path); err == nil {
		t.Fatal("want a not-found error")
	}
	if reloaded, _ := sync.NewStateManager(); reloaded.HasFailedSyncForFile(path) {
		t.Fatalf("failure for a missing file kept: %+v", reloaded.GetFailedSyncs())
	}
}

func TestRecordLockTimeoutQueuesRetry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	sm, err := sync.NewStateManager()
	if err != nil {
		t.Fatal(err)
	}
	path := "/t/projects/x/0b9e-sess.jsonl"
	if err := recordLockTimeout(sm, path, errors.New("lock wait expired")); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := sync.NewStateManager()
	fs := reloaded.GetFailedSyncs()
	if len(fs) != 1 || fs[0].SessionID != "0b9e-sess" || fs[0].FilePath != path || fs[0].RetryCount >= 3 {
		t.Fatalf("failed syncs = %+v, want one retryable entry for %s", fs, path)
	}
}

func TestScheduleAutoSyncRollsBackOnSpawnFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	base := t.TempDir()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	prev := startAutoSyncProcess
	t.Cleanup(func() { startAutoSyncProcess = prev })

	startAutoSyncProcess = func(string, int) (string, error) { return "", errors.New("exec failed") }
	if _, started := scheduleAutoSync(base, "s1", "/t/s1.jsonl", now); started {
		t.Fatal("spawn failed: must not report started")
	}
	if sync.AutoSyncPending(base, "s1", now) {
		t.Fatal("failed spawn left a schedule stamp: later Stops would be suppressed")
	}

	var spawned []int
	startAutoSyncProcess = func(_ string, delay int) (string, error) {
		spawned = append(spawned, delay)
		return "/bin/pc", nil
	}
	if _, started := scheduleAutoSync(base, "s1", "/t/s1.jsonl", now.Add(100*time.Millisecond)); !started {
		t.Fatal("the next Stop after a failed spawn must start a sync")
	}
	if _, started := scheduleAutoSync(base, "s1", "/t/s1.jsonl", now.Add(100*time.Millisecond)); started {
		t.Fatal("a successful spawn's schedule still debounces the same instant")
	}
	if len(spawned) != 1 || spawned[0] != 1 {
		t.Fatalf("spawned = %v, want one sync with the 1s flush delay", spawned)
	}
}
