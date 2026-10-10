package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/sync"
)

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
