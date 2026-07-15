package eventlog

import "net/http"

// RecordSendOutcome updates the rolling status counters (and errors.log on
// failure) after a POST to the events endpoint. The payload itself is NOT
// re-recorded here — events.jsonl already captured it at hook time; full HTTP
// diagnostics live in ~/.config/promptconduit/outbound.ndjson (`promptconduit
// watch`). Best-effort and gated on Enabled().
//
// eventID is the envelope's event_id and is what makes a failure line
// actionable: it's the key that finds the exact offending envelope in
// events.jsonl. Both identifiers render as "-" when the caller couldn't
// determine them (an unparseable payload), which is itself a signal worth
// seeing in the log.
func RecordSendOutcome(eventID, hookEvent string, status int, latencyMs int64, sendErr error) {
	failed := sendErr != nil || status < 200 || status >= 300
	if !failed {
		Bump(OutcomeSent, "")
		return
	}
	detail := ""
	if sendErr != nil {
		detail = sendErr.Error()
	}
	if detail == "" {
		detail = httpDetail(status)
	}
	Errorf("send failed event=%s event_id=%s status=%d latency=%dms: %s",
		emptyDash(hookEvent), emptyDash(eventID), status, latencyMs, detail)
	Bump(OutcomeFailed, detail)
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func httpDetail(status int) string {
	if status == 0 {
		return "no response"
	}
	if txt := http.StatusText(status); txt != "" {
		return txt
	}
	return "unexpected status"
}
