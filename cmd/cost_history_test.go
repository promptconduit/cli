package cmd

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// costLine builds one events.jsonl envelope carrying a cost enrichment.
func costLine(capturedAt string, reqs ...string) string {
	return `{"schema":2,"captured_at":"` + capturedAt + `","enrichments":{"cost":{"requests":[` + strings.Join(reqs, ",") + `]}}}`
}

func costReq(id, ts string, priced bool, usd float64, in, out, cacheRead int64) string {
	p := "false"
	if priced {
		p = "true"
	}
	return `{"request_id":"` + id + `","ts":"` + ts + `","model_priced":` + p +
		`,"tokens":{"input":` + itoa(in) + `,"output":` + itoa(out) + `,"cache_read":` + itoa(cacheRead) + `,"cache_write":0}` +
		`,"usd":{"total":` + ftoa(usd) + `}}`
}

func itoa(n int64) string   { return strconv.FormatInt(n, 10) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func TestAggregateCostHistory(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 8, 15, 0, 0, 0, loc)

	log := strings.Join([]string{
		costLine("2026-10-08T10:00:00Z",
			costReq("r1", "2026-10-08T09:59:00Z", true, 1.25, 100, 50, 1000),
			costReq("r2", "2026-10-08T10:00:00Z", true, 0.75, 10, 5, 0),
		),
		// r1 again in a later envelope: counted once.
		costLine("2026-10-08T11:00:00Z", costReq("r1", "2026-10-08T09:59:00Z", true, 1.25, 100, 50, 1000)),
		// Yesterday, plus an unpriced model.
		costLine("2026-10-07T12:00:00Z",
			costReq("r3", "2026-10-07T12:00:00Z", true, 2.00, 200, 20, 0),
			costReq("r4", "2026-10-07T12:01:00Z", false, 0, 30, 3, 0),
		),
		// No ts on the request: falls back to captured_at.
		costLine("2026-10-07T13:00:00Z", costReq("r5", "", true, 0.50, 1, 1, 0)),
		// Outside a 2-day window.
		costLine("2026-10-01T12:00:00Z", costReq("r6", "2026-10-01T12:00:00Z", true, 9.99, 1, 1, 0)),
		// Future timestamp (clock skew): excluded.
		costLine("2026-10-09T12:00:00Z", costReq("r7", "2026-10-09T12:00:00Z", true, 9.99, 1, 1, 0)),
		// Junk and envelopes without a cost enrichment are skipped.
		`not json`,
		`{"schema":2,"captured_at":"2026-10-08T10:00:00Z","enrichments":{"env":{}}}`,
	}, "\n")

	got, err := aggregateCostHistory(strings.NewReader(log), now, 2, loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 days, got %d: %+v", len(got), got)
	}

	today, yesterday := got[0], got[1]
	if today.Day != "2026-10-08" || yesterday.Day != "2026-10-07" {
		t.Fatalf("days out of order or wrong: %q, %q", today.Day, yesterday.Day)
	}
	if today.Requests != 2 || !approx(today.CostTotal, 2.00) || today.Input != 110 || today.Output != 55 || today.CacheRead != 1000 {
		t.Errorf("today: %+v", today)
	}
	if yesterday.Requests != 3 || !approx(yesterday.CostTotal, 2.50) || yesterday.Unpriced != 1 || yesterday.Input != 231 {
		t.Errorf("yesterday: %+v", yesterday)
	}
}

func TestAggregateCostHistoryBucketsInLocalTime(t *testing.T) {
	// 02:00 UTC on Oct 8 is still Oct 7 in New York.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable:", err)
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, ny)
	log := costLine("2026-10-08T02:00:00Z", costReq("r1", "2026-10-08T02:00:00Z", true, 1, 1, 1, 0))

	got, err := aggregateCostHistory(strings.NewReader(log), now, 7, ny)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Day != "2026-10-07" {
		t.Fatalf("want one bucket on 2026-10-07, got %+v", got)
	}
}

func TestAggregateCostHistoryEmpty(t *testing.T) {
	got, err := aggregateCostHistory(strings.NewReader(""), time.Now(), 7, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no days, got %+v", got)
	}
}

func TestAggregateCostHistoryReviewEdgeCases(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 8, 15, 0, 0, 0, loc)

	// A line over bufio.Scanner's old 16 MiB cap must not abort the read.
	huge := `{"schema":2,"captured_at":"2026-10-08T09:00:00Z","raw_event":{"tool_response":"` +
		strings.Repeat("x", 17*1024*1024) + `"}}`

	log := strings.Join([]string{
		huge,
		// r1: first copy has an unparseable ts AND captured_at, so it can't be
		// counted; it must not shadow the later valid copy.
		`{"schema":2,"captured_at":"garbage","enrichments":{"cost":{"requests":[` +
			costReq("r1", "also-garbage", true, 1.00, 10, 1, 0) + `]}}}`,
		costLine("2026-10-08T10:00:00Z", costReq("r1", "2026-10-08T10:00:00Z", true, 1.00, 10, 1, 0)),
		// r2: unparseable ts falls back to a valid captured_at.
		costLine("2026-10-08T11:00:00Z", costReq("r2", "Oct 8 11:00", true, 2.00, 20, 2, 0)),
		// No request_id: skipped, like the extension's reader.
		costLine("2026-10-08T12:00:00Z", costReq("", "2026-10-08T12:00:00Z", true, 9.99, 1, 1, 0)),
	}, "\n")

	got, err := aggregateCostHistory(strings.NewReader(log), now, 1, loc)
	if err != nil {
		t.Fatalf("oversized line should not fail the read: %v", err)
	}
	if len(got) != 1 || got[0].Requests != 2 || !approx(got[0].CostTotal, 3.00) {
		t.Fatalf("want r1 + r2 only ($3.00, 2 requests), got %+v", got)
	}
}

func TestAggregateCostHistoryRejectsBadDays(t *testing.T) {
	if _, err := aggregateCostHistory(strings.NewReader(""), time.Now(), 0, time.UTC); err == nil {
		t.Fatal("days=0 should be an error")
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
