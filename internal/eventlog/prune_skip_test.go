package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPruneSkipsFileWhoseOldestRecordIsInsideWindow(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// Chronological log, all inside the 30-day window.
	writeEvents(t, iso(20, now), iso(10, now), iso(1, now))
	before, _ := os.ReadFile(EventsJSONLPath())

	if removed := Prune(30); removed != 0 {
		t.Fatalf("nothing is expired, got %d removed", removed)
	}
	after, _ := os.ReadFile(EventsJSONLPath())
	if !bytes.Equal(before, after) {
		t.Fatalf("log must be untouched")
	}
	st := loadPruneStamp()
	until, ok := st.until(EventsJSONLPath())
	want := now.Add(-20 * 24 * time.Hour).Add(30 * 24 * time.Hour)
	if !ok || !until.Equal(want) {
		t.Fatalf("stamp until = %v ok=%v, want %v (oldest record + window)", until, ok, want)
	}
}

func TestPruneSkipDecisionReadsOnlyTheHead(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	// Out-of-order on purpose: the first record is young, so the automatic pass
	// must decide from the head alone and never look at (or drop) the rest.
	writeEvents(t, iso(1, now), iso(90, now))
	if removed := Prune(30); removed != 0 {
		t.Fatalf("automatic pass must skip on a young head, got %d removed", removed)
	}
	if got := len(readEventLines(t)); got != 2 {
		t.Fatalf("expected both lines kept, got %d", got)
	}
}

func TestPruneRecordsUntilAfterTrimming(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)
	writeEvents(t, iso(90, now), iso(5, now))

	// Force the trim path through PruneExpired semantics via a tiny ceiling:
	// pruneFile is what Prune calls once the head is expired.
	cutoff := now.Add(-30 * 24 * time.Hour)
	if removed := pruneFile(EventsJSONLPath(), 0, 0, cutoff, envelopeCapturedAt); removed != 1 {
		t.Fatalf("expected the 90-day record removed, got %d", removed)
	}
	oldest, ok := oldestRecordTime(EventsJSONLPath(), envelopeCapturedAt)
	if !ok || !oldest.Equal(now.Add(-5*24*time.Hour)) {
		t.Fatalf("oldest after trim = %v ok=%v", oldest, ok)
	}
}

func TestNeedsPruneHonoursStampUntil(t *testing.T) {
	withTempDir(t)
	now := time.Now().UTC()
	fixedNow(t, now)

	f, err := os.Create(EventsJSONLPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(EventsCeiling + 1); err != nil { // sparse: no real disk
		t.Fatal(err)
	}
	_ = f.Close()

	savePruneStamp(&pruneStamp{Until: map[string]time.Time{"events.jsonl": now.Add(24 * time.Hour)}})
	old := time.Now().Add(-2 * pruneMinInterval)
	_ = os.Chtimes(pruneStampPath(), old, old) // throttle lapsed
	if NeedsPrune(30) {
		t.Fatalf("oldest record not yet expired: no pass is due")
	}

	savePruneStamp(&pruneStamp{Until: map[string]time.Time{"events.jsonl": now.Add(-time.Minute)}})
	_ = os.Chtimes(pruneStampPath(), old, old)
	if !NeedsPrune(30) {
		t.Fatalf("oldest record has expired: a pass is due")
	}

	// A legacy empty stamp never suppresses a pass.
	if err := os.WriteFile(pruneStampPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(pruneStampPath(), old, old)
	if !NeedsPrune(30) {
		t.Fatalf("empty stamp must not suppress a pass")
	}
}

func TestPruneHandlesLinesLargerThanOldScannerCap(t *testing.T) {
	withTempDir(t)
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	fixedNow(t, now)

	big := strings.Repeat("b", 9*1024*1024) // > the old 8 MB bufio.Scanner cap
	oldBig := fmt.Sprintf(`{"event_id":"oldbig","captured_at":"%s","pad":"%s"}`, iso(90, now), big)
	youngBig := fmt.Sprintf(`{"event_id":"youngbig","captured_at":"%s","pad":"%s"}`, iso(1, now), big)
	small := fmt.Sprintf(`{"event_id":"small","captured_at":"%s"}`, iso(2, now))
	content := oldBig + "\n" + small + "\n" + youngBig + "\n"
	if err := os.WriteFile(EventsJSONLPath(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := Stats(30)
	if s.EventsTotal != 3 || s.EventsExpired != 1 {
		t.Fatalf("stats = %+v, want 3 total / 1 expired", s)
	}
	removed, err := PruneExpired(30)
	if err != nil || removed != 1 {
		t.Fatalf("expected the oversized expired record removed, got %d err=%v", removed, err)
	}
	got, _ := os.ReadFile(EventsJSONLPath())
	if want := small + "\n" + youngBig + "\n"; string(got) != want {
		t.Fatalf("survivors not preserved byte-for-byte (len got %d want %d)", len(got), len(want))
	}
}

func TestScanLinesReportsSizesAndUnterminatedTail(t *testing.T) {
	long := strings.Repeat("L", lineHeadMax*3)
	in := "a\n\n" + long + "\nlast"
	type rec struct {
		head string
		size int64
		term bool
	}
	var got []rec
	if err := scanLines(strings.NewReader(in), func(h []byte, n int64, term bool) bool {
		got = append(got, rec{string(h), n, term})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	want := []rec{
		{"a", 2, true},
		{"", 1, true},
		{long[:lineHeadMax], int64(len(long) + 1), true},
		{"last", 4, false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	var total int64
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = {%d bytes head, %d, %v}, want {%d bytes head, %d, %v}",
				i, len(got[i].head), got[i].size, got[i].term, len(want[i].head), want[i].size, want[i].term)
		}
		total += got[i].size
	}
	if total != int64(len(in)) {
		t.Errorf("sizes sum to %d, want %d", total, len(in))
	}
}

func TestAppendWaitsWhileRenameLockHeld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	withTempDir(t)
	release, ok := lockAppendsForRename(EventsJSONLPath())
	if !ok {
		t.Fatal("could not take rename lock")
	}
	var done atomic.Bool
	go func() {
		RecordCapture([]byte(`{"schema":2,"event_id":"w"}`))
		done.Store(true)
	}()
	time.Sleep(100 * time.Millisecond)
	if done.Load() {
		t.Fatal("append must wait while a rename holds the exclusive lock")
	}
	release()
	deadline := time.Now().Add(2 * time.Second)
	for !done.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(readEventLines(t)); got != 1 {
		t.Fatalf("append must land once the lock is released, got %d lines", got)
	}
}

func TestAppendNeverDroppedWhenLockStaysHeld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	withTempDir(t)
	release, _ := lockAppendsForRename(EventsJSONLPath())
	defer release()
	start := time.Now()
	RecordCapture([]byte(`{"schema":2,"event_id":"w"}`))
	if elapsed := time.Since(start); elapsed > appendLockWait+time.Second {
		t.Fatalf("append blocked %v, want bounded by ~%v", elapsed, appendLockWait)
	}
	if got := len(readEventLines(t)); got != 1 {
		t.Fatalf("append must still happen after the bounded wait, got %d lines", got)
	}
}

// TestRewriteDoesNotLoseConcurrentAppends races real appender PROCESSES
// against repeated LockedRewrite renames. Without the append lock a line
// written into the old inode between the final carry and the rename is lost.
func TestRewriteDoesNotLoseConcurrentAppends(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns worker processes; skipped under -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("advisory locks are a no-op on windows")
	}
	dir := withTempDir(t)
	if err := os.WriteFile(EventsJSONLPath(), []byte(`{"schema":2,"event_id":"seed","n":0}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const workers, writes = 6, 400
	start := time.Now().Add(500 * time.Millisecond)
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestEventLogAppendWorker$")
			cmd.Env = append(os.Environ(),
				childDirEnv+"="+dir,
				childIDEnv+"="+strconv.Itoa(i),
				childStartEnv+"="+strconv.FormatInt(start.UnixNano(), 10),
				childWritesEnv+"="+strconv.Itoa(writes),
				childPadEnv+"="+strconv.Itoa(200),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = fmt.Errorf("%v: %s", err, out)
			}
		}(i)
	}

	stop := make(chan struct{})
	stopped := make(chan struct{})
	var rewrites atomic.Int64
	go func() {
		defer close(stopped)
		n := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			n++
			_, _ = LockedRewrite(nil, func(line []byte) ([]byte, bool) {
				if !bytes.Contains(line, []byte(`"seed"`)) {
					return nil, false
				}
				return []byte(fmt.Sprintf(`{"schema":2,"event_id":"seed","n":%d}`, n)), true
			})
			rewrites.Add(1)
		}
	}()
	wg.Wait()
	close(stop)
	<-stopped
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}

	f, err := os.Open(EventsJSONLPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	seen := map[string]bool{}
	for sc.Scan() {
		var v struct {
			ID string `json:"event_id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("corrupt line: %v", err)
		}
		seen[v.ID] = true
	}
	t.Logf("rewrites during the race: %d", rewrites.Load())
	if want := workers*writes + 1; len(seen) != want {
		t.Fatalf("distinct records = %d, want %d (appends lost across a rename)", len(seen), want)
	}
}
