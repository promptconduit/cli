package eventlog

import (
	"bufio"
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

// Concurrency regression test for the events.jsonl append path (issue #125).
//
// The corruption this guards against is a cross-PROCESS race, so it cannot be
// reproduced with goroutines: appendLine takes the package-level writeMu, which
// serializes every writer inside a single process. Real hook invocations are
// separate processes that share nothing but the file, so this test re-executes
// the test binary as N independent worker processes that hammer one log.
//
// With the two-write (`Write(line)` then `Write("\n")`) implementation this
// fails: O_APPEND makes each individual write atomic with respect to the file
// offset, but between a worker's line write and its newline write another
// worker can land its own line, yielding `A_lineB_line\n\n` — one line holding
// two concatenated envelopes, followed by one empty line.

const (
	childDirEnv    = "PROMPTCONDUIT_EVENTLOG_CHILD_DIR"
	childIDEnv     = "PROMPTCONDUIT_EVENTLOG_CHILD_ID"
	childStartEnv  = "PROMPTCONDUIT_EVENTLOG_CHILD_START"
	childWritesEnv = "PROMPTCONDUIT_EVENTLOG_CHILD_WRITES"
	childPadEnv    = "PROMPTCONDUIT_EVENTLOG_CHILD_PAD"
)

// TestEventLogAppendWorker is not an assertion-bearing test. It is the worker
// body that TestAppendLineIsAtomicAcrossProcesses re-executes as a separate
// process, and is skipped during a normal test run.
func TestEventLogAppendWorker(t *testing.T) {
	dir := os.Getenv(childDirEnv)
	if dir == "" {
		t.Skip("not running as an append worker")
	}

	id := os.Getenv(childIDEnv)
	writes, err := strconv.Atoi(os.Getenv(childWritesEnv))
	if err != nil {
		t.Fatalf("bad %s: %v", childWritesEnv, err)
	}
	pad, err := strconv.Atoi(os.Getenv(childPadEnv))
	if err != nil {
		t.Fatalf("bad %s: %v", childPadEnv, err)
	}
	startNano, err := strconv.ParseInt(os.Getenv(childStartEnv), 10, 64)
	if err != nil {
		t.Fatalf("bad %s: %v", childStartEnv, err)
	}

	SetDirForTest(dir)
	SetEnabled(true)

	// All workers idle until a common wall-clock instant so their writes
	// actually overlap instead of being staggered by process startup.
	if d := time.Until(time.Unix(0, startNano)); d > 0 {
		time.Sleep(d)
	}

	filler := strings.Repeat("x", pad)
	for i := 0; i < writes; i++ {
		RecordCapture([]byte(fmt.Sprintf(
			`{"schema":2,"event_id":"%s-%d","pad":"%s"}`, id, i, filler)))
	}
}

func TestAppendLineIsAtomicAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns worker processes; skipped under -short")
	}

	// Oversubscribe the CPUs so workers are preempted mid-append and actually
	// race, but stay bounded: this spawns real processes, and an unbounded
	// 2*NumCPU on a large CI host would fork a swarm for no extra signal.
	workers := 2 * runtime.NumCPU()
	if workers < 8 {
		workers = 8
	}
	if workers > 24 {
		workers = 24
	}
	const (
		writesPerWorker = 1500
		// ~2KB lines, matching the size of the envelopes observed corrupted in
		// the field report on issue #125.
		padBytes = 2000
	)

	// Must point THIS process at the temp dir too, not just the workers:
	// EventsJSONLPath() below is resolved in the parent, and without the
	// override it falls back to the real ~/.promptconduit/events.jsonl.
	dir := withTempDir(t)
	start := time.Now().Add(750 * time.Millisecond)

	var wg sync.WaitGroup
	errs := make([]error, workers)
	output := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestEventLogAppendWorker$")
			cmd.Env = append(os.Environ(),
				childDirEnv+"="+dir,
				childIDEnv+"="+strconv.Itoa(i),
				childStartEnv+"="+strconv.FormatInt(start.UnixNano(), 10),
				childWritesEnv+"="+strconv.Itoa(writesPerWorker),
				childPadEnv+"="+strconv.Itoa(padBytes),
			)
			out, err := cmd.CombinedOutput()
			errs[i], output[i] = err, string(out)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v\n%s", i, err, output[i])
		}
	}

	f, err := os.Open(EventsJSONLPath())
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var (
		lineNo     int
		good       int
		empty      int
		corrupt    int
		firstFails []string
	)
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			empty++
			if len(firstFails) < 5 {
				firstFails = append(firstFails, fmt.Sprintf("line %d: empty line", lineNo))
			}
			continue
		}
		// json.Unmarshal is strict about trailing data: two concatenated
		// envelopes on one line fail here exactly as they do in the strict
		// per-line readers (the editor extension's JSON.parse).
		var v map[string]any
		if err := json.Unmarshal(line, &v); err != nil {
			corrupt++
			if len(firstFails) < 5 {
				firstFails = append(firstFails, fmt.Sprintf(
					"line %d (len %d): %v", lineNo, len(line), err))
			}
			continue
		}
		good++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan events.jsonl: %v", err)
	}

	wantLines := workers * writesPerWorker
	t.Logf("workers=%d writes/worker=%d padBytes=%d", workers, writesPerWorker, padBytes)
	t.Logf("lines=%d valid=%d corrupt=%d empty=%d (want %d valid, 0 corrupt, 0 empty)",
		lineNo, good, corrupt, empty, wantLines)

	if corrupt > 0 || empty > 0 {
		t.Errorf("events.jsonl corrupted by concurrent appends: %d unparseable line(s), %d empty line(s)\n%s",
			corrupt, empty, strings.Join(firstFails, "\n"))
	}
	if good != wantLines {
		t.Errorf("valid lines = %d, want %d (events lost or merged)", good, wantLines)
	}
}
