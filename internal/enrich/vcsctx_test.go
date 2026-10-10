package enrich

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptconduit/cli/internal/envelope"
)

type stubRepo struct {
	calls   int
	gitDir  string
	extract func() *envelope.GitContext // overrides the default result when set
}

// stubExtract replaces git extraction with a counter for the test's duration.
// The returned context's GitDir/CommonDir is a temp dir with HEAD/index files,
// so the fingerprint is real and tests can mutate it.
func stubExtract(t *testing.T) *stubRepo {
	t.Helper()
	SetStateDirForTest(t.TempDir())
	r := &stubRepo{gitDir: t.TempDir()}
	for _, f := range []string{"HEAD", "index"} {
		r.touch(t, f)
	}
	orig := extractGitContext
	extractGitContext = func(string) *envelope.GitContext {
		r.calls++
		if r.extract != nil {
			return r.extract()
		}
		return &envelope.GitContext{Branch: "main", GitDir: r.gitDir, CommonDir: r.gitDir}
	}
	t.Cleanup(func() {
		extractGitContext = orig
		SetStateDirForTest("")
	})
	return r
}

// touch creates or rewrites a file under the stub git dir with a distinct mtime.
func (r *stubRepo) touch(t *testing.T, name string) {
	t.Helper()
	p := filepath.Join(r.gitDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(time.Duration(r.calls+1) * time.Second)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func pre(cwd string) *Context  { return &Context{Cwd: cwd, HookEvent: "PreToolUse"} }
func post(cwd string) *Context { return &Context{Cwd: cwd, HookEvent: "PostToolUse"} }

func TestGitContext_HighFrequencyEventsReuseSnapshot(t *testing.T) {
	r := stubExtract(t)
	for i := 0; i < 6; i++ {
		if gc := gitContext(pre("/repo")); gc == nil || gc.Branch != "main" {
			t.Fatalf("call %d: got %+v", i, gc)
		}
	}
	// The first snapshot for a cwd has no "before" fingerprint to vouch for it,
	// so one more extraction verifies it; everything after is served cached.
	if r.calls != 2 {
		t.Errorf("extractions = %d, want 2", r.calls)
	}

	// A different cwd has its own snapshot.
	gitContext(post("/other"))
	if r.calls != 3 {
		t.Errorf("extractions after new cwd = %d, want 3", r.calls)
	}
}

func TestGitContext_BoundaryEventsAlwaysFresh(t *testing.T) {
	r := stubExtract(t)
	for _, ev := range []string{"UserPromptSubmit", "Stop", "SessionStart", "SubagentStop", "SomeFutureEvent"} {
		gitContext(&Context{Cwd: "/repo", HookEvent: ev})
	}
	if r.calls != 5 {
		t.Errorf("extractions = %d, want 5 (boundary/unknown events never use the snapshot)", r.calls)
	}

	// ...and boundary extractions seed the snapshot for tool events.
	gitContext(post("/repo"))
	if r.calls != 5 {
		t.Errorf("extractions = %d, want 5 (tool event reuses the boundary snapshot)", r.calls)
	}
}

// A git-state change inside the TTL (the tool call ran commit/checkout/push/
// fetch/remote set-url) invalidates the snapshot, so PostToolUse sees it.
func TestGitContext_RepoChangeInvalidatesSnapshot(t *testing.T) {
	for _, name := range []string{
		"HEAD", "index", "logs/HEAD", "refs/heads/main",
		"refs/remotes/origin/main", "packed-refs", "FETCH_HEAD", "config",
	} {
		t.Run(name, func(t *testing.T) {
			r := stubExtract(t)
			gitContext(pre("/repo"))
			gitContext(pre("/repo")) // vouched
			before := r.calls
			r.touch(t, name)
			gitContext(post("/repo"))
			if r.calls != before+1 {
				t.Errorf("change to %s did not re-extract", name)
			}
		})
	}
}

// A change that lands DURING extraction must not be vouched for: the data was
// read before it, the fingerprint after.
func TestGitContext_ChangeDuringExtractionNotVouched(t *testing.T) {
	r := stubExtract(t)
	gitContext(pre("/repo"))
	gitContext(pre("/repo")) // vouched snapshot exists

	r.touch(t, "index") // invalidate, then change again mid-extraction
	r.extract = func() *envelope.GitContext {
		r.touch(t, "logs/HEAD")
		return &envelope.GitContext{Branch: "main", GitDir: r.gitDir, CommonDir: r.gitDir}
	}
	gitContext(pre("/repo"))
	r.extract = nil
	before := r.calls
	gitContext(post("/repo"))
	if r.calls != before+1 {
		t.Errorf("snapshot taken across a mid-extraction change was trusted")
	}
}

func TestGitContext_StaleSnapshotReextracts(t *testing.T) {
	r := stubExtract(t)
	gc := &envelope.GitContext{Branch: "stale", GitDir: r.gitDir, CommonDir: r.gitDir}
	path := vcsSnapshotPath("/repo")

	for _, capturedAt := range []time.Time{
		time.Now().Add(-2 * vcsSnapshotTTL), // expired
		time.Now().Add(time.Hour),           // clock moved backwards
	} {
		data, _ := json.Marshal(vcsSnapshot{CapturedAt: capturedAt, Fingerprint: repoFingerprint(gc), Git: gc})
		if !writeFileAtomic(filepath.Dir(path), path, data) {
			t.Fatal("seeding snapshot failed")
		}
		before := r.calls
		if got := gitContext(pre("/repo")); got == nil || got.Branch != "main" {
			t.Errorf("capturedAt %v: got %+v, want fresh extraction", capturedAt, got)
		}
		if r.calls != before+1 {
			t.Errorf("capturedAt %v: snapshot was trusted", capturedAt)
		}
	}
}

// A snapshot without git dirs (e.g. written by an older build) can't be
// change-checked, so it is never trusted.
func TestGitContext_SnapshotWithoutDirsRejected(t *testing.T) {
	r := stubExtract(t)
	path := vcsSnapshotPath("/repo")
	data, _ := json.Marshal(vcsSnapshot{CapturedAt: time.Now(), Fingerprint: "", Git: &envelope.GitContext{Branch: "old"}})
	if !writeFileAtomic(filepath.Dir(path), path, data) {
		t.Fatal("seeding snapshot failed")
	}
	if got := gitContext(pre("/repo")); got == nil || got.Branch != "main" || r.calls != 1 {
		t.Errorf("got %+v after %d extractions; dir-less snapshot must not be trusted", got, r.calls)
	}
}

// nil (not a repo, or a failed rev-parse) and Degraded (status failed) results
// are never cached: a transient failure must not blank or falsify the slug.
func TestGitContext_DoesNotCacheFailures(t *testing.T) {
	for name, result := range map[string]func(string) *envelope.GitContext{
		"nil": func(string) *envelope.GitContext { return nil },
		"degraded": func(dir string) *envelope.GitContext {
			return &envelope.GitContext{GitDir: dir, CommonDir: dir, Degraded: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := stubExtract(t)
			r.extract = func() *envelope.GitContext { return result(r.gitDir) }
			for i := 0; i < 3; i++ {
				gitContext(pre("/repo"))
			}
			if r.calls != 3 {
				t.Errorf("extractions = %d, want 3 (%s results not cached)", r.calls, name)
			}
		})
	}
}
