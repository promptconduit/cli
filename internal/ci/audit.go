package ci

import (
	"fmt"
	"sort"
	"strings"
)

// Thresholds for the waste signals.
const (
	StormRuns     = 4  // runs of one workflow on one PR within an hour
	BurstRuns     = 10 // runs across the repo within an hour
	SpikeFactor   = 3  // day cost over this many median days
	SlowFactor    = 2  // job over this many times its median duration
	SlowMinMinute = 5
)

// Group is a cost bucket (day, PR, workflow).
type Group struct {
	Key     string  `json:"key"`
	Runs    int     `json:"runs"`
	Minutes int     `json:"minutes"`
	USD     float64 `json:"usd"`
}

// Waste is one waste signal with its total and a fix.
type Waste struct {
	Signal  string  `json:"signal"`
	Title   string  `json:"title"`
	Count   int     `json:"count"`
	Runs    int     `json:"runs"`
	Minutes int     `json:"minutes"`
	USD     float64 `json:"usd"`
	Fix     string  `json:"fix"`
	Detail  string  `json:"detail,omitempty"`
}

// SessionCost is CI cost attributed to one agent session.
type SessionCost struct {
	Session   string   `json:"session"`
	Started   string   `json:"started,omitempty"`
	PRs       []int    `json:"prs,omitempty"`
	CIRuns    int      `json:"ci_runs"`
	CIMinutes float64  `json:"ci_minutes"`
	CIUSD     float64  `json:"ci_usd"`
	TokenUSD  float64  `json:"token_usd"`
	MatchedBy []string `json:"matched_by"`
}

// Report is the result of an audit.
type Report struct {
	Repo           string        `json:"repo"`
	Since          string        `json:"since"`
	Until          string        `json:"until"`
	Private        bool          `json:"private"`
	RatesEffective string        `json:"rates_effective"`
	Runs           int           `json:"runs"`
	Jobs           int           `json:"jobs"`
	Minutes        int           `json:"minutes"`
	USD            float64       `json:"usd"`
	ByDay          []Group       `json:"by_day"`
	ByPR           []Group       `json:"by_pr"`
	ByWorkflow     []Group       `json:"by_workflow"`
	BySKU          []Group       `json:"by_runner"`
	Waste          []Waste       `json:"waste"`
	MedianDayUSD   float64       `json:"median_day_usd"`
	SpikeDays      []Group       `json:"spike_days"`
	Sessions       []SessionCost `json:"sessions,omitempty"`
	AttributedUSD  float64       `json:"attributed_usd"`
	SessionsRead   bool          `json:"sessions_read"`
	APICalls       int           `json:"api_calls"`
}

type pricedRun struct {
	*Run
	minutes int
	usd     float64
	jobs    []pricedJob
}

type pricedJob struct {
	Job
	sku     string
	minutes int
	usd     float64
}

func priceRuns(ds *Dataset) []pricedRun {
	out := make([]pricedRun, 0, len(ds.Runs))
	for _, r := range ds.Runs {
		pr := pricedRun{Run: r}
		for _, j := range r.Jobs {
			sku := SKUForLabels(j.Labels, j.RunnerName)
			m := BillableMinutes(j.StartedAt, j.CompletedAt)
			c := JobUSD(sku, m, ds.Private)
			pr.jobs = append(pr.jobs, pricedJob{Job: j, sku: sku, minutes: m, usd: c})
			pr.minutes += m
			pr.usd += c
		}
		out = append(out, pr)
	}
	return out
}

func group(runs []pricedRun, key func(pricedRun) string) []Group {
	m := map[string]*Group{}
	for _, r := range runs {
		k := key(r)
		g := m[k]
		if g == nil {
			g = &Group{Key: k}
			m[k] = g
		}
		g.Runs++
		g.Minutes += r.minutes
		g.USD += r.usd
	}
	out := make([]Group, 0, len(m))
	for _, g := range m {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func hourKey(r pricedRun) string { return r.CreatedAt.UTC().Format("2006-01-02T15") }

// Analyze prices a dataset, groups it, finds waste and, when sessions is not
// nil, attributes runs to agent sessions.
func Analyze(ds *Dataset, sessions *SessionIndex) *Report {
	runs := priceRuns(ds)
	rep := &Report{Repo: ds.Repo, Since: ds.Since, Until: ds.Until, Private: ds.Private, RatesEffective: RatesEffective, Runs: len(runs), APICalls: ds.APICalls}
	for _, r := range runs {
		rep.Minutes += r.minutes
		rep.USD += r.usd
		rep.Jobs += len(r.jobs)
	}
	rep.ByDay = group(runs, func(r pricedRun) string { return r.CreatedAt.UTC().Format("2006-01-02") })
	sort.Slice(rep.ByDay, func(i, j int) bool { return rep.ByDay[i].Key < rep.ByDay[j].Key })
	rep.ByPR = group(runs, func(r pricedRun) string {
		if r.PR > 0 {
			return fmt.Sprintf("#%d", r.PR)
		}
		return r.Event + ":" + r.HeadBranch
	})
	rep.ByWorkflow = group(runs, func(r pricedRun) string { return r.Name })
	skus := map[string]*Group{}
	for _, r := range runs {
		for _, j := range r.jobs {
			g := skus[j.sku]
			if g == nil {
				g = &Group{Key: j.sku}
				skus[j.sku] = g
			}
			g.Runs++
			g.Minutes += j.minutes
			g.USD += j.usd
		}
	}
	for _, g := range skus {
		rep.BySKU = append(rep.BySKU, *g)
	}
	sort.Slice(rep.BySKU, func(i, j int) bool { return rep.BySKU[i].USD > rep.BySKU[j].USD })

	rep.Waste = findWaste(ds, runs, rep)
	if sessions != nil {
		rep.SessionsRead = true
		rep.Sessions, rep.AttributedUSD = attribute(runs, sessions)
	}
	return rep
}

func findWaste(ds *Dataset, runs []pricedRun, rep *Report) []Waste {
	var out []Waste

	// Re-test after merge: a workflow that ran on pull_request runs again on
	// the push to the default branch (a squash merge is a new commit).
	prWorkflows := map[string]bool{}
	for _, r := range runs {
		if r.Event == "pull_request" {
			prWorkflows[r.Name] = true
		}
	}
	retest := Waste{Signal: "retest_after_merge", Title: "Re-test after merge",
		Fix: "Split test and deploy; skip tests on the default-branch push when the PR passed, or use a merge queue."}
	for _, r := range runs {
		if r.Event == "push" && r.HeadBranch == ds.DefaultBranch && prWorkflows[r.Name] {
			retest.Count++
			retest.Runs++
			retest.Minutes += r.minutes
			retest.USD += r.usd
		}
	}
	out = append(out, retest)

	// Repo-wide bursts: >= BurstRuns runs across the repo within an hour.
	burst := Waste{Signal: "burst", Title: "Repo-wide bursts",
		Fix: "Batch agent PRs, or use a merge queue so a wave of small PRs doesn't run full CI each."}
	byHour := map[string][]pricedRun{}
	for _, r := range runs {
		byHour[hourKey(r)] = append(byHour[hourKey(r)], r)
	}
	var burstHours []string
	for h, rs := range byHour {
		if len(rs) < BurstRuns {
			continue
		}
		burst.Count++
		burstHours = append(burstHours, h)
		for _, r := range rs {
			burst.Runs++
			burst.Minutes += r.minutes
			burst.USD += r.usd
		}
	}
	sort.Strings(burstHours)
	if len(burstHours) > 0 {
		burst.Detail = "hours (UTC): " + strings.Join(firstN(burstHours, 5), ", ")
	}
	out = append(out, burst)

	// Per-PR push storms: >= StormRuns runs of one workflow on one PR in an hour.
	storm := Waste{Signal: "push_storm", Title: "Per-PR push storms",
		Fix: "Tell the agent to push less often (commit locally, push once), and add concurrency to the workflow."}
	stormKey := map[string][]pricedRun{}
	for _, r := range runs {
		if r.PR > 0 {
			k := fmt.Sprintf("%d|%s|%s", r.PR, r.Name, hourKey(r))
			stormKey[k] = append(stormKey[k], r)
		}
	}
	for _, rs := range stormKey {
		if len(rs) < StormRuns {
			continue
		}
		storm.Count++
		for _, r := range rs {
			storm.Runs++
			storm.Minutes += r.minutes
			storm.USD += r.usd
		}
	}
	out = append(out, storm)

	// Superseded: a newer commit on the same branch + workflow started before
	// this run finished, and this run still ran to the end. Default-branch
	// pushes are skipped: deploys there usually should all run.
	super := Waste{Signal: "superseded", Title: "Superseded runs",
		Fix: "Add `concurrency: { group: ${{ github.workflow }}-${{ github.ref }}, cancel-in-progress: true }`."}
	byBranch := map[string][]pricedRun{}
	for _, r := range runs {
		k := r.Name + "|" + r.HeadBranch
		byBranch[k] = append(byBranch[k], r)
	}
	for _, rs := range byBranch {
		sort.Slice(rs, func(i, j int) bool { return rs[i].CreatedAt.Before(rs[j].CreatedAt) })
		for i := 0; i+1 < len(rs); i++ {
			r, next := rs[i], rs[i+1]
			if r.Event == "push" && r.HeadBranch == ds.DefaultBranch {
				continue
			}
			if next.HeadSHA != r.HeadSHA && next.CreatedAt.Before(r.UpdatedAt) && r.Conclusion != "cancelled" {
				super.Count++
				super.Runs++
				super.Minutes += r.minutes
				super.USD += r.usd
			}
		}
	}
	out = append(out, super)

	// Slow-job outliers: > SlowFactor x the job's median and >= SlowMinMinute.
	slow := Waste{Signal: "slow_job", Title: "Slow-job outliers",
		Fix: "Cache dependencies, split long jobs, and watch the job's duration trend."}
	durs := map[string][]int{}
	for _, r := range runs {
		for _, j := range r.jobs {
			if j.minutes > 0 {
				k := r.Name + "/" + j.Name
				durs[k] = append(durs[k], j.minutes)
			}
		}
	}
	slowJobs := map[string]bool{}
	for _, r := range runs {
		for _, j := range r.jobs {
			xs := durs[r.Name+"/"+j.Name]
			if len(xs) < 5 {
				continue
			}
			med := medianInt(xs)
			if j.minutes >= SlowMinMinute && float64(j.minutes) > SlowFactor*float64(med) {
				slow.Count++
				slow.Runs++
				slow.Minutes += j.minutes - med
				slow.USD += float64(j.minutes-med) * Rates[j.sku]
				slowJobs[r.Name+"/"+j.Name] = true
			}
		}
	}
	if len(slowJobs) > 0 {
		names := make([]string, 0, len(slowJobs))
		for k := range slowJobs {
			names = append(names, k)
		}
		sort.Strings(names)
		slow.Detail = "jobs: " + strings.Join(firstN(names, 5), ", ")
	}
	out = append(out, slow)

	// Spike days: > SpikeFactor x the median day (and over $1).
	costs := make([]float64, len(rep.ByDay))
	for i, d := range rep.ByDay {
		costs[i] = d.USD
	}
	sort.Float64s(costs)
	if len(costs) > 0 {
		rep.MedianDayUSD = costs[len(costs)/2]
	}
	for _, d := range rep.ByDay {
		if d.USD > SpikeFactor*rep.MedianDayUSD && d.USD > 1 {
			rep.SpikeDays = append(rep.SpikeDays, d)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].USD > out[j].USD })
	return out
}

func attribute(runs []pricedRun, idx *SessionIndex) ([]SessionCost, float64) {
	type acc struct {
		runs    int
		minutes float64
		usd     float64
		how     map[string]bool
	}
	by := map[string]*acc{}
	attributed := 0.0
	for _, r := range runs {
		// Exact: a session whose HEAD was this commit. Fallback: sessions that
		// worked on the run's PR. Split the run evenly across matches.
		ids, how := idx.BySHA[r.HeadSHA], "commit"
		if len(ids) == 0 && r.PR > 0 {
			ids, how = idx.ByPR[r.PR], "pr"
		}
		if len(ids) == 0 {
			continue
		}
		attributed += r.usd
		n := float64(len(ids))
		for id := range ids {
			a := by[id]
			if a == nil {
				a = &acc{how: map[string]bool{}}
				by[id] = a
			}
			a.runs++
			a.minutes += float64(r.minutes) / n
			a.usd += r.usd / n
			a.how[how] = true
		}
	}
	out := make([]SessionCost, 0, len(by))
	for id, a := range by {
		s := idx.Sessions[id]
		sc := SessionCost{Session: id, CIRuns: a.runs, CIMinutes: a.minutes, CIUSD: a.usd}
		if s != nil {
			sc.Started = s.First
			sc.TokenUSD = s.TokenUSD
			for pr := range s.PRs {
				sc.PRs = append(sc.PRs, pr)
			}
			sort.Ints(sc.PRs)
		}
		for h := range a.how {
			sc.MatchedBy = append(sc.MatchedBy, h)
		}
		sort.Strings(sc.MatchedBy)
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CIUSD != out[j].CIUSD {
			return out[i].CIUSD > out[j].CIUSD
		}
		return out[i].Session < out[j].Session
	})
	return out, attributed
}

func medianInt(xs []int) int {
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return s[len(s)/2]
}

func firstN(xs []string, n int) []string {
	if len(xs) > n {
		return append(append([]string(nil), xs[:n]...), fmt.Sprintf("+%d more", len(xs)-n))
	}
	return xs
}
