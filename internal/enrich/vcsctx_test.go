package enrich

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
)

// stubExtract replaces git extraction with a counter for the test's duration.
// The returned context's GitDir is a temp dir with HEAD/index files, so the
// fingerprint is real and tests can mutate it.
func stubExtract(t *testing.T) (*int, string) {
	t.Helper()
	SetStateDirForTest(t.TempDir())
	gitDir := t.TempDir()
	for _, f := range []string{"HEAD", "index"} {
		if err := os.WriteFile(filepath.Join(gitDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	orig := extractGitContext
	extractGitContext = func(string) *envelope.GitContext {
		calls++
		return &envelope.GitContext{Branch: "main", GitDir: gitDir}
	}
	t.Cleanup(func() {
		extractGitContext = orig
		SetStateDirForTest("")
	})
	return &calls, gitDir
}

func TestGitContext_HighFrequencyEventsReuseSnapshot(t *testing.T) {
	calls, _ := stubExtract(t)
	ctx := &Context{Cwd: "/repo", HookEvent: "PreToolUse"}

	for i := 0; i < 5; i++ {
		if gc := gitContext(ctx); gc == nil || gc.Branch != "main" {
			t.Fatalf("call %d: got %+v", i, gc)
		}
	}
	if *calls != 1 {
		t.Errorf("extractions = %d, want 1 (later tool events served from snapshot)", *calls)
	}

	// A different cwd has its own snapshot.
	gitContext(&Context{Cwd: "/other", HookEvent: "PostToolUse"})
	if *calls != 2 {
		t.Errorf("extractions after new cwd = %d, want 2", *calls)
	}
}

func TestGitContext_BoundaryEventsAlwaysFresh(t *testing.T) {
	calls, _ := stubExtract(t)
	for _, ev := range []string{"UserPromptSubmit", "Stop", "SessionStart", "SubagentStop", "SomeFutureEvent"} {
		gitContext(&Context{Cwd: "/repo", HookEvent: ev})
	}
	if *calls != 5 {
		t.Errorf("extractions = %d, want 5 (boundary/unknown events never use the snapshot)", *calls)
	}

	// ...and a boundary event's fresh extraction seeds the snapshot for tool events.
	gitContext(&Context{Cwd: "/repo", HookEvent: "PostToolUse"})
	if *calls != 5 {
		t.Errorf("extractions = %d, want 5 (tool event reuses the boundary snapshot)", *calls)
	}
}

// A git-state change inside the TTL (the tool call ran `git commit`/`checkout`)
// invalidates the snapshot, so PostToolUse sees the new state.
func TestGitContext_RepoChangeInvalidatesSnapshot(t *testing.T) {
	calls, gitDir := stubExtract(t)
	gitContext(&Context{Cwd: "/repo", HookEvent: "PreToolUse"})

	later := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(gitDir, "HEAD"), later, later); err != nil {
		t.Fatal(err)
	}
	gitContext(&Context{Cwd: "/repo", HookEvent: "PostToolUse"})
	if *calls != 2 {
		t.Errorf("extractions = %d, want 2 (HEAD change must re-extract)", *calls)
	}

	// A new reflog appearing counts as a change too.
	if err := os.MkdirAll(filepath.Join(gitDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "logs", "HEAD"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitContext(&Context{Cwd: "/repo", HookEvent: "PostToolBatch"})
	if *calls != 3 {
		t.Errorf("extractions = %d, want 3 (reflog change must re-extract)", *calls)
	}
}

func TestGitContext_StaleSnapshotReextracts(t *testing.T) {
	calls, gitDir := stubExtract(t)
	ctx := &Context{Cwd: "/repo", HookEvent: "PreToolUse"}
	path := vcsSnapshotPath(ctx.Cwd)

	for _, capturedAt := range []time.Time{
		time.Now().Add(-2 * vcsSnapshotTTL), // expired
		time.Now().Add(time.Hour),           // clock moved backwards
	} {
		data, _ := json.Marshal(vcsSnapshot{
			CapturedAt:  capturedAt,
			Fingerprint: repoFingerprint(gitDir),
			Git:         &envelope.GitContext{Branch: "stale", GitDir: gitDir},
		})
		if !writeFileAtomic(filepath.Dir(path), path, data) {
			t.Fatal("seeding snapshot failed")
		}
		before := *calls
		if gc := gitContext(ctx); gc == nil || gc.Branch != "main" {
			t.Errorf("capturedAt %v: got %+v, want fresh extraction", capturedAt, gc)
		}
		if *calls != before+1 {
			t.Errorf("capturedAt %v: snapshot was trusted", capturedAt)
		}
	}
}

// nil (not a repo, or a failed rev-parse) is never cached: a transient
// failure must not blank the vcs slug for the whole TTL.
func TestGitContext_DoesNotCacheNil(t *testing.T) {
	calls, _ := stubExtract(t)
	extractGitContext = func(string) *envelope.GitContext { *calls++; return nil }
	ctx := &Context{Cwd: "/not-a-repo", HookEvent: "PreToolUse"}
	gitContext(ctx)
	gitContext(ctx)
	if *calls != 2 {
		t.Errorf("extractions = %d, want 2 (nil results not cached)", *calls)
	}
}
