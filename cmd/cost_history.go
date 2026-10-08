package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/promptconduit/cli/internal/cost"
	"github.com/promptconduit/cli/internal/eventlog"
	"github.com/spf13/cobra"
)

// costDay is one calendar day (local time) of priced requests.
type costDay struct {
	Day        string  `json:"day"`
	CostTotal  float64 `json:"cost_total"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Requests   int     `json:"requests"`
	Unpriced   int     `json:"unpriced"`
	Currency   string  `json:"currency"`
}

// historyEnvelope is the slice of a v2 envelope that cost history reads: the
// `cost` enrichment's requests, plus captured_at as a timestamp fallback.
type historyEnvelope struct {
	CapturedAt  string `json:"captured_at"`
	Enrichments struct {
		Cost *struct {
			Requests []struct {
				RequestID   string      `json:"request_id"`
				Timestamp   string      `json:"ts"`
				ModelPriced bool        `json:"model_priced"`
				Tokens      cost.Tokens `json:"tokens"`
				USD         cost.Cost   `json:"usd"`
			} `json:"requests"`
		} `json:"cost"`
	} `json:"enrichments"`
}

// aggregateCostHistory sums the `cost` enrichment in an events.jsonl stream into
// per-day totals for the last `days` calendar days (today included), newest
// first. A request repeated across envelopes is counted once by request_id
// (first seen wins), matching the editor extension's month-spend reader.
// Malformed lines are skipped. Days are bucketed in loc.
func aggregateCostHistory(r io.Reader, now time.Time, days int, loc *time.Location) ([]costDay, error) {
	if days < 1 {
		days = 1
	}
	nowLocal := now.In(loc)
	start := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	byDay := map[string]*costDay{}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var env historyEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil || env.Enrichments.Cost == nil {
			continue
		}
		for _, req := range env.Enrichments.Cost.Requests {
			if req.RequestID != "" {
				if seen[req.RequestID] {
					continue
				}
				seen[req.RequestID] = true
			}
			stamp := req.Timestamp
			if stamp == "" {
				stamp = env.CapturedAt
			}
			ts, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				continue
			}
			ts = ts.In(loc)
			if ts.Before(start) || ts.After(nowLocal) {
				continue
			}
			key := ts.Format("2006-01-02")
			d := byDay[key]
			if d == nil {
				d = &costDay{Day: key, Currency: cost.Currency}
				byDay[key] = d
			}
			d.Requests++
			d.Input += req.Tokens.Input
			d.Output += req.Tokens.Output
			d.CacheRead += req.Tokens.CacheRead
			d.CacheWrite += req.Tokens.CacheWrite
			if req.ModelPriced {
				d.CostTotal += req.USD.Total
			} else {
				d.Unpriced++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	out := make([]costDay, 0, len(byDay))
	for _, d := range byDay {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out, nil
}

func runCostHistory(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()

	var days []costDay
	f, err := os.Open(eventlog.EventsJSONLPath())
	switch {
	case os.IsNotExist(err):
		// No events yet: an empty history, not an error.
	case err != nil:
		return fmt.Errorf("read event log: %w", err)
	default:
		defer func() { _ = f.Close() }()
		if days, err = aggregateCostHistory(f, time.Now(), costDays, time.Local); err != nil {
			return fmt.Errorf("read event log: %w", err)
		}
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

	if len(days) == 0 {
		_, _ = fmt.Fprintf(out, "No priced AI requests in the last %d day(s).\n", costDays)
		_, _ = fmt.Fprintln(out, "Cost is recorded from Claude Code and Cursor hooks; run `promptconduit install claude-code` or `promptconduit install cursor`, then use the assistant.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "%-12s %12s %10s %12s %12s %14s\n", "DAY", "COST (USD)", "REQUESTS", "INPUT", "OUTPUT", "CACHE READ")
	var total float64
	unpriced := 0
	for _, d := range days {
		_, _ = fmt.Fprintf(out, "%-12s %12.4f %10d %12d %12d %14d\n", d.Day, d.CostTotal, d.Requests, d.Input, d.Output, d.CacheRead)
		total += d.CostTotal
		unpriced += d.Unpriced
	}
	_, _ = fmt.Fprintf(out, "%-12s %12.4f\n", "TOTAL", total)
	if unpriced > 0 {
		_, _ = fmt.Fprintf(out, "%d request(s) used a model not in the rate table: tokens are counted, cost is not.\n", unpriced)
	}
	return nil
}
