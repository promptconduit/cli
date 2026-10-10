package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"
)

func TestStateSaveMergesConcurrentManagers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sync_state.json")
	a := newStateManagerAt(p)
	b := newStateManagerAt(p) // loaded before a saves: stale in-memory copy

	a.MarkSynced("/t/a.jsonl", SyncedFileInfo{Hash: "ha"})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	b.MarkSynced("/t/b.jsonl", SyncedFileInfo{Hash: "hb"})
	b.AddFailedSync("s1", "/t/c.jsonl", "boom")
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}

	got := newStateManagerAt(p)
	if !got.IsSynced("/t/a.jsonl", "ha") {
		t.Error("b's save erased a's record")
	}
	if !got.IsSynced("/t/b.jsonl", "hb") {
		t.Error("b's record missing")
	}
	if len(got.GetFailedSyncs()) != 1 {
		t.Errorf("failed syncs = %v", got.GetFailedSyncs())
	}
}

func TestStateSaveReplaysRelativeOps(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sync_state.json")
	a := newStateManagerAt(p)
	a.AddFailedSync("s1", "/t/x", "e1")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	// Two managers each record a retry from the same starting point: both
	// increments must survive.
	b, c := newStateManagerAt(p), newStateManagerAt(p)
	b.AddFailedSync("s1", "/t/x", "e2")
	c.AddFailedSync("s1", "/t/x", "e3")
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	fs := newStateManagerAt(p).GetFailedSyncs()
	if len(fs) != 1 || fs[0].RetryCount != 2 || fs[0].LastError != "e3" {
		t.Fatalf("failed sync = %+v, want retry 2 / e3", fs)
	}
}

func TestStateSaveRefusesToOverwriteCorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sync_state.json")
	corrupt := []byte(`{"synced_files": {"/t/a.jsonl": {"hash": "h`) // torn
	if err := os.WriteFile(p, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	sm := newStateManagerAt(p)
	sm.MarkSynced("/t/b.jsonl", SyncedFileInfo{Hash: "hb"})
	if err := sm.Save(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Save err = %v, want ErrStateCorrupt", err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != string(corrupt) {
		t.Fatalf("corrupt state must be left untouched, got %q", data)
	}
}

func TestStateSaveIsAtomicUnderConcurrency(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sync_state.json")
	const n = 25
	var wg gosync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sm := newStateManagerAt(p)
			sm.MarkSynced(fmt.Sprintf("/t/%d.jsonl", i), SyncedFileInfo{Hash: "h"})
			errs[i] = sm.Save()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	got := newStateManagerAt(p)
	for i := 0; i < n; i++ {
		if !got.IsSynced(fmt.Sprintf("/t/%d.jsonl", i), "h") {
			t.Errorf("record %d lost", i)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".sync_state-*")); len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

func TestStatePendingUploadLifecycle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sync_state.json")
	sm := newStateManagerAt(p)
	sm.SetPendingUpload("/t/a", PendingUploadInfo{SourceFileHash: "h1"})
	sm.UpdatePendingUploadProgress("/t/a", 3)
	if err := sm.Save(); err != nil {
		t.Fatal(err)
	}
	re := newStateManagerAt(p)
	info, ok := re.GetPendingUpload("/t/a", "h1")
	if !ok || info.ChunksUploaded != 3 {
		t.Fatalf("pending = %+v ok=%v", info, ok)
	}
	if _, ok := re.GetPendingUpload("/t/a", "h2"); ok {
		t.Fatal("hash change must invalidate the pending upload")
	}
	if err := re.Save(); err != nil {
		t.Fatal(err)
	}
	if _, ok := newStateManagerAt(p).GetPendingUpload("/t/a", "h1"); ok {
		t.Fatal("stale pending upload should have been removed on save")
	}
}
