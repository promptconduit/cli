package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSKUForLabels(t *testing.T) {
	cases := map[string]struct {
		labels []string
		runner string
	}{
		"actions_linux":       {[]string{"ubuntu-latest"}, ""},
		"actions_linux_slim":  {[]string{"ubuntu-slim"}, ""},
		"actions_linux_arm":   {[]string{"ubuntu-24.04-arm"}, ""},
		"actions_windows":     {[]string{"windows-latest"}, ""},
		"actions_windows_arm": {[]string{"windows-11-arm"}, ""},
		"actions_macos":       {[]string{"macos-15"}, ""},
		"macos_l":             {[]string{"macos-14-large"}, ""},
		"macos_xl":            {[]string{"macos-15-xlarge"}, ""},
		"linux_8_core":        {[]string{"linux-8-core"}, ""},
		"linux_16_core_arm":   {[]string{"ubuntu-16-cores-arm64"}, ""},
		"windows_4_core_gpu":  {[]string{"windows-4-core-gpu"}, ""},
		"self_hosted":         {[]string{"self-hosted", "linux"}, ""},
	}
	for want, c := range cases {
		if got := SKUForLabels(c.labels, c.runner); got != want {
			t.Errorf("SKUForLabels(%v) = %s, want %s", c.labels, got, want)
		}
	}
	if got := SKUForLabels(nil, "my-box"); got != "self_hosted" {
		t.Errorf("unlabelled custom runner = %s", got)
	}
	if got := SKUForLabels([]string{"linux-7-core"}, ""); got != "actions_linux" {
		t.Errorf("unknown size = %s", got)
	}
}

func TestPricing(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	if m := BillableMinutes(t0, t0.Add(time.Second)); m != 1 {
		t.Errorf("1s = %d min", m)
	}
	if m := BillableMinutes(t0, t0.Add(3*time.Minute)); m != 3 {
		t.Errorf("3m = %d min", m)
	}
	if m := BillableMinutes(time.Time{}, t0); m != 0 {
		t.Errorf("never started = %d", m)
	}
	if !near(JobUSD("actions_linux", 10, true), 0.06) || JobUSD("actions_linux", 10, false) != 0 || !near(JobUSD("linux_8_core", 10, false), 0.22) {
		t.Error("JobUSD")
	}
}

// fixture: PR 7 runs ci on pull_request twice in one hour (superseded first
// run), then its squash merge runs ci again on push:main.
func fixture() *Dataset {
	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	job := func(id int64, start time.Time, mins int) Job {
		return Job{ID: id, Name: "test", Labels: []string{"ubuntu-latest"}, StartedAt: start, CompletedAt: start.Add(time.Duration(mins) * time.Minute)}
	}
	run := func(id int64, event, branch, sha string, at time.Time, mins, pr int) *Run {
		return &Run{ID: id, Name: "ci", Event: event, HeadBranch: branch, HeadSHA: sha, CreatedAt: at, UpdatedAt: at.Add(time.Duration(mins) * time.Minute),
			Conclusion: "success", Jobs: []Job{job(id*10, at, mins)}, PR: pr}
	}
	return &Dataset{Repo: "acme/web", Private: true, DefaultBranch: "main", Since: "2026-11-05", Until: "2026-11-05", Runs: []*Run{
		run(1, "pull_request", "feat/x", "aaa", t0, 10, 7),
		run(2, "pull_request", "feat/x", "bbb", t0.Add(5*time.Minute), 10, 7),
		run(3, "push", "main", "ccc", t0.Add(30*time.Minute), 10, 7),
	}}
}

func TestAnalyze(t *testing.T) {
	idx, err := indexSessions(strings.NewReader(strings.Join([]string{
		`{"session_id":"s1","captured_at":"2026-11-05T09:00:00Z","enrichments":{"vcs":{"repo":"Acme/Web","commit":{"hash":"AAA"}},"cost":{"requests":[{"request_id":"r1","model_priced":true,"usd":{"total":1.5}}]}}}`,
		`{"session_id":"s1","captured_at":"2026-11-05T09:01:00Z","enrichments":{"cost":{"requests":[{"request_id":"r1","model_priced":true,"usd":{"total":1.5}}]},"vcs":{"repo":"acme/web","commit":{"hash":"bbb"},"pr":{"number":7}}}}`,
		`{"session_id":"s2","captured_at":"2026-11-05T09:02:00Z","enrichments":{"vcs":{"repo":"acme/web","pr":{"number":7}}}}`,
		`{"session_id":"x","enrichments":{"vcs":{"repo":"other/repo","commit":{"hash":"ccc"}}}}`,
		`not json "acme/web"`,
	}, "\n")), "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	if !near(idx.Sessions["s1"].TokenUSD, 1.5) {
		t.Errorf("token $ counted each request once: %v", idx.Sessions["s1"].TokenUSD)
	}
	rep := Analyze(fixture(), idx)
	if rep.Runs != 3 || rep.Jobs != 3 || rep.Minutes != 30 || !near(rep.USD, 0.18) {
		t.Fatalf("totals: %+v", rep)
	}
	byName := map[string]Waste{}
	for _, w := range rep.Waste {
		byName[w.Signal] = w
	}
	if w := byName["retest_after_merge"]; w.Runs != 1 || !near(w.USD, 0.06) {
		t.Errorf("retest: %+v", w)
	}
	if w := byName["superseded"]; w.Runs != 1 || !near(w.USD, 0.06) {
		t.Errorf("superseded: %+v", w)
	}
	// aaa and bbb match s1 by commit; the merge push (ccc) falls back to PR 7: s1 + s2.
	if !near(rep.AttributedUSD, 0.18) || len(rep.Sessions) != 2 {
		t.Fatalf("attribution: %v %+v", rep.AttributedUSD, rep.Sessions)
	}
	s1 := rep.Sessions[0]
	if s1.Session != "s1" || !near(s1.CIUSD, 0.15) || s1.CIRuns != 3 || strings.Join(s1.MatchedBy, "+") != "commit+pr" {
		t.Errorf("s1: %+v", s1)
	}
	var buf bytes.Buffer
	WriteText(&buf, rep, 5)
	for _, want := range []string{"$0.18", "Re-test after merge", "Superseded runs", "matched to 2 sessions"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestUploadsChunk(t *testing.T) {
	ds := fixture()
	for i := 0; i < 1200; i++ {
		ds.Runs[0].Jobs = append(ds.Runs[0].Jobs, Job{ID: int64(1000 + i)})
	}
	ups := Uploads(ds)
	if len(ups) != 3 || len(ups[0].Jobs) != 500 || len(ups[2].Jobs) != 203 {
		t.Fatalf("chunks: %d", len(ups))
	}
	j := ups[0].Jobs[0]
	if j.RunID != 1 || j.Event != "pull_request" || j.PRNumber != 7 || j.WorkflowName != "ci" || j.StartedAt != "2026-11-05T10:00:00Z" {
		t.Errorf("job: %+v", j)
	}
}

func TestCollectResolvesSquashMergePR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/repos/acme/web":
			_, _ = fmt.Fprint(w, `{"private":true,"default_branch":"main"}`)
		case p == "/repos/acme/web/actions/runs" && r.URL.Query().Get("created") == "2026-11-05":
			_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":1,"name":"ci","event":"push","head_sha":"ccc","head_branch":"main","created_at":"2026-11-05T10:00:00Z","pull_requests":[]}]}`)
		case p == "/repos/acme/web/actions/runs":
			_, _ = fmt.Fprint(w, `{"workflow_runs":[]}`)
		case p == "/repos/acme/web/actions/runs/1/jobs":
			_, _ = fmt.Fprint(w, `{"jobs":[{"id":11,"name":"test","labels":["ubuntu-latest"],"started_at":"2026-11-05T10:00:00Z","completed_at":"2026-11-05T10:04:00Z"}]}`)
		case p == "/repos/acme/web/pulls":
			_, _ = fmt.Fprint(w, `[]`)
		case p == "/repos/acme/web/commits/ccc/pulls":
			_, _ = fmt.Fprint(w, `[{"number":3,"merged_at":null},{"number":9,"merged_at":"2026-11-05T09:59:00Z"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := NewGitHub("t")
	g.BaseURL = srv.URL
	ds, err := g.Collect("acme/web", "2026-11-04", "2026-11-05", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Runs) != 1 || ds.Runs[0].PR != 9 || len(ds.Runs[0].Jobs) != 1 || ds.APICalls != 6 {
		b, _ := json.Marshal(ds.Runs)
		t.Fatalf("runs %s calls %d", b, ds.APICalls)
	}
}
