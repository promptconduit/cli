package eventlog

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func queueN(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		AppendOutbox(prod, outboxEnv(fmt.Sprintf("e%d", i), time.Now()))
	}
}

// A drain right after a background flush stamped the cooldown still runs.
func TestDrainOutboxIgnoresCooldown(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 3)
	deliver := func([]byte) ReplayResult { return ReplayDelivered }
	if n := FlushOutbox(prod, 1, deliver); n != 1 {
		t.Fatalf("background pass delivered %d, want 1", n)
	}
	if n := FlushOutbox(prod, 10, deliver); n != 0 {
		t.Fatalf("background pass inside the cooldown delivered %d, want 0", n)
	}
	res := DrainOutbox(prod, 10, deliver, nil)
	if res.Delivered != 2 || res.Remaining != 0 || res.Busy || res.Stopped {
		t.Fatalf("drain = %+v, want 2 delivered and nothing left", res)
	}
}

// The background path keeps its cooldown after a drain (the drain stamps it).
func TestFlushOutboxCooldownHonoredAfterDrain(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 1)
	deliver := func([]byte) ReplayResult { return ReplayDelivered }
	DrainOutbox(prod, 10, deliver, nil)
	queueN(t, 1)
	if n := FlushOutbox(prod, 10, deliver); n != 0 {
		t.Fatalf("background flush right after a drain delivered %d, want 0 (cooldown)", n)
	}
}

// Passes repeat in batches until the queue is empty.
func TestDrainOutboxLoopsUntilEmpty(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 7)
	var passes []FlushStats
	res := DrainOutbox(prod, 2, func([]byte) ReplayResult { return ReplayDelivered },
		func(pass int, st FlushStats) { passes = append(passes, st) })
	if res.Passes != 4 || len(passes) != 4 {
		t.Fatalf("passes = %d (callback saw %d), want 4 for 7 envelopes in batches of 2", res.Passes, len(passes))
	}
	if res.Delivered != 7 || res.Remaining != 0 || OutboxCount(prod) != 0 {
		t.Fatalf("drain = %+v, outbox = %d; want all 7 delivered", res, OutboxCount(prod))
	}
	if st := LoadStatus(); st.Replayed != 7 {
		t.Fatalf("replayed = %d, want 7", st.Replayed)
	}
}

// Envelopes that keep failing (ReplaySkip) end the drain once a pass makes no
// progress, instead of looping forever.
func TestDrainOutboxStopsWithoutProgress(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 4)
	calls := 0
	res := DrainOutbox(prod, 2, func(env []byte) ReplayResult {
		calls++
		if idOf(env) == "e0" {
			return ReplayDelivered
		}
		return ReplaySkip
	}, nil)
	// pass 1: e0 delivered, e1 skipped; pass 2: e2, e3 skipped -> no progress.
	if res.Passes != 2 || calls != 4 {
		t.Fatalf("passes=%d calls=%d, want 2 passes and 4 sends", res.Passes, calls)
	}
	if res.Delivered != 1 || res.Remaining != 3 || res.Stopped {
		t.Fatalf("drain = %+v, want 1 delivered, 3 remaining, not stopped", res)
	}
}

// ReplayStop (server down, bad credentials, cancelled) ends the drain.
func TestDrainOutboxStopsOnReplayStop(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 5)
	calls := 0
	res := DrainOutbox(prod, 2, func([]byte) ReplayResult {
		calls++
		if calls == 3 {
			return ReplayStop
		}
		return ReplayDelivered
	}, nil)
	if !res.Stopped || res.Passes != 2 || res.Delivered != 2 || res.Remaining != 3 {
		t.Fatalf("drain = %+v, want stopped in pass 2 with 2 delivered and 3 remaining", res)
	}
}

// A drain never runs alongside another flush holding the lock.
func TestDrainOutboxBusyWhenLocked(t *testing.T) {
	useTempEventLog(t)
	queueN(t, 2)
	if err := os.WriteFile(outboxLockPath(prod), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := DrainOutbox(prod, 10, func([]byte) ReplayResult {
		t.Fatal("must not send while another flush holds the lock")
		return ReplayDelivered
	}, nil)
	if !res.Busy || res.Remaining != 2 || res.Passes != 0 {
		t.Fatalf("drain = %+v, want busy with 2 remaining", res)
	}
}

func TestDrainOutboxEmptyOrDisabled(t *testing.T) {
	useTempEventLog(t)
	noSend := func([]byte) ReplayResult { t.Fatal("no sends expected"); return ReplayStop }
	if res := DrainOutbox(prod, 10, noSend, nil); res.Passes != 0 || res.Remaining != 0 {
		t.Fatalf("empty drain = %+v", res)
	}
	queueN(t, 1)
	SetEnabled(false)
	if res := DrainOutbox(prod, 10, noSend, nil); res.Passes != 0 || res.Remaining != 1 {
		t.Fatalf("disabled drain = %+v, want untouched queue of 1", res)
	}
}
