package eventlog

import "net/http"

// RecordSendOutcome updates the rolling status counters (and errors.log on
// failure) after a POST to the events endpoint. The payload itself is NOT
// re-recorded here — events.jsonl already captured it at hook time; full HTTP
// diagnostics live in ~/.config/promptconduit/outbound.ndjson (`promptconduit
// watch`). Best-effort and gated on Enabled().
func RecordSendOutcome(hookEvent string, status int, latencyMs int64, sendErr error) {
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
	Errorf("send failed event=%s status=%d latency=%dms: %s",
		emptyDash(hookEvent), status, latencyMs, detail)
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
