package eventlog

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func spoolEnv(id string, capturedAt time.Time, pad int) []byte {
	return []byte(`{"schema":2,"event_id":"` + id + `","captured_at":"` + capturedAt.UTC().Format(time.RFC3339) +
		`","pad":"` + strings.Repeat("x", pad) + `"}`)
}

func deliverBatch(batch [][]byte) []ReplayResult {
	return make([]ReplayResult, len(batch)) // zero value is ReplayDelivered
}

func TestSpoolAppendCountAndPerTarget(t *testing.T) {
	useTempEventLog(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := AppendSpool(prod, spoolEnv(id, time.Now(), 0)); err != nil {
			t.Fatal(err)
		}
	}
	if got := SpoolCount(prod); got != 3 {
		t.Fatalf("SpoolCount = %d, want 3", got)
	}
	if got := SpoolCount("http://localhost:8787"); got != 0 {
		t.Fatalf("another target's spool = %d, want 0", got)
	}
	res := FlushSpool("http://localhost:8787", SpoolFlushLimits{MaxEvents: 10, MaxBytes: 1 << 20}, func([][]byte) []ReplayResult {
		t.Fatal("prod's spool must never be sent to another target")
		return nil
	})
	if res.Tried != 0 || SpoolCount(prod) != 3 {
		t.Fatalf("res=%+v prod=%d; want prod untouched", res, SpoolCount(prod))
	}
}

func TestAppendSpoolDisabledAndFull(t *testing.T) {
	useTempEventLog(t)
	SetEnabled(false)
	if err := AppendSpool(prod, spoolEnv("a", time.Now(), 0)); err != ErrSpoolDisabled {
		t.Fatalf("disabled: err = %v, want ErrSpoolDisabled", err)
	}
	SetEnabled(true)
	f, err := os.Create(SpoolPath(prod))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(spoolCeiling); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := AppendSpool(prod, spoolEnv("a", time.Now(), 0)); err != ErrSpoolFull {
		t.Fatalf("full: err = %v, want ErrSpoolFull", err)
	}
}

func TestSpoolConcurrentAppendInProcess(t *testing.T) {
	useTempEventLog(t)
	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := AppendSpool(prod, spoolEnv(fmt.Sprintf("w%d-%d", w, i), time.Now(), 6000)); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	data, err := os.ReadFile(SpoolPath(prod))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != writers*each {
		t.Fatalf("got %d lines, want %d", len(lines), writers*each)
	}
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line %d is not valid JSON (interleaved write)", i)
		}
	}
}

func TestFlushSpoolSuccessEmptiesSpool(t *testing.T) {
	useTempEventLog(t)
	for i := 0; i < 5; i++ {
		_ = AppendSpool(prod, spoolEnv(fmt.Sprint(i), time.Now(), 0))
	}
	var sent []string
	res := FlushSpool(prod, SpoolFlushLimits{MaxEvents: 100, MaxBytes: 1 << 20}, func(batch [][]byte) []ReplayResult {
		for _, b := range batch {
			sent = append(sent, idOf(b))
		}
		return deliverBatch(batch)
	})
	if res.Delivered != 5 || res.Batches != 1 || res.Remaining != 0 {
		t.Fatalf("res = %+v", res)
	}
	if strings.Join(sent, ",") != "0,1,2,3,4" {
		t.Fatalf("sent order %v", sent)
	}
	if SpoolCount(prod) != 0 {
		t.Fatal("spool should be empty")
	}
}

func TestFlushSpoolBatchCaps(t *testing.T) {
	useTempEventLog(t)
	for i := 0; i < 25; i++ {
		_ = AppendSpool(prod, spoolEnv(fmt.Sprint(i), time.Now(), 100))
	}
	// One oversized envelope must go alone.
	_ = AppendSpool(prod, spoolEnv("big", time.Now(), 5000))
	var sizes []int
	res := FlushSpool(prod, SpoolFlushLimits{MaxEvents: 10, MaxBytes: 2000}, func(batch [][]byte) []ReplayResult {
		total := 0
		for _, b := range batch {
			total += len(b)
		}
		if len(batch) > 10 {
			t.Errorf("batch of %d exceeds MaxEvents", len(batch))
		}
		if len(batch) > 1 && total > 2000 {
			t.Errorf("batch of %d bytes exceeds MaxBytes", total)
		}
		sizes = append(sizes, len(batch))
		return deliverBatch(batch)
	})
	if res.Delivered != 26 || res.Remaining != 0 {
		t.Fatalf("res = %+v", res)
	}
	if last := sizes[len(sizes)-1]; last != 1 {
		t.Fatalf("oversized envelope should be sent alone; batch sizes %v", sizes)
	}
}

func TestFlushSpoolPartialRejectionAndStop(t *testing.T) {
	useTempEventLog(t)
	for _, id := range []string{"ok1", "bad", "ok2", "poison", "ok3", "ok4"} {
		_ = AppendSpool(prod, spoolEnv(id, time.Now(), 0))
	}
	calls := 0
	res := FlushSpool(prod, SpoolFlushLimits{MaxEvents: 3, MaxBytes: 1 << 20}, func(batch [][]byte) []ReplayResult {
		calls++
		out := make([]ReplayResult, len(batch))
		for i, b := range batch {
			switch idOf(b) {
			case "bad":
				out[i] = ReplayRejected
			case "poison":
				out[i] = ReplaySkip
			}
		}
		if calls == 2 {
			out[len(out)-1] = ReplayStop // the server went away mid-pass
		}
		return out
	})
	if res.Delivered != 3 || res.Rejected != 1 || res.Skipped != 1 || !res.Stopped {
		t.Fatalf("res = %+v", res)
	}
	data, _ := os.ReadFile(SpoolPath(prod))
	var left []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		left = append(left, idOf([]byte(l)))
	}
	// ok4 was Stop (kept in place); poison rotated to the back.
	if strings.Join(left, ",") != "ok4,poison" {
		t.Fatalf("remaining = %v, want [ok4 poison]", left)
	}
}

func TestFlushSpoolAllStopKeepsEverything(t *testing.T) {
	useTempEventLog(t)
	for i := 0; i < 4; i++ {
		_ = AppendSpool(prod, spoolEnv(fmt.Sprint(i), time.Now(), 0))
	}
	before, _ := os.ReadFile(SpoolPath(prod))
	res := FlushSpool(prod, SpoolFlushLimits{MaxEvents: 100, MaxBytes: 1 << 20}, func(batch [][]byte) []ReplayResult {
		out := make([]ReplayResult, len(batch))
		for i := range out {
			out[i] = ReplayStop
		}
		return out
	})
	after, _ := os.ReadFile(SpoolPath(prod))
	if !res.Stopped || res.Remaining != 4 || string(before) != string(after) {
		t.Fatalf("a failed flush must leave the spool byte-identical; res=%+v", res)
	}
}

func TestFlushSpoolDropsExpired(t *testing.T) {
	useTempEventLog(t)
	_ = AppendSpool(prod, spoolEnv("old", time.Now().Add(-spoolMaxAge-time.Hour), 0))
	_ = AppendSpool(prod, spoolEnv("new", time.Now(), 0))
	var sent []string
	res := FlushSpool(prod, SpoolFlushLimits{MaxEvents: 100, MaxBytes: 1 << 20}, func(batch [][]byte) []ReplayResult {
		for _, b := range batch {
			sent = append(sent, idOf(b))
		}
		return deliverBatch(batch)
	})
	if res.Expired != 1 || res.Delivered != 1 || strings.Join(sent, ",") != "new" {
		t.Fatalf("res=%+v sent=%v", res, sent)
	}
	if LoadStatus().Dropped != 1 {
		t.Fatal("expired entry should count as dropped")
	}
}

func TestFlushSpoolMaxBatches(t *testing.T) {
	useTempEventLog(t)
	for i := 0; i < 10; i++ {
		_ = AppendSpool(prod, spoolEnv(fmt.Sprint(i), time.Now(), 0))
	}
	res := FlushSpool(prod, SpoolFlushLimits{MaxEvents: 3, MaxBytes: 1 << 20, MaxBatches: 2}, deliverBatch)
	if res.Batches != 2 || res.Delivered != 6 || res.Remaining != 4 {
		t.Fatalf("res = %+v", res)
	}
}

func TestFlushSpoolBusyAndStaleLock(t *testing.T) {
	useTempEventLog(t)
	_ = AppendSpool(prod, spoolEnv("a", time.Now(), 0))
	lock := spoolLockPath(prod)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !SpoolFlushing(prod) {
		t.Fatal("a fresh lock means a flusher is running")
	}
	res := FlushSpool(prod, SpoolFlushLimits{MaxEvents: 10, MaxBytes: 1 << 20}, func([][]byte) []ReplayResult {
		t.Fatal("must not send while another flusher holds the lock")
		return nil
	})
	if !res.Busy || res.Remaining != 1 {
		t.Fatalf("res = %+v, want Busy", res)
	}

	// A lock left by a killed flusher (older than SpoolLockStale) is broken.
	old := time.Now().Add(-SpoolLockStale - time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if SpoolFlushing(prod) {
		t.Fatal("a stale lock must not count as a running flusher")
	}
	res = FlushSpool(prod, SpoolFlushLimits{MaxEvents: 10, MaxBytes: 1 << 20}, deliverBatch)
	if res.Busy || res.Delivered != 1 {
		t.Fatalf("stale lock not recovered: res = %+v", res)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("lock should be released after the flush")
	}
}

func TestSpoolDue(t *testing.T) {
	useTempEventLog(t)
	if SpoolDue(prod, 3, time.Hour) {
		t.Fatal("empty spool is never due")
	}
	_ = AppendSpool(prod, spoolEnv("a", time.Now(), 0))
	if SpoolDue(prod, 3, time.Hour) {
		t.Fatal("1 fresh entry with minCount 3 is not due")
	}
	_ = AppendSpool(prod, spoolEnv("b", time.Now(), 0))
	_ = AppendSpool(prod, spoolEnv("c", time.Now(), 0))
	if !SpoolDue(prod, 3, time.Hour) {
		t.Fatal("minCount reached: due")
	}

	useTempEventLog(t)
	_ = AppendSpool(prod, spoolEnv("old", time.Now().Add(-10*time.Second), 0))
	if !SpoolDue(prod, 20, 5*time.Second) {
		t.Fatal("oldest entry past maxAge: due")
	}
	UpdateSpoolState(prod, func(st *SpoolState) { st.NextAttemptAt = time.Now().Add(time.Minute) })
	if SpoolDue(prod, 20, 5*time.Second) || !SpoolInBackoff(prod) {
		t.Fatal("in backoff: not due")
	}
}

func TestSpoolStateRoundTrip(t *testing.T) {
	useTempEventLog(t)
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	UpdateSpoolState(prod, func(st *SpoolState) {
		st.LastResult = "ok"
		st.BatchUnsupportedUntil = until
	})
	st := LoadSpoolState(prod)
	if st.LastResult != "ok" || !st.BatchUnsupportedUntil.Equal(until) {
		t.Fatalf("state = %+v", st)
	}
	if LoadSpoolState("http://other").LastResult != "" {
		t.Fatal("state is per target")
	}
}

// Cross-process: hook processes append while a flusher rewrites the spool.
// Every appended envelope must end up either delivered or still spooled —
// none lost, none corrupted.

const (
	spoolChildDirEnv    = "PROMPTCONDUIT_SPOOL_CHILD_DIR"
	spoolChildIDEnv     = "PROMPTCONDUIT_SPOOL_CHILD_ID"
	spoolChildWritesEnv = "PROMPTCONDUIT_SPOOL_CHILD_WRITES"
)

func TestSpoolAppendWorker(t *testing.T) {
	dir := os.Getenv(spoolChildDirEnv)
	if dir == "" {
		t.Skip("not running as a spool append worker")
	}
	writes, _ := strconv.Atoi(os.Getenv(spoolChildWritesEnv))
	SetDirForTest(dir)
	SetEnabled(true)
	id := os.Getenv(spoolChildIDEnv)
	for i := 0; i < writes; i++ {
		if err := AppendSpool(prod, spoolEnv(fmt.Sprintf("%s-%d", id, i), time.Now(), 3000)); err != nil {
			t.Fatalf("append: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSpoolAppendDuringFlushAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns worker processes; skipped under -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("append/rename coordination relies on flock (no-op on Windows)")
	}
	useTempEventLog(t)
	dir := Dir()
	const workers, writes = 4, 60

	var procs []*exec.Cmd
	for w := 0; w < workers; w++ {
		c := exec.Command(os.Args[0], "-test.run=^TestSpoolAppendWorker$", "-test.count=1")
		c.Env = append(os.Environ(),
			spoolChildDirEnv+"="+dir,
			spoolChildIDEnv+"="+fmt.Sprintf("w%d", w),
			spoolChildWritesEnv+"="+strconv.Itoa(writes))
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, c)
	}
	done := make(chan error, workers)
	for _, c := range procs {
		go func(c *exec.Cmd) { done <- c.Wait() }(c)
	}

	delivered := map[string]int{}
	record := func(batch [][]byte) []ReplayResult {
		for _, b := range batch {
			if !json.Valid(b) {
				t.Errorf("flusher read a corrupt line: %.80q", b)
			}
			delivered[idOf(b)]++
		}
		return deliverBatch(batch)
	}
	finished := 0
	for finished < workers {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("worker failed: %v", err)
			}
			finished++
		default:
			FlushSpool(prod, SpoolFlushLimits{MaxEvents: 7, MaxBytes: 1 << 20}, record)
			time.Sleep(2 * time.Millisecond)
		}
	}
	FlushSpool(prod, SpoolFlushLimits{MaxEvents: 100, MaxBytes: 1 << 20}, record)

	if SpoolCount(prod) != 0 {
		t.Fatalf("spool not drained: %d left", SpoolCount(prod))
	}
	for w := 0; w < workers; w++ {
		for i := 0; i < writes; i++ {
			id := fmt.Sprintf("w%d-%d", w, i)
			if delivered[id] == 0 {
				t.Errorf("event %s was lost", id)
			}
		}
	}
}
