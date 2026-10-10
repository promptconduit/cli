package eventlog

import (
	"os"
	"strings"
	"testing"
	"time"
)

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

func TestOutboxAppendAndCount(t *testing.T) {
	useTempEventLog(t)
	if OutboxCount() != 0 {
		t.Fatal("empty outbox should count 0")
	}
	AppendOutbox(outboxEnv("a", time.Now()))
	AppendOutbox(outboxEnv("b", time.Now()))
	if got := OutboxCount(); got != 2 {
		t.Fatalf("OutboxCount = %d, want 2", got)
	}
}

func TestFlushOutboxDeliversRejectsAndStops(t *testing.T) {
	useTempEventLog(t)
	now := time.Now()
	for _, id := range []string{"ok1", "bad", "ok2", "stop", "later"} {
		AppendOutbox(outboxEnv(id, now))
	}
	AppendOutbox(outboxEnv("expired", now.Add(-31*24*time.Hour)))

	var sent []string
	n := FlushOutbox(100, func(env []byte) ReplayResult {
		s := string(env)
		switch {
		case strings.Contains(s, `"bad"`):
			sent = append(sent, "bad")
			return ReplayRejected
		case strings.Contains(s, `"stop"`):
			sent = append(sent, "stop")
			return ReplayStop
		case strings.Contains(s, `"expired"`):
			t.Fatal("expired entries must be dropped without sending")
		}
		sent = append(sent, s[strings.Index(s, `"event_id":"`)+12:][:3])
		return ReplayDelivered
	})
	if n != 2 {
		t.Fatalf("delivered = %d, want 2", n)
	}
	// After a stop, the rest of the pass is left alone.
	if strings.Join(sent, ",") != "ok1,bad,ok2,stop" {
		t.Fatalf("send order = %v", sent)
	}
	data, _ := os.ReadFile(OutboxPath())
	rest := string(data)
	if !strings.Contains(rest, `"stop"`) || !strings.Contains(rest, `"later"`) ||
		strings.Contains(rest, `"ok1"`) || strings.Contains(rest, `"bad"`) || strings.Contains(rest, `"expired"`) {
		t.Fatalf("remaining outbox wrong:\n%s", rest)
	}
	if got := LoadStatus().Replayed; got != 2 {
		t.Fatalf("status replayed = %d, want 2", got)
	}
}

func TestFlushOutboxCooldownAndLimit(t *testing.T) {
	useTempEventLog(t)
	for _, id := range []string{"a", "b", "c"} {
		AppendOutbox(outboxEnv(id, time.Now()))
	}
	calls := 0
	deliver := func([]byte) ReplayResult { calls++; return ReplayDelivered }
	if n := FlushOutbox(2, deliver); n != 2 {
		t.Fatalf("first pass delivered %d, want the limit 2", n)
	}
	// Within the cooldown nothing runs, even with work left.
	if n := FlushOutbox(2, deliver); n != 0 || calls != 2 {
		t.Fatalf("cooldown not honored: n=%d calls=%d", n, calls)
	}
	if OutboxCount() != 1 {
		t.Fatalf("want 1 left after the limited pass, got %d", OutboxCount())
	}
}

func TestFlushOutboxEmptyIsNoop(t *testing.T) {
	useTempEventLog(t)
	if n := FlushOutbox(10, func([]byte) ReplayResult { t.Fatal("no sends expected"); return ReplayStop }); n != 0 {
		t.Fatalf("n = %d", n)
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
