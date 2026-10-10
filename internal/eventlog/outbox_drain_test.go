package eventlog

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func queueN(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		AppendOutbox(prod, outboxEnv(fmt.Sprintf("e%d", i), time.Now()))
	}
}

func deliverAll([]byte) ReplayResult { return ReplayDelivered }

// A drain right after a background flush stamped the cooldown still runs.
func TestDrainOutboxIgnoresCooldown(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 3)
	if n := FlushOutbox(prod, 1, deliverAll); n != 1 {
		t.Fatalf("background pass delivered %d, want 1", n)
	}
	if n := FlushOutbox(prod, 10, deliverAll); n != 0 {
		t.Fatalf("background pass inside the cooldown delivered %d, want 0", n)
	}
	res := DrainOutbox(prod, deliverAll, nil)
	if res.Delivered != 2 || res.Remaining != 0 || res.Busy || res.Stopped {
		t.Fatalf("drain = %+v, want 2 delivered and nothing left", res)
	}
}

// The background path keeps its cooldown after a drain (the drain stamps it).
func TestFlushOutboxCooldownHonoredAfterDrain(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 1)
	DrainOutbox(prod, deliverAll, nil)
	queueN(t, 1)
	if n := FlushOutbox(prod, 10, deliverAll); n != 0 {
		t.Fatalf("background flush right after a drain delivered %d, want 0 (cooldown)", n)
	}
}

// The drain has no batch limit: one pass over the whole queue, well past the
// background batch size.
func TestDrainOutboxDeliversWholeQueue(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 1200)
	sends := 0
	res := DrainOutbox(prod, deliverAll, func(FlushStats) { sends++ })
	if res.Tried != 1200 || res.Delivered != 1200 || res.Remaining != 0 || OutboxCount(prod) != 0 {
		t.Fatalf("drain = %+v, outbox = %d; want all 1200 delivered", res, OutboxCount(prod))
	}
	if sends != 1200 {
		t.Fatalf("onSend called %d times, want once per send", sends)
	}
	if st := LoadStatus(); st.Replayed != 1200 {
		t.Fatalf("replayed = %d, want 1200", st.Replayed)
	}
}

// Deliverable envelopes queued behind many failing ones are still tried, and
// no envelope is sent twice in one drain; the failures rotate to the back
// once, in order.
func TestDrainOutboxTriesEverythingBehindFailures(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 800)
	seen := map[string]int{}
	res := DrainOutbox(prod, func(env []byte) ReplayResult {
		id := idOf(env)
		seen[id]++
		var i int
		_, _ = fmt.Sscanf(id, "e%d", &i)
		if i < 500 {
			return ReplaySkip
		}
		return ReplayDelivered
	}, nil)
	if res.Tried != 800 || res.Delivered != 300 || res.Skipped != 500 || res.Remaining != 500 || res.Stopped {
		t.Fatalf("drain = %+v, want 800 tried, 300 delivered, 500 skipped and kept", res)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s sent %d times in one drain, want once", id, n)
		}
	}
	data, err := os.ReadFile(OutboxPath(prod))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 500 || idOf([]byte(lines[0])) != "e0" || idOf([]byte(lines[499])) != "e499" {
		t.Fatalf("want e0..e499 kept in order, got %d lines", len(lines))
	}
}

// ReplayStop (server down, bad credentials, cancelled) ends the drain.
func TestDrainOutboxStopsOnReplayStop(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 5)
	calls := 0
	res := DrainOutbox(prod, func([]byte) ReplayResult {
		calls++
		if calls == 3 {
			return ReplayStop
		}
		return ReplayDelivered
	}, nil)
	if !res.Stopped || res.Tried != 3 || res.Delivered != 2 || res.Remaining != 3 {
		t.Fatalf("drain = %+v, want stopped on the 3rd send with 2 delivered and 3 remaining", res)
	}
}

// A drain never runs alongside another flush holding the lock.
func TestDrainOutboxBusyWhenLocked(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 2)
	if err := os.WriteFile(outboxLockPath(prod), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := DrainOutbox(prod, func([]byte) ReplayResult {
		t.Fatal("must not send while another flush holds the lock")
		return ReplayDelivered
	}, nil)
	if !res.Busy || res.Remaining != 2 || res.Tried != 0 {
		t.Fatalf("drain = %+v, want busy with 2 remaining", res)
	}
}

// When the outbox can't be rewritten after the sends, the stats still report
// what was sent, nothing is removed, and the replayed counter isn't bumped.
func TestDrainOutboxReportsRewriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions as a non-root user")
	}
	useTempEventLog(t)
	queueN(t, 3)
	dir := Dir()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	res := DrainOutbox(prod, func([]byte) ReplayResult {
		_ = os.Chmod(dir, 0o500) // the final rename into dir will fail
		return ReplayDelivered
	}, nil)
	_ = os.Chmod(dir, 0o755)
	if res.Err == nil || res.Tried != 3 || res.Delivered != 3 {
		t.Fatalf("drain = %+v, want a rewrite error with 3 sent", res)
	}
	if OutboxCount(prod) != 3 {
		t.Fatalf("outbox = %d, want all 3 kept", OutboxCount(prod))
	}
	if st := LoadStatus(); st.Replayed != 0 {
		t.Fatalf("replayed = %d, want 0 until entries are actually removed", st.Replayed)
	}
}

func TestDrainOutboxEmptyOrDisabled(t *testing.T) {
	useTempEventLog(t)
	noSend := func([]byte) ReplayResult { t.Fatal("no sends expected"); return ReplayStop }
	if res := DrainOutbox(prod, noSend, nil); res.Tried != 0 || res.Remaining != 0 {
		t.Fatalf("empty drain = %+v", res)
	}
	queueN(t, 1)
	SetEnabled(false)
	if res := DrainOutbox(prod, noSend, nil); res.Tried != 0 || res.Remaining != 1 {
		t.Fatalf("disabled drain = %+v, want untouched queue of 1", res)
	}
}
