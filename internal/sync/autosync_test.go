package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/filelock"
)

func TestPlanAutoSyncDebouncesWithTrailingEdge(t *testing.T) {
	base := t.TempDir()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	steps := []struct {
		name      string
		at        time.Duration // since t0
		wantSpawn bool
		wantDelay int
	}{
		{"first stop syncs after the flush delay", 0, true, 1},
		{"same-instant stop: the scheduled sync starts a full flush delay later, covered", 0, false, 0},
		{"stop too close to the scheduled start gets its own flush delay", 500 * time.Millisecond, true, 1},
		{"stop after it started schedules one follow-up at the interval end", 10 * time.Second, true, 52},
		{"more stops well before the follow-up are covered", 30 * time.Second, false, 0},
		{"covered while the follow-up is still a flush delay away", 60 * time.Second, false, 0},
		{"too close to the follow-up: extend by a flush delay", 61 * time.Second, true, 1},
		{"stop after that started schedules the next window", 63 * time.Second, true, 59},
		{"long after everything, sync immediately", 10 * time.Minute, true, 1},
	}
	for _, s := range steps {
		delay, spawn := PlanAutoSync(base, "sess-1", t0.Add(s.at))
		if spawn != s.wantSpawn || (spawn && delay != s.wantDelay) {
			t.Fatalf("%s: got spawn=%v delay=%d, want spawn=%v delay=%d", s.name, spawn, delay, s.wantSpawn, s.wantDelay)
		}
	}
}

func TestPlanAutoSyncIsPerSession(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	if _, spawn := PlanAutoSync(base, "a", now); !spawn {
		t.Fatal("first a")
	}
	if _, spawn := PlanAutoSync(base, "b", now); !spawn {
		t.Fatal("session b must not be debounced by session a")
	}
	if !AutoSyncPending(base, "a", now) {
		t.Fatal("a should be pending (scheduled after now)")
	}
	if AutoSyncPending(base, "c", now) {
		t.Fatal("c has no scheduled sync")
	}
}

func TestPlanAutoSyncFallsBackWithoutState(t *testing.T) {
	if d, spawn := PlanAutoSync("", "s", time.Now()); !spawn || d != 1 {
		t.Fatalf("no base dir: want spawn with 1s delay, got %v %d", spawn, d)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"0b9e-ab_C.1": "0b9e-ab_C.1",
		"../../etc":   ".._.._etc",
		"a/b":         "a_b",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safeName(".."); got == ".." || got == "" {
		t.Errorf("safeName(..) = %q must not be a dot path", got)
	}
}

func TestLockTranscriptExcludesSameFileOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	base := t.TempDir()
	release, ok := LockTranscript(base, "/t/a.jsonl", time.Second)
	if !ok {
		t.Fatal("first lock")
	}
	if _, ok := LockTranscript(base, "/t/a.jsonl", 20*time.Millisecond); ok {
		t.Fatal("a second upload of the same transcript must wait")
	}
	other, ok := LockTranscript(base, "/t/b.jsonl", 20*time.Millisecond)
	if !ok {
		t.Fatal("a different transcript must not be blocked")
	}
	other()
	release()
	again, ok := LockTranscript(base, "/t/a.jsonl", time.Second)
	if !ok {
		t.Fatal("lock must be free after release")
	}
	again()
}

// TestPlanAutoSyncEveryStopIsCoveredAfterFlush checks the invariant on a
// pseudo-random Stop stream: every Stop is followed by a scheduled sync that
// starts at least the flush delay after it.
func TestPlanAutoSyncEveryStopIsCoveredAfterFlush(t *testing.T) {
	base := t.TempDir()
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var starts []time.Time
	now := t0
	seed := uint32(176)
	for i := 0; i < 400; i++ {
		seed = seed*1664525 + 1013904223
		now = now.Add(time.Duration(seed%7000) * time.Millisecond) // 0-7s gaps
		if delay, spawn := PlanAutoSync(base, "s", now); spawn {
			starts = append(starts, now.Add(time.Duration(delay)*time.Second))
		}
		covered := false
		for _, s := range starts {
			if !s.Before(now.Add(autoSyncFlushDelay)) {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("stop %d at %v has no sync starting >= flush delay after it", i, now.Sub(t0))
		}
	}
	if limit := int(now.Sub(t0)/AutoSyncInterval)*3 + 3; len(starts) > limit {
		t.Fatalf("debounce ineffective: %d syncs over %v", len(starts), now.Sub(t0))
	}
}

func TestAutoSyncPendingRequiresFlushDelay(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	PlanAutoSync(base, "s", now) // scheduled at now+1s
	if !AutoSyncPending(base, "s", now) {
		t.Fatal("a sync a full flush delay out covers this stop")
	}
	if AutoSyncPending(base, "s", now.Add(500*time.Millisecond)) {
		t.Fatal("a sync starting <1s after this stop must not count as covering it")
	}
}

func TestAutoSyncSweepKeepsHeldLocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	base := t.TempDir()
	dir := filepath.Join(base, autoSyncSubdir)
	locks := filepath.Join(dir, "locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{ // path -> held
		filepath.Join(dir, "s1.next"):      false,
		filepath.Join(dir, "s1.next.lock"): true,
		filepath.Join(dir, "s2.next.lock"): false,
		filepath.Join(locks, "aaaa.lock"):  true,
		filepath.Join(locks, "bbbb.lock"):  false,
		filepath.Join(dir, "fresh.next"):   false,
	}
	old := time.Now().Add(-2 * autoSyncStampMaxAge)
	for p, held := range files {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if held {
			release, ok := filelock.Exclusive(p, time.Second)
			if !ok {
				t.Fatal("lock")
			}
			defer release()
		}
		if filepath.Base(p) != "fresh.next" {
			_ = os.Chtimes(p, old, old)
		}
	}
	maybeSweepAutoSync(dir, time.Now())
	for p, held := range files {
		_, err := os.Stat(p)
		present := err == nil
		want := held || filepath.Base(p) == "fresh.next"
		if present != want {
			t.Errorf("%s: present=%v, want %v", filepath.Base(p), present, want)
		}
	}
}
