package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/cost"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

// costDay is one calendar day (local time) of priced requests. CostTotal and
// the token/request counts include subagent spend (Claude Code writes each
// subagent to its own transcript, priced on SubagentStop); SubagentCost is
// that share. UnpricedModels names the models behind Unpriced, with counts.
type costDay struct {
	Day            string         `json:"day"`
	CostTotal      float64        `json:"cost_total"`
	SubagentCost   float64        `json:"subagent_cost"`
	Input          int64          `json:"input"`
	Output         int64          `json:"output"`
	CacheRead      int64          `json:"cache_read"`
	CacheWrite     int64          `json:"cache_write"`
	Requests       int            `json:"requests"`
	Unpriced       int            `json:"unpriced"`
	UnpricedModels map[string]int `json:"unpriced_models,omitempty"`
	Currency       string         `json:"currency"`
}

// historyEnvelope is the slice of a v2 envelope that cost history reads: the
// `cost` enrichment's requests, the `subagent` enrichment's priced totals, plus
// captured_at as a timestamp fallback.
type historyEnvelope struct {
	CapturedAt  string `json:"captured_at"`
	Enrichments struct {
		Cost *struct {
			Requests []struct {
				RequestID   string      `json:"request_id"`
				Timestamp   string      `json:"ts"`
				Model       string      `json:"model"`
				ModelPriced bool        `json:"model_priced"`
				Tokens      cost.Tokens `json:"tokens"`
				USD         cost.Cost   `json:"usd"`
			} `json:"requests"`
		} `json:"cost"`
		Subagent *historySubagent `json:"subagent"`
	} `json:"enrichments"`
}

// historySubagent is the SubagentStop slice of the `subagent` enrichment: the
// subagent transcript's priced totals. The main-turn `cost` enrichment never
// includes these requests (they live in a separate transcript), so adding them
// does not double count.
type historySubagent struct {
	AgentID          string       `json:"agent_id"`
	Phase            string       `json:"phase"`
	Requests         int          `json:"requests"`
	Model            string       `json:"model"`
	Tokens           *cost.Tokens `json:"tokens"`
	USD              *cost.Cost   `json:"usd"`
	ModelPriced      *bool        `json:"model_priced"`
	UnpricedRequests int          `json:"unpriced_requests"`
	UnpricedModel    string       `json:"unpriced_model"`
}

// costEnrichmentKey and subagentEnrichmentKey cheaply pre-filter event-log
// lines: most envelopes carry neither, and full JSON decoding of large tool
// payloads is wasted.
var (
	costEnrichmentKey     = []byte(`"cost":`)
	subagentEnrichmentKey = []byte(`"subagent":`)
)

// requestTime returns the request's timestamp: its own `ts`, else the
// envelope's captured_at (also when `ts` is present but unparseable).
func requestTime(ts, capturedAt string) (time.Time, bool) {
	for _, s := range []string{ts, capturedAt} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// aggregateCostHistory sums the `cost` and `subagent` enrichments in an
// events.jsonl stream into per-day totals for the last `days` calendar days
// (today included), newest first. Requests without a request_id are skipped,
// and a request (or subagent) repeated across envelopes is counted once (first
// in-window occurrence wins). Malformed lines are skipped and lines have no
// length cap (raw tool payloads can be very large). Days are bucketed in loc.
// days must be >= 1.
func aggregateCostHistory(r io.Reader, now time.Time, days int, loc *time.Location) ([]costDay, error) {
	if days < 1 {
		return nil, fmt.Errorf("days must be at least 1, got %d", days)
	}
	nowLocal := now.In(loc)
	start := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	byDay := map[string]*costDay{}
	seen := map[string]bool{} // in-window request ids and subagent ids only
	br := bufio.NewReader(r)
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 && (bytes.Contains(line, costEnrichmentKey) || bytes.Contains(line, subagentEnrichmentKey)) {
			aggregateLine(line, start, nowLocal, loc, seen, byDay)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}

	out := make([]costDay, 0, len(byDay))
	for _, d := range byDay {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out, nil
}

// aggregateLine adds one envelope's in-window, not-yet-seen requests (and its
// subagent total, if any) to byDay.
func aggregateLine(line []byte, start, nowLocal time.Time, loc *time.Location, seen map[string]bool, byDay map[string]*costDay) {
	var env historyEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	inWindow := func(ts string) (string, bool) {
		t, ok := requestTime(ts, env.CapturedAt)
		if !ok {
			return "", false
		}
		t = t.In(loc)
		if t.Before(start) || t.After(nowLocal) {
			return "", false
		}
		return t.Format("2006-01-02"), true
	}
	dayFor := func(key string) *costDay {
		d := byDay[key]
		if d == nil {
			d = &costDay{Day: key, Currency: cost.Currency}
			byDay[key] = d
		}
		return d
	}

	if c := env.Enrichments.Cost; c != nil {
		for _, req := range c.Requests {
			if req.RequestID == "" || seen[req.RequestID] {
				continue
			}
			key, ok := inWindow(req.Timestamp)
			if !ok {
				continue
			}
			// Mark only once counted, so an unusable copy can't shadow a good one.
			seen[req.RequestID] = true
			d := dayFor(key)
			d.Requests++
			d.addTokens(req.Tokens)
			if req.ModelPriced {
				d.CostTotal += req.USD.Total
			} else {
				d.addUnpriced(req.Model, 1)
			}
		}
	}

	sa := env.Enrichments.Subagent
	if sa == nil || sa.Phase != "stop" || sa.AgentID == "" || sa.Tokens == nil {
		return
	}
	seenKey := "subagent:" + sa.AgentID
	if seen[seenKey] {
		return
	}
	key, ok := inWindow("")
	if !ok {
		return
	}
	seen[seenKey] = true
	d := dayFor(key)
	d.Requests += sa.Requests
	d.addTokens(*sa.Tokens)
	usd := 0.0
	if sa.USD != nil {
		usd = sa.USD.Total
	}
	d.CostTotal += usd
	d.SubagentCost += usd
	switch {
	case sa.ModelPriced != nil:
		if !*sa.ModelPriced {
			model := sa.UnpricedModel
			if model == "" {
				model = sa.Model
			}
			d.addUnpriced(model, sa.UnpricedRequests)
		}
	case usd == 0 && tokenSum(*sa.Tokens) > 0:
		// Recorded before the enrichment carried model_priced: tokens at $0
		// mean the model was missing from the rate table.
		d.addUnpriced(sa.Model, sa.Requests)
	}
}

func (d *costDay) addTokens(t cost.Tokens) {
	d.Input += t.Input
	d.Output += t.Output
	d.CacheRead += t.CacheRead
	d.CacheWrite += t.CacheWrite
}

// addUnpriced records n requests (at least one) on a model missing from the
// rate table.
func (d *costDay) addUnpriced(model string, n int) {
	if n < 1 {
		n = 1
	}
	if model == "" {
		model = "unknown"
	}
	d.Unpriced += n
	if d.UnpricedModels == nil {
		d.UnpricedModels = map[string]int{}
	}
	d.UnpricedModels[model] += n
}

func tokenSum(t cost.Tokens) int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite
}

// mergeModelCounts sums per-day unpriced model counts.
func mergeModelCounts(days []costDay) map[string]int {
	out := map[string]int{}
	for _, d := range days {
		for m, n := range d.UnpricedModels {
			out[m] += n
		}
	}
	return out
}

// formatModelCounts renders {"m1": 3, "m2": 1} as "m1 x3, m2 x1", sorted by
// count then name so the output is stable.
func formatModelCounts(m map[string]int) string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if m[names[i]] != m[names[j]] {
			return m[names[i]] > m[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s x%d", name, m[name])
	}
	return strings.Join(parts, ", ")
}

// loadCostHistory aggregates the last n days of priced requests from the
// local event log. A missing log is an empty history, not an error.
func loadCostHistory(n int) ([]costDay, error) {
	f, err := os.Open(eventlog.EventsJSONLPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	defer func() { _ = f.Close() }()
	days, err := aggregateCostHistory(f, time.Now(), n, time.Local)
	if err != nil {
		return nil, fmt.Errorf("read event log: %w", err)
	}
	return days, nil
}

func runCostHistory(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	if costDays < 1 {
		return fmt.Errorf("--days must be at least 1, got %d", costDays)
	}

	days, err := loadCostHistory(costDays)
	if err != nil {
		return err
	}

	if costJSON {
		if days == nil {
			days = []costDay{}
		}
		data, err := json.Marshal(days)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, string(data))
		return nil
	}

	// events.jsonl only guarantees the retention window; older records are
	// trimmed once the log passes its size ceiling, so a longer window may be
	// partial. Say so rather than presenting a short total as complete.
	if retention := client.LoadConfig().RetentionDays(); retention > 0 && costDays > retention {
		defer func() {
			_, _ = fmt.Fprintf(out, "Note: local history is kept for at least %d days; days before that may be incomplete.\n", retention)
		}()
	}

	if len(days) == 0 {
		_, _ = fmt.Fprintf(out, "No priced AI requests in the last %d day(s).\n", costDays)
		_, _ = fmt.Fprintln(out, "Cost is recorded by the Claude Code and Cursor hooks; `promptconduit status` shows which are installed.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "%-12s %12s %10s %12s %12s %14s\n", "DAY", "COST (USD)", "REQUESTS", "INPUT", "OUTPUT", "CACHE READ")
	var total, subagent float64
	unpriced := 0
	for _, d := range days {
		_, _ = fmt.Fprintf(out, "%-12s %12.4f %10d %12d %12d %14d\n", d.Day, d.CostTotal, d.Requests, d.Input, d.Output, d.CacheRead)
		total += d.CostTotal
		subagent += d.SubagentCost
		unpriced += d.Unpriced
	}
	_, _ = fmt.Fprintf(out, "%-12s %12.4f\n", "TOTAL", total)
	if subagent > 0 {
		_, _ = fmt.Fprintf(out, "Includes $%.4f of subagent spend.\n", subagent)
	}
	if unpriced > 0 {
		_, _ = fmt.Fprintf(out, "%d request(s) used a model not in the rate table (%s): tokens are counted, cost is not.\n",
			unpriced, formatModelCounts(mergeModelCounts(days)))
	}
	return nil
}
