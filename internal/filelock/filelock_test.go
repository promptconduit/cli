//go:build !windows

package filelock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusiveExcludesExclusiveAndShared(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "x.lock")
	release, ok := Exclusive(p, time.Second)
	if !ok {
		t.Fatal("first exclusive lock should succeed")
	}
	// flock locks belong to the open file description, so a second open in the
	// same process contends exactly like another process would.
	if _, ok := Exclusive(p, 20*time.Millisecond); ok {
		t.Fatal("second exclusive lock should time out while the first is held")
	}
	if _, ok := Shared(p, 20*time.Millisecond); ok {
		t.Fatal("shared lock should time out while an exclusive lock is held")
	}
	release()
	r2, ok := Exclusive(p, time.Second)
	if !ok {
		t.Fatal("lock should be acquirable after release")
	}
	r2()
}

func TestSharedAllowsSharedButNotExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	r1, ok := Shared(p, time.Second)
	if !ok {
		t.Fatal("shared 1")
	}
	r2, ok := Shared(p, time.Second)
	if !ok {
		t.Fatal("shared 2 should coexist with shared 1")
	}
	if _, ok := Exclusive(p, 20*time.Millisecond); ok {
		t.Fatal("exclusive must wait for shared holders")
	}
	r1()
	r2()
	r3, ok := Exclusive(p, time.Second)
	if !ok {
		t.Fatal("exclusive after shared release")
	}
	r3()
}

func TestWaitAcquiresWhenReleased(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	release, _ := Exclusive(p, time.Second)
	go func() {
		time.Sleep(30 * time.Millisecond)
		release()
	}()
	r, ok := Exclusive(p, 2*time.Second)
	if !ok {
		t.Fatal("waiter should acquire once the holder releases")
	}
	r()
}

func TestRemoveIfUnlocked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	release, ok := Exclusive(p, time.Second)
	if !ok {
		t.Fatal("lock")
	}
	if RemoveIfUnlocked(p) {
		t.Fatal("a held lock file must not be removed")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("held lock file was removed: %v", err)
	}
	release()
	if !RemoveIfUnlocked(p) {
		t.Fatal("a free lock file should be removed")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("lock file still present: %v", err)
	}
}

// TestAcquireRejectsLockOnUnlinkedFile reproduces the race where GC unlinks a
// lock file after we opened it but before we flocked it, and another process
// then locks a freshly created file at the same path. We must not also
// "hold" the lock (on the orphaned inode).
func TestAcquireRejectsLockOnUnlinkedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var other func()
	testHookAfterOpen = func() {
		testHookAfterOpen = nil // fire once
		if !RemoveIfUnlocked(p) {
			t.Error("GC should remove the (unheld) lock file")
		}
		r, ok := Exclusive(p, time.Second) // another process, new inode
		if !ok {
			t.Error("other holder should lock the recreated file")
		}
		other = r
	}
	t.Cleanup(func() { testHookAfterOpen = nil })

	if release, ok := Exclusive(p, 100*time.Millisecond); ok {
		release()
		t.Fatal("acquired a lock on an unlinked inode while another process holds the path: two holders")
	}
	other()
	r, ok := Exclusive(p, time.Second)
	if !ok {
		t.Fatal("lock should be acquirable once the other holder releases")
	}
	r()
}

func TestImpossiblePathFailsFast(t *testing.T) {
	release, ok := Exclusive("/dev/null/nope/x.lock", time.Second)
	if ok {
		t.Fatal("expected failure for an impossible path")
	}
	release() // must be safe
}
