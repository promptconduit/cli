package enrich

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sleepyEnricher sleeps, tracking how many stateful enrichers overlap.
type sleepyEnricher struct {
	slug     string
	d        time.Duration
	inFlight *atomic.Int32
	maxSeen  *atomic.Int32
	order    *[]string
	mu       *sync.Mutex
}

func (s sleepyEnricher) Slug() string          { return s.slug }
func (s sleepyEnricher) Applies(*Context) bool { return true }
func (s sleepyEnricher) Enrich(*Context) (any, error) {
	if s.inFlight != nil {
		n := s.inFlight.Add(1)
		for {
			m := s.maxSeen.Load()
			if n <= m || s.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		defer s.inFlight.Add(-1)
	}
	if s.order != nil {
		s.mu.Lock()
		*s.order = append(*s.order, s.slug)
		s.mu.Unlock()
	}
	time.Sleep(s.d)
	return s.slug, nil
}

type statefulSleepy struct{ sleepyEnricher }

func (statefulSleepy) usesSessionState() {}

func TestRun_IndependentEnrichersRunConcurrently(t *testing.T) {
	const d = 150 * time.Millisecond
	withRegistry(t,
		sleepyEnricher{slug: "a", d: d},
		sleepyEnricher{slug: "b", d: d},
		sleepyEnricher{slug: "c", d: d},
		sleepyEnricher{slug: "d", d: d},
	)
	start := time.Now()
	out := Run(&Context{})
	if elapsed := time.Since(start); elapsed > 3*d {
		t.Fatalf("4 x %v enrichers took %v; expected them to overlap", d, elapsed)
	}
	for _, s := range []string{"a", "b", "c", "d"} {
		if string(out[s]) != `"`+s+`"` {
			t.Errorf("slug %s = %s", s, out[s])
		}
	}
}

func TestRun_SessionStateEnrichersNeverOverlapAndKeepOrder(t *testing.T) {
	var inFlight, maxSeen atomic.Int32
	var order []string
	var mu sync.Mutex
	mk := func(slug string) Enricher {
		return statefulSleepy{sleepyEnricher{slug: slug, d: 20 * time.Millisecond, inFlight: &inFlight, maxSeen: &maxSeen, order: &order, mu: &mu}}
	}
	withRegistry(t, mk("s1"), sleepyEnricher{slug: "p", d: 20 * time.Millisecond}, mk("s2"), mk("s3"))
	for i := 0; i < 5; i++ {
		order = nil
		out := Run(&Context{})
		if len(out) != 4 {
			t.Fatalf("out = %v", out)
		}
		if got := strings.Join(order, ","); got != "s1,s2,s3" {
			t.Fatalf("stateful order = %s, want registration order", got)
		}
	}
	if maxSeen.Load() != 1 {
		t.Fatalf("stateful enrichers overlapped (max %d in flight)", maxSeen.Load())
	}
}

func TestRun_OutputIsDeterministic(t *testing.T) {
	withRegistry(t,
		sleepyEnricher{slug: "x", d: time.Millisecond},
		fakeEnricher{slug: "y", applies: true, payload: map[string]int{"n": 1}},
		fakeEnricher{slug: "z", applies: true, payload: nil},
	)
	first, _ := json.Marshal(Run(&Context{}))
	for i := 0; i < 20; i++ {
		got, _ := json.Marshal(Run(&Context{}))
		if string(got) != string(first) {
			t.Fatalf("run %d = %s, want %s", i, got, first)
		}
	}
}

// TestSessionStateUsersAreMarked fails if an enricher file calls
// loadState/saveState but its enricher doesn't implement sessionStateUser —
// it would then run concurrently with the others and race on the state file.
func TestSessionStateUsersAreMarked(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	typeRE := regexp.MustCompile(`(?m)^type (\w+Enricher) struct`)
	marked := map[string]bool{}
	for _, e := range registry {
		if _, ok := e.(sessionStateUser); ok {
			marked[reflect.TypeOf(e).Name()] = true
		}
	}
	checked := 0
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "state.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "loadState(") && !strings.Contains(string(src), "saveState(") {
			continue
		}
		for _, m := range typeRE.FindAllStringSubmatch(string(src), -1) {
			checked++
			if !marked[m[1]] {
				t.Errorf("%s (%s) uses session state but does not implement sessionStateUser", m[1], name)
			}
		}
	}
	if checked < 4 {
		t.Fatalf("expected to check at least the prompt/cost/subagent/turn enrichers, checked %d", checked)
	}
}

// TestRun_RealRegistryConcurrentIsRaceFree drives the real enrichers through a
// prompt -> subagent -> stop sequence (run under -race in CI).
func TestRun_RealRegistryConcurrentIsRaceFree(t *testing.T) {
	// The trace enricher writes under client.ConfigDir(); keep it (and anything
	// else resolving the home dir) inside the test's temp dir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	SetStateDirForTest(t.TempDir())
	t.Cleanup(func() { SetStateDirForTest("") })
	events := []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "SubagentStart", "PostToolUse", "SubagentStop", "Stop"}
	for _, ev := range events {
		Run(&Context{
			Tool:      "claude-code",
			HookEvent: ev,
			SessionID: "race-session",
			RawEvent:  map[string]interface{}{"hook_event_name": ev, "prompt": "hi", "agent_id": "a1", "tool_name": "Bash", "tool_use_id": "t1"},
			RawJSON:   []byte(`{}`),
		})
	}
	st := loadState("race-session")
	if st.PromptCount != 1 {
		t.Fatalf("prompt count = %d, want 1", st.PromptCount)
	}
	if st.TurnStartedAt != "" {
		t.Fatalf("turn should be closed by Stop, got %q", st.TurnStartedAt)
	}
}
