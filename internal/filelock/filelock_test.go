//go:build !windows

package filelock

import (
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

func TestImpossiblePathFailsFast(t *testing.T) {
	release, ok := Exclusive("/dev/null/nope/x.lock", time.Second)
	if ok {
		t.Fatal("expected failure for an impossible path")
	}
	release() // must be safe
}
