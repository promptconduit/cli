package ci

import (
	"fmt"
	"io"
	"strings"
	"time"
)

func usd(n float64) string { return fmt.Sprintf("$%.2f", n) }

// WriteText writes a one-screen report: total, top waste with the $ next to
// each fix, top PRs and, when the event log was read, sessions.
func WriteText(w io.Writer, r *Report, top int) {
	vis := "private repo"
	if !r.Private {
		vis = "public repo: standard runners are free"
	}
	_, _ = fmt.Fprintf(w, "CI cost audit: %s, %s to %s\n\n", r.Repo, r.Since, r.Until)
	_, _ = fmt.Fprintf(w, "  %s  %s min across %d runs, %d jobs (%s)\n", usd(r.USD), commas(r.Minutes), r.Runs, r.Jobs, vis)
	_, _ = fmt.Fprintf(w, "  Estimate from GitHub's rate card (effective %s), each job rounded up to the minute;\n  your plan's included minutes are not subtracted.\n\n", r.RatesEffective)

	_, _ = fmt.Fprintln(w, "Waste found")
	any := false
	for _, x := range r.Waste {
		if x.Count == 0 {
			continue
		}
		any = true
		pct := ""
		if r.USD > 0 {
			pct = fmt.Sprintf(" (%d%%)", int(100*x.USD/r.USD+0.5))
		}
		_, _ = fmt.Fprintf(w, "  %-22s %7s%s  %d runs, %s min\n", x.Title, usd(x.USD), pct, x.Runs, commas(x.Minutes))
		if x.Detail != "" {
			_, _ = fmt.Fprintf(w, "  %-22s %s\n", "", x.Detail)
		}
		_, _ = fmt.Fprintf(w, "  %-22s fix: %s\n", "", x.Fix)
	}
	if !any {
		_, _ = fmt.Fprintln(w, "  nothing over the thresholds")
	}
	if len(r.SpikeDays) > 0 {
		parts := make([]string, 0, len(r.SpikeDays))
		for _, d := range r.SpikeDays {
			parts = append(parts, fmt.Sprintf("%s %s (%d runs)", d.Key, usd(d.USD), d.Runs))
		}
		_, _ = fmt.Fprintf(w, "  Spike days (> %dx the %s median day): %s\n", SpikeFactor, usd(r.MedianDayUSD), strings.Join(parts, ", "))
	}

	_, _ = fmt.Fprintf(w, "\nTop PRs and branches\n")
	for _, g := range limit(r.ByPR, top) {
		_, _ = fmt.Fprintf(w, "  %-32s %7s  %3d runs  %5s min\n", trunc(g.Key, 32), usd(g.USD), g.Runs, commas(g.Minutes))
	}
	_, _ = fmt.Fprintf(w, "\nBy workflow\n")
	for _, g := range limit(r.ByWorkflow, top) {
		_, _ = fmt.Fprintf(w, "  %-32s %7s  %3d runs  %5s min\n", trunc(g.Key, 32), usd(g.USD), g.Runs, commas(g.Minutes))
	}

	if r.SessionsRead {
		pct := 0
		if r.USD > 0 {
			pct = int(100*r.AttributedUSD/r.USD + 0.5)
		}
		_, _ = fmt.Fprintf(w, "\nAgent sessions (CI $ next to token $)\n  %s of %s (%d%%) matched to %d sessions by commit or PR\n",
			usd(r.AttributedUSD), usd(r.USD), pct, len(r.Sessions))
		for _, s := range limit(r.Sessions, top) {
			started := s.Started
			if t, err := time.Parse(time.RFC3339, s.Started); err == nil {
				started = t.Local().Format("Jan 02 15:04")
			}
			prs := make([]string, 0, len(s.PRs))
			for _, p := range s.PRs {
				prs = append(prs, fmt.Sprintf("#%d", p))
			}
			_, _ = fmt.Fprintf(w, "  %s  %-12s CI %7s  tokens %7s  %2d runs  %s\n",
				trunc(s.Session, 8), started, usd(s.CIUSD), usd(s.TokenUSD), s.CIRuns, trunc(strings.Join(prs, " "), 24))
		}
	}
}

func limit[T any](xs []T, n int) []T {
	if n > 0 && len(xs) > n {
		return xs[:n]
	}
	return xs
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func commas(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// UploadJob is one job in the /v1/ci/jobs payload (GitHub REST field names).
type UploadJob struct {
	ID            int64    `json:"id"`
	RunID         int64    `json:"run_id"`
	RunAttempt    int      `json:"run_attempt"`
	WorkflowName  string   `json:"workflow_name"`
	Name          string   `json:"name"`
	Event         string   `json:"event"`
	HeadSHA       string   `json:"head_sha"`
	HeadBranch    string   `json:"head_branch"`
	PRNumber      int      `json:"pr_number,omitempty"`
	Labels        []string `json:"labels"`
	RunnerName    string   `json:"runner_name,omitempty"`
	RunCreatedAt  string   `json:"run_created_at"`
	RunUpdatedAt  string   `json:"run_updated_at,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	CompletedAt   string   `json:"completed_at,omitempty"`
	Conclusion    string   `json:"conclusion,omitempty"`
	RunConclusion string   `json:"run_conclusion,omitempty"`
}

// Upload is one POST /v1/ci/jobs body.
type Upload struct {
	Repo          string      `json:"repo"`
	Private       bool        `json:"private"`
	DefaultBranch string      `json:"default_branch"`
	Jobs          []UploadJob `json:"jobs"`
}

// MaxUploadJobs is the service's per-request cap.
const MaxUploadJobs = 500

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Uploads splits a dataset into request bodies of at most MaxUploadJobs jobs.
func Uploads(ds *Dataset) []Upload {
	var out []Upload
	cur := Upload{Repo: ds.Repo, Private: ds.Private, DefaultBranch: ds.DefaultBranch}
	for _, r := range ds.Runs {
		for _, j := range r.Jobs {
			attempt := j.RunAttempt
			if attempt == 0 {
				attempt = r.RunAttempt
			}
			wf := j.WorkflowName
			if wf == "" {
				wf = r.Name
			}
			cur.Jobs = append(cur.Jobs, UploadJob{
				ID: j.ID, RunID: r.ID, RunAttempt: attempt, WorkflowName: wf, Name: j.Name, Event: r.Event,
				HeadSHA: r.HeadSHA, HeadBranch: r.HeadBranch, PRNumber: r.PR, Labels: j.Labels, RunnerName: j.RunnerName,
				RunCreatedAt: ts(r.CreatedAt), RunUpdatedAt: ts(r.UpdatedAt), StartedAt: ts(j.StartedAt),
				CompletedAt: ts(j.CompletedAt), Conclusion: j.Conclusion, RunConclusion: r.Conclusion,
			})
			if len(cur.Jobs) == MaxUploadJobs {
				out = append(out, cur)
				cur = Upload{Repo: ds.Repo, Private: ds.Private, DefaultBranch: ds.DefaultBranch}
			}
		}
	}
	if len(cur.Jobs) > 0 {
		out = append(out, cur)
	}
	return out
}
