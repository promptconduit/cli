package eventlog

import (
	"os"
	"strings"
	"testing"
	"time"
)

const prod = "https://api.example"

func outboxEnv(id string, capturedAt time.Time) []byte {
	return []byte(`{"schema":2,"event_id":"` + id + `","captured_at":"` + capturedAt.UTC().Format(time.RFC3339) + `"}`)
}

func useTempEventLog(t *testing.T) {
	t.Helper()
	SetDirForTest(t.TempDir())
	SetEnabled(true)
	t.Cleanup(func() {
		SetDirForTest("")
		SetEnabled(false)
	})
}

func idOf(env []byte) string {
	s := string(env)
	i := strings.Index(s, `"event_id":"`) + len(`"event_id":"`)
	return s[i : i+strings.Index(s[i:], `"`)]
}

func TestOutboxAppendAndCount(t *testing.T) {
	useTempEventLog(t)
	if OutboxCount(prod) != 0 {
		t.Fatal("empty outbox should count 0")
	}
	AppendOutbox(prod, outboxEnv("a", time.Now()))
	AppendOutbox(prod, outboxEnv("b", time.Now()))
	if got := OutboxCount(prod); got != 2 {
		t.Fatalf("OutboxCount = %d, want 2", got)
	}
}

// Events queued for one API URL are never replayed to another.
func TestOutboxIsPerTarget(t *testing.T) {
	useTempEventLog(t)
	AppendOutbox(prod, outboxEnv("p", time.Now()))
	if n := FlushOutbox("http://localhost:8787", 10, func([]byte) ReplayResult {
		t.Fatal("prod's queue must not be sent to another target")
		return ReplayDelivered
	}); n != 0 || OutboxCount(prod) != 1 {
		t.Fatalf("n=%d prod queue=%d; want prod queue untouched", n, OutboxCount(prod))
	}
}

func TestFlushOutboxOutcomes(t *testing.T) {
	useTempEventLog(t)
	now := time.Now()
	for _, id := range []string{"ok1", "bad", "poison", "ok2", "stop", "later"} {
		AppendOutbox(prod, outboxEnv(id, now))
	}
	AppendOutbox(prod, outboxEnv("expired", now.Add(-31*24*time.Hour)))

	var sent []string
	n := FlushOutbox(prod, 100, func(env []byte) ReplayResult {
		id := idOf(env)
		sent = append(sent, id)
		switch id {
		case "bad":
			return ReplayRejected
		case "poison":
			return ReplaySkip // fails, but must not block what follows
		case "stop":
			return ReplayStop
		case "expired":
			t.Fatal("expired entries must be dropped without sending")
		}
		return ReplayDelivered
	})
	if n != 2 {
		t.Fatalf("delivered = %d, want 2", n)
	}
	if strings.Join(sent, ",") != "ok1,bad,poison,ok2,stop" {
		t.Fatalf("send order = %v (a skip continues, a stop ends the pass)", sent)
	}
	data, _ := os.ReadFile(OutboxPath(prod))
	rest := string(data)
	// A skipped envelope rotates behind the entries it would otherwise block.
	if strings.Index(rest, `"later"`) > strings.Index(rest, `"poison"`) {
		t.Errorf("poison should rotate to the back:\n%s", rest)
	}
	if st := LoadStatus(); st.Dropped != 1 {
		t.Errorf("expired entry should count as dropped, got %d", st.Dropped)
	}
	for _, keep := range []string{"poison", "stop", "later"} {
		if !strings.Contains(rest, `"`+keep+`"`) {
			t.Errorf("%s should remain queued", keep)
		}
	}
	for _, gone := range []string{"ok1", "bad", "ok2", "expired"} {
		if strings.Contains(rest, `"`+gone+`"`) {
			t.Errorf("%s should be removed", gone)
		}
	}
}

func TestFlushOutboxCooldownAndLimit(t *testing.T) {
	useTempEventLog(t)
	for _, id := range []string{"a", "b", "c"} {
		AppendOutbox(prod, outboxEnv(id, time.Now()))
	}
	calls := 0
	deliver := func([]byte) ReplayResult { calls++; return ReplayDelivered }
	if n := FlushOutbox(prod, 2, deliver); n != 2 {
		t.Fatalf("first pass delivered %d, want the limit 2", n)
	}
	if n := FlushOutbox(prod, 2, deliver); n != 0 || calls != 2 {
		t.Fatalf("cooldown not honored: n=%d calls=%d", n, calls)
	}
	if OutboxCount(prod) != 1 {
		t.Fatalf("want 1 left after the limited pass, got %d", OutboxCount(prod))
	}
}

func TestFlushOutboxEmptyIsNoop(t *testing.T) {
	useTempEventLog(t)
	if n := FlushOutbox(prod, 10, func([]byte) ReplayResult { t.Fatal("no sends expected"); return ReplayStop }); n != 0 {
		t.Fatalf("n = %d", n)
	}
}

// A replay moves the event from failed to sent and counts it as replayed.
func TestReplayMovesFailedToSent(t *testing.T) {
	useTempEventLog(t)
	RecordSendOutcome("e1", "Stop", 503, 5, 3, errFor("API error: 503"))
	AppendOutbox(prod, outboxEnv("e1", time.Now()))
	FlushOutbox(prod, 10, func([]byte) ReplayResult { return ReplayDelivered })
	st := LoadStatus()
	if st.Failed != 0 || st.Sent != 1 || st.Replayed != 1 {
		t.Fatalf("failed=%d sent=%d replayed=%d; want 0/1/1", st.Failed, st.Sent, st.Replayed)
	}
}

func TestRecordSendOutcomeCountsRetried(t *testing.T) {
	useTempEventLog(t)
	RecordSendOutcome("e", "Stop", 201, 5, 3, nil)
	RecordSendOutcome("f", "Stop", 201, 5, 1, nil)
	st := LoadStatus()
	if st.Sent != 2 || st.Retried != 1 {
		t.Fatalf("sent=%d retried=%d, want 2 and 1", st.Sent, st.Retried)
	}
}

type testErr string

func (e testErr) Error() string { return string(e) }
func errFor(s string) error     { return testErr(s) }
