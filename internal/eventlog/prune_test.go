package eventlog

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// fixedNow pins nowUTC for a test so retention cutoffs are deterministic.
func fixedNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := nowUTC
	nowUTC = func() time.Time { return at }
	t.Cleanup(func() { nowUTC = prev })
}

// writeEvents writes envelope-shaped lines (captured_at) to events.jsonl.
func writeEvents(t *testing.T, tss ...string) {
	t.Helper()
	var b strings.Builder
	for i, ts := range tss {
		fmt.Fprintf(&b, `{"schema":2,"event_id":"e%d","captured_at":"%s"}`+"\n", i, ts)
	}
	if err := os.WriteFile(EventsJSONLPath(), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readEventLines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(EventsJSONLPath())
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func iso(daysAgo int, base time.Time) string {
	return base.Add(-time.Duration(daysAgo) * 24 * time.Hour).Format(time.RFC3339)
}

func TestPruneExpiredRemovesOldKeepsYoung(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// 3 within a 30-day window, 2 older than it.
	writeEvents(t, iso(1, now), iso(10, now), iso(29, now), iso(31, now), iso(90, now))

	removed := PruneExpired(30)
	if removed != 2 {
		t.Fatalf("expected 2 expired records removed, got %d", removed)
	}
	lines := readEventLines(t)
	if len(lines) != 3 {
		t.Fatalf("expected 3 retained lines, got %d: %v", len(lines), lines)
	}
	// The two oldest (e3=31d, e4=90d) must be gone; e0/e1/e2 kept in order.
	for i, want := range []string{"e0", "e1", "e2"} {
		if !strings.Contains(lines[i], `"`+want+`"`) {
			t.Errorf("line %d = %q, expected to contain %q", i, lines[i], want)
		}
	}
}

func TestPruneUnderCeilingKeepsEverything(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// Even records older than the window survive the automatic ceiling policy
	// while the file is under the ceiling: the floor is a minimum, not a cap.
	writeEvents(t, iso(1, now), iso(100, now), iso(400, now))

	removed := Prune(30) // ceiling policy (EventsCeiling = 500MB, far above this)
	if removed != 0 {
		t.Fatalf("expected nothing pruned under the ceiling, got %d removed", removed)
	}
	if got := len(readEventLines(t)); got != 3 {
		t.Fatalf("expected all 3 lines retained, got %d", got)
	}
}

func TestPruneCeilingTrimsOldestExpiredFirst(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// Chronological order, as a real append-only log accumulates (oldest first):
	// three expired (60d, 50d, 40d) then two young (2d, 1d). Padded bodies let us
	// set a small ceiling and force trimming.
	pad := strings.Repeat("x", 100)
	tss := []string{iso(60, now), iso(50, now), iso(40, now), iso(2, now), iso(1, now)}
	var b strings.Builder
	for i, ts := range tss {
		fmt.Fprintf(&b, `{"event_id":"e%d","captured_at":"%s","pad":"%s"}`+"\n", i, ts, pad)
	}
	if err := os.WriteFile(EventsJSONLPath(), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(EventsJSONLPath())
	// Ceiling that leaves room for ~4 of the 5 lines: forces dropping 1 (the
	// oldest = e0 at 60d), but never the young ones.
	ceiling := info.Size() - 120
	cutoff := now.Add(-30 * 24 * time.Hour)

	removed := pruneFile(EventsJSONLPath(), ceiling, cutoff, envelopeCapturedAt)
	if removed != 1 {
		t.Fatalf("expected exactly 1 record trimmed to meet the ceiling, got %d", removed)
	}
	lines := readEventLines(t)
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines after trim, got %d", len(lines))
	}
	// e0 (60d, oldest) dropped first; e3/e4 (young) and e1/e2 (next-oldest
	// expired) kept.
	if strings.Contains(strings.Join(lines, "\n"), `"e0"`) {
		t.Errorf("oldest expired record e0 should have been trimmed first")
	}
	for _, mustKeep := range []string{"e3", "e4"} {
		if !strings.Contains(strings.Join(lines, "\n"), `"`+mustKeep+`"`) {
			t.Errorf("young record %s must never be trimmed", mustKeep)
		}
	}
}

func TestPruneNeverDropsYoungEvenOverCeiling(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// All records are within the window; even a tiny ceiling can't drop them.
	pad := strings.Repeat("y", 100)
	var b strings.Builder
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, `{"event_id":"y%d","captured_at":"%s","pad":"%s"}`+"\n", i, iso(i, now), pad)
	}
	if err := os.WriteFile(EventsJSONLPath(), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-30 * 24 * time.Hour)

	removed := pruneFile(EventsJSONLPath(), 1 /* absurdly small ceiling */, cutoff, envelopeCapturedAt)
	if removed != 0 {
		t.Fatalf("young records must survive any ceiling; got %d removed", removed)
	}
	if got := len(readEventLines(t)); got != 4 {
		t.Fatalf("expected all 4 young lines retained, got %d", got)
	}
}

func TestPruneKeepsUnparseableTimestampLines(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// A line with no captured_at must be treated as young (kept), never dropped.
	lines := []string{
		`{"event_id":"old","captured_at":"` + iso(90, now) + `"}`,
		`{"event_id":"noTs"}`,
		`{"event_id":"young","captured_at":"` + iso(1, now) + `"}`,
	}
	if err := os.WriteFile(EventsJSONLPath(), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed := PruneExpired(30)
	if removed != 1 {
		t.Fatalf("expected only the 90-day record removed, got %d", removed)
	}
	joined := strings.Join(readEventLines(t), "\n")
	if !strings.Contains(joined, `"noTs"`) {
		t.Errorf("record with no timestamp must be preserved, not dropped")
	}
	if strings.Contains(joined, `"old"`) {
		t.Errorf("90-day-old record should have been removed")
	}
}

func TestPruneKeepForeverIsNoOp(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)
	writeEvents(t, iso(1, now), iso(1000, now))

	if removed := Prune(0); removed != 0 {
		t.Fatalf("retention 0 must never prune, got %d", removed)
	}
	if removed := PruneExpired(0); removed != 0 {
		t.Fatalf("retention 0 must never prune, got %d", removed)
	}
	if removed := Prune(-1); removed != 0 {
		t.Fatalf("negative retention must never prune, got %d", removed)
	}
	if got := len(readEventLines(t)); got != 2 {
		t.Fatalf("expected both lines retained, got %d", got)
	}
}

func TestStatsCountsExpired(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)
	writeEvents(t, iso(1, now), iso(5, now), iso(40, now), iso(90, now))

	s := Stats(30)
	if s.EventsTotal != 4 {
		t.Errorf("EventsTotal = %d, want 4", s.EventsTotal)
	}
	if s.EventsExpired != 2 {
		t.Errorf("EventsExpired = %d, want 2", s.EventsExpired)
	}
	if s.EventsBytes == 0 {
		t.Errorf("EventsBytes should be non-zero")
	}

	// Keep-forever: nothing is expired, but totals still report.
	sf := Stats(0)
	if sf.EventsExpired != 0 || sf.EventsTotal != 4 {
		t.Errorf("keep-forever stats = %+v, want total 4 / expired 0", sf)
	}
}

func TestPrunePreservesConcurrentAppendTail(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// Write expired + young records with padding so we can force a ceiling trim.
	pad := strings.Repeat("z", 100)
	tss := []string{iso(60, now), iso(50, now), iso(1, now)}
	var b strings.Builder
	for i, ts := range tss {
		fmt.Fprintf(&b, `{"event_id":"e%d","captured_at":"%s","pad":"%s"}`+"\n", i, ts, pad)
	}
	if err := os.WriteFile(EventsJSONLPath(), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(EventsJSONLPath())
	startSize := info.Size()

	// Simulate a concurrent append landing after we captured startSize by
	// appending a new line, then invoking pruneFile with the stat it would have
	// taken. We approximate by appending before the call but relying on pruneFile
	// re-statting: the tail-copy must carry the appended line over.
	appended := fmt.Sprintf(`{"event_id":"late","captured_at":"%s"}`+"\n", iso(0, now))
	f, err := os.OpenFile(EventsJSONLPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(appended)
	_ = f.Close()

	// Ceiling forces trimming the oldest expired (e0). startSize-based scan means
	// the "late" line (appended after startSize) is carried via the tail copy.
	ceiling := startSize - 120
	cutoff := now.Add(-30 * 24 * time.Hour)
	removed := pruneFileFrom(EventsJSONLPath(), ceiling, cutoff, envelopeCapturedAt, startSize)
	if removed != 1 {
		t.Fatalf("expected 1 expired trimmed, got %d", removed)
	}
	joined := strings.Join(readEventLines(t), "\n")
	if !strings.Contains(joined, `"late"`) {
		t.Errorf("concurrently-appended line must be preserved, got:\n%s", joined)
	}
	if strings.Contains(joined, `"e0"`) {
		t.Errorf("oldest expired e0 should have been trimmed")
	}
}
