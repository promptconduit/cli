package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/promptconduit/cli/internal/client"
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

// costEnrichmentKey cheaply pre-filters event-log lines: most envelopes carry
// no cost enrichment, and full JSON decoding of large tool payloads is wasted.
var costEnrichmentKey = []byte(`"cost":`)

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

// aggregateCostHistory sums the `cost` enrichment in an events.jsonl stream into
// per-day totals for the last `days` calendar days (today included), newest
// first. It mirrors the editor extension's month-spend reader: requests without
// a request_id are skipped, and a request repeated across envelopes is counted
// once (first in-window occurrence wins). Malformed lines are skipped and lines
// have no length cap (raw tool payloads can be very large). Days are bucketed
// in loc. days must be >= 1.
func aggregateCostHistory(r io.Reader, now time.Time, days int, loc *time.Location) ([]costDay, error) {
	if days < 1 {
		return nil, fmt.Errorf("days must be at least 1, got %d", days)
	}
	nowLocal := now.In(loc)
	start := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	byDay := map[string]*costDay{}
	seen := map[string]bool{} // in-window request ids only
	br := bufio.NewReader(r)
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, costEnrichmentKey) {
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

// aggregateLine adds one envelope's in-window, not-yet-seen requests to byDay.
func aggregateLine(line []byte, start, nowLocal time.Time, loc *time.Location, seen map[string]bool, byDay map[string]*costDay) {
	var env historyEnvelope
	if err := json.Unmarshal(line, &env); err != nil || env.Enrichments.Cost == nil {
		return
	}
	for _, req := range env.Enrichments.Cost.Requests {
		if req.RequestID == "" || seen[req.RequestID] {
			continue
		}
		ts, ok := requestTime(req.Timestamp, env.CapturedAt)
		if !ok {
			continue
		}
		ts = ts.In(loc)
		if ts.Before(start) || ts.After(nowLocal) {
			continue
		}
		// Mark only once counted, so an unusable copy can't shadow a good one.
		seen[req.RequestID] = true
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

func runCostHistory(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	if costDays < 1 {
		return fmt.Errorf("--days must be at least 1, got %d", costDays)
	}

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
