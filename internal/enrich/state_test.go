package enrich

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/filelock"
)

func chtimes(p string, t time.Time) error { return os.Chtimes(p, t, t) }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func withStateDir(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })
}

// TestUpdateState_ConcurrentWritersLoseNothing models several hook processes
// (or abandoned enricher goroutines) updating one session at once: every
// increment must survive. flock is per open file description, so goroutines
// contend exactly like separate processes.
func TestUpdateState_ConcurrentWritersLoseNothing(t *testing.T) {
	withStateDir(t)
	prev := stateLockWait
	stateLockWait = 30 * time.Second // test the lock, not the busy fallback
	t.Cleanup(func() { stateLockWait = prev })

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			updateState("s1", func(st *sessionState) bool {
				st.PromptCount++
				return true
			})
		}()
	}
	wg.Wait()
	if got := loadState("s1").PromptCount; got != n {
		t.Fatalf("prompt count = %d, want %d (lost updates)", got, n)
	}
}

func TestUpdateState_SkipsWhenLockBusy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	withStateDir(t)
	prev := stateLockWait
	stateLockWait = 30 * time.Millisecond
	t.Cleanup(func() { stateLockWait = prev })

	updateState("s1", func(st *sessionState) bool { st.PromptCount = 5; return true })
	release, ok := filelock.Exclusive(stateLockPath("s1"), time.Second)
	if !ok {
		t.Fatal("could not take the state lock")
	}
	defer release()

	ran := false
	if updateState("s1", func(st *sessionState) bool { ran = true; st.PromptCount = 99; return true }) {
		t.Fatal("updateState must report failure while the lock is held")
	}
	if ran {
		t.Fatal("fn must not run without the lock")
	}
	if got := loadState("s1").PromptCount; got != 5 {
		t.Fatalf("state written without the lock: count = %d", got)
	}

	// The prompt enricher degrades to a read-only view instead of writing.
	out, err := promptEnricher{}.Enrich(&Context{HookEvent: "UserPromptSubmit", SessionID: "s1", RawEvent: map[string]interface{}{"prompt": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := out.(PromptEnrichment); p.Count != 6 {
		t.Fatalf("read-only fallback count = %d, want 6", p.Count)
	}
	if got := loadState("s1").PromptCount; got != 5 {
		t.Fatalf("prompt enricher wrote unlocked: count = %d", got)
	}
}

func TestStateGCKeepsHeldLockFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	withStateDir(t)
	updateState("held", func(st *sessionState) bool { st.PromptCount = 1; return true })
	updateState("free", func(st *sessionState) bool { st.PromptCount = 1; return true })
	release, _ := filelock.Exclusive(stateLockPath("held"), time.Second)
	defer release()

	old := time.Now().Add(-2 * stateMaxAge)
	for _, p := range []string{stateLockPath("held"), stateLockPath("free")} {
		if err := chtimes(p, old); err != nil {
			t.Fatal(err)
		}
	}
	gcDir(stateDir(), stateMaxAge)
	if !exists(stateLockPath("held")) {
		t.Error("GC removed a lock file that is held")
	}
	if exists(stateLockPath("free")) {
		t.Error("GC should remove a stale, unheld lock file")
	}
}
