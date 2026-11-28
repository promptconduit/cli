package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Run is a workflow run with its jobs and the PR it belongs to.
type Run struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Event        string    `json:"event"`
	HeadSHA      string    `json:"head_sha"`
	HeadBranch   string    `json:"head_branch"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Conclusion   string    `json:"conclusion"`
	RunAttempt   int       `json:"run_attempt"`
	HTMLURL      string    `json:"html_url"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`

	Jobs []Job `json:"-"`
	PR   int   `json:"-"`
}

// Job is one job attempt.
type Job struct {
	ID           int64     `json:"id"`
	RunID        int64     `json:"run_id"`
	RunAttempt   int       `json:"run_attempt"`
	Name         string    `json:"name"`
	WorkflowName string    `json:"workflow_name"`
	HeadSHA      string    `json:"head_sha"`
	HeadBranch   string    `json:"head_branch"`
	Labels       []string  `json:"labels"`
	RunnerName   string    `json:"runner_name"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	Conclusion   string    `json:"conclusion"`
}

// Dataset is everything an audit needs for one repo and date range.
type Dataset struct {
	Repo          string
	Private       bool
	DefaultBranch string
	Since, Until  string // YYYY-MM-DD, inclusive
	Runs          []*Run
	APICalls      int
}

// GitHub is a minimal read-only REST client.
type GitHub struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
	mu      sync.Mutex
	calls   int
}

// Token finds a GitHub token: GITHUB_TOKEN, GH_TOKEN, then `gh auth token`.
func Token() (string, error) {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", errors.New("no GitHub token: set GITHUB_TOKEN or sign in with `gh auth login`")
	}
	return strings.TrimSpace(string(out)), nil
}

// NewGitHub returns a client for api.github.com.
func NewGitHub(token string) *GitHub {
	return &GitHub{Token: token, BaseURL: "https://api.github.com", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Calls is the number of API requests made so far.
func (g *GitHub) Calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *GitHub) get(path string, out interface{}) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("GET", g.BaseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+g.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "promptconduit-cli")
		g.mu.Lock()
		g.calls++
		g.mu.Unlock()
		resp, err := g.HTTP.Do(req)
		if err != nil {
			if attempt < 3 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			return json.Unmarshal(body, out)
		}
		if (resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < 3 {
			time.Sleep(time.Duration(2*(attempt+1)) * time.Second)
			continue
		}
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("GitHub %d on %s: %s", resp.StatusCode, strings.SplitN(path, "?", 2)[0], msg)
	}
}

// Collect reads runs created in [since, until] (UTC days), every job attempt
// of each run, and resolves each run's PR. progress, when set, gets short
// status lines.
func (g *GitHub) Collect(repo, since, until string, progress func(string)) (*Dataset, error) {
	if progress == nil {
		progress = func(string) {}
	}
	var info struct {
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.get("/repos/"+repo, &info); err != nil {
		return nil, err
	}
	start, err := time.Parse("2006-01-02", since)
	if err != nil {
		return nil, fmt.Errorf("bad --since %q (want YYYY-MM-DD)", since)
	}
	end, err := time.Parse("2006-01-02", until)
	if err != nil {
		return nil, fmt.Errorf("bad --until %q (want YYYY-MM-DD)", until)
	}
	ds := &Dataset{Repo: repo, Private: info.Private, DefaultBranch: info.DefaultBranch, Since: since, Until: until}

	// The runs search caps at 1,000 results per query, so page day by day.
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		day := d.Format("2006-01-02")
		for page := 1; page <= 10; page++ {
			var r struct {
				WorkflowRuns []*Run `json:"workflow_runs"`
			}
			if err := g.get(fmt.Sprintf("/repos/%s/actions/runs?created=%s&per_page=100&page=%d", repo, day, page), &r); err != nil {
				return nil, err
			}
			ds.Runs = append(ds.Runs, r.WorkflowRuns...)
			if len(r.WorkflowRuns) < 100 {
				break
			}
		}
	}
	progress(fmt.Sprintf("%d runs; reading jobs...", len(ds.Runs)))

	if err := pool(len(ds.Runs), 8, func(i int) error {
		run := ds.Runs[i]
		for page := 1; page <= 5; page++ {
			var r struct {
				Jobs []Job `json:"jobs"`
			}
			// filter=all includes earlier attempts: re-runs are billed too.
			if err := g.get(fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=all&per_page=100&page=%d", repo, run.ID, page), &r); err != nil {
				return err
			}
			run.Jobs = append(run.Jobs, r.Jobs...)
			if len(r.Jobs) < 100 {
				break
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if err := g.resolvePRs(ds, progress); err != nil {
		return nil, err
	}
	ds.APICalls = g.Calls()
	return ds, nil
}

// resolvePRs: the run's own PR, else a PR whose head is the run's commit or
// branch (forks leave pull_requests empty), else the PR the commit belongs to
// (GET /commits/{sha}/pulls), which is how a squash-merge push to the default
// branch finds its PR.
func (g *GitHub) resolvePRs(ds *Dataset, progress func(string)) error {
	type pull struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	bySHA := map[string]int{}
	byBranch := map[string]int{}
	since, _ := time.Parse("2006-01-02", ds.Since)
	for page := 1; page <= 20; page++ {
		var ps []pull
		if err := g.get(fmt.Sprintf("/repos/%s/pulls?state=all&per_page=100&page=%d&sort=updated&direction=desc", ds.Repo, page), &ps); err != nil {
			return err
		}
		for _, p := range ps {
			bySHA[p.Head.SHA] = p.Number
			if _, ok := byBranch[p.Head.Ref]; !ok {
				byBranch[p.Head.Ref] = p.Number
			}
		}
		if len(ps) < 100 || (len(ps) > 0 && ps[len(ps)-1].UpdatedAt.Before(since.AddDate(0, 0, -30))) {
			break
		}
	}
	var lookups []*Run
	for _, r := range ds.Runs {
		switch {
		case len(r.PullRequests) > 0:
			r.PR = r.PullRequests[0].Number
		case bySHA[r.HeadSHA] != 0:
			r.PR = bySHA[r.HeadSHA]
		case r.HeadBranch != ds.DefaultBranch && byBranch[r.HeadBranch] != 0:
			r.PR = byBranch[r.HeadBranch]
		case r.HeadSHA != "":
			lookups = append(lookups, r)
		}
	}
	if len(lookups) > 0 {
		progress(fmt.Sprintf("matching %d pushes to their PRs...", len(lookups)))
	}
	cache := map[string]int{}
	var mu sync.Mutex
	shas := []string{}
	for _, r := range lookups {
		if _, ok := cache[r.HeadSHA]; !ok {
			cache[r.HeadSHA] = 0
			shas = append(shas, r.HeadSHA)
		}
	}
	_ = pool(len(shas), 8, func(i int) error {
		var prs []struct {
			Number   int     `json:"number"`
			MergedAt *string `json:"merged_at"`
		}
		if err := g.get(fmt.Sprintf("/repos/%s/commits/%s/pulls", ds.Repo, shas[i]), &prs); err != nil || len(prs) == 0 {
			return nil // best effort: the run keeps no PR
		}
		n := prs[0].Number
		for _, p := range prs {
			if p.MergedAt != nil {
				n = p.Number
				break
			}
		}
		mu.Lock()
		cache[shas[i]] = n
		mu.Unlock()
		return nil
	})
	for _, r := range lookups {
		r.PR = cache[r.HeadSHA]
	}
	return nil
}

func pool(n, workers int, fn func(i int) error) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		next     int
		firstErr error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= n || firstErr != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				if err := fn(i); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}
