package enrich

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
)

// stubExtract replaces git extraction with a counter for the test's duration.
func stubExtract(t *testing.T, gc *envelope.GitContext) *int {
	t.Helper()
	SetStateDirForTest(t.TempDir())
	calls := 0
	orig := extractGitContext
	extractGitContext = func(string) *envelope.GitContext { calls++; return gc }
	t.Cleanup(func() {
		extractGitContext = orig
		SetStateDirForTest("")
	})
	return &calls
}

func TestGitContext_HighFrequencyEventsReuseSnapshot(t *testing.T) {
	calls := stubExtract(t, &envelope.GitContext{Branch: "main"})
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
	calls := stubExtract(t, &envelope.GitContext{Branch: "main"})
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

func TestGitContext_StaleSnapshotReextracts(t *testing.T) {
	calls := stubExtract(t, &envelope.GitContext{Branch: "main"})
	ctx := &Context{Cwd: "/repo", HookEvent: "PreToolUse"}
	path := vcsSnapshotPath(ctx.Cwd)

	for _, capturedAt := range []time.Time{
		time.Now().Add(-2 * vcsSnapshotTTL), // expired
		time.Now().Add(time.Hour),           // clock moved backwards
	} {
		data, _ := json.Marshal(vcsSnapshot{CapturedAt: capturedAt, Git: &envelope.GitContext{Branch: "stale"}})
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

func TestGitContext_CachesNonRepo(t *testing.T) {
	calls := stubExtract(t, nil)
	ctx := &Context{Cwd: "/not-a-repo", HookEvent: "PreToolUse"}
	if gc := gitContext(ctx); gc != nil {
		t.Fatalf("got %+v, want nil", gc)
	}
	if gc := gitContext(ctx); gc != nil {
		t.Fatalf("got %+v, want nil", gc)
	}
	if *calls != 1 {
		t.Errorf("extractions = %d, want 1 (non-repo result cached too)", *calls)
	}
}
