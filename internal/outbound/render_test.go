package outbound

import (
	"strings"
	"testing"
	"time"
)

func TestRenderSummary_Default(t *testing.T) {
	e := Entry{
		TS:        time.Date(2026, 5, 11, 15, 30, 42, 0, time.UTC),
		Method:    "POST",
		URL:       "https://api.example.com/v1/events/raw",
		ReqBody:   strings.Repeat("x", 3200),
		Status:    200,
		LatencyMs: 87,
	}
	got := RenderSummary(e, false, false)
	// Time is rendered in local TZ; just match the rest.
	for _, want := range []string{"POST", "/v1/events/raw", "3.1KB", "200", "(87ms)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in summary: %q", want, got)
		}
	}
	// No ANSI color codes when disabled.
	if strings.Contains(got, "\x1b[") {
		t.Errorf("color leaked into summary: %q", got)
	}
}

func TestRenderSummary_VerboseIncludesBody(t *testing.T) {
	e := Entry{
		TS:        time.Now(),
		Method:    "POST",
		URL:       "https://example/v1/events/raw",
		ReqBody:   `{"foo":"bar"}`,
		Status:    200,
		LatencyMs: 1,
	}
	got := RenderSummary(e, true, false)
	if !strings.Contains(got, `"foo"`) {
		t.Errorf("verbose summary missing body: %q", got)
	}
	// Pretty-printed JSON should be indented with two spaces.
	if !strings.Contains(got, `  "foo": "bar"`) {
		t.Errorf("expected pretty-printed JSON; got %q", got)
	}
}

func TestRenderSummary_ColorOnStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, colorGreen},
		{301, colorYellow},
		{404, colorRed},
		{500, colorRed},
	}
	for _, c := range cases {
		e := Entry{TS: time.Now(), Method: "GET", URL: "/x", Status: c.status}
		got := RenderSummary(e, false, true)
		if !strings.Contains(got, c.want) {
			t.Errorf("status %d: missing color %q in %q", c.status, c.want, got)
		}
	}
}

func TestRenderSummary_NetworkError(t *testing.T) {
	e := Entry{
		TS:        time.Now(),
		Method:    "POST",
		URL:       "https://nope",
		LatencyMs: 5000,
		Error:     "dial tcp: connect: connection refused",
	}
	got := RenderSummary(e, false, false)
	if !strings.Contains(got, "ERR") {
		t.Errorf("expected ERR marker for transport failure; got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("expected error message preserved; got %q", got)
	}
}

func TestTruncateBody(t *testing.T) {
	body, trunc, orig := truncateBody([]byte("hello"), 100)
	if string(body) != "hello" || trunc || orig != 5 {
		t.Errorf("short body should pass through: %q %v %d", body, trunc, orig)
	}
	body, trunc, orig = truncateBody([]byte("hello world"), 5)
	if string(body) != "hello" || !trunc || orig != 11 {
		t.Errorf("long body should truncate: %q %v %d", body, trunc, orig)
	}
	body, trunc, orig = truncateBody([]byte("any"), 0)
	if string(body) != "any" || trunc || orig != 3 {
		t.Errorf("max=0 should disable truncation: %q %v %d", body, trunc, orig)
	}
}

func TestParseLine(t *testing.T) {
	line := []byte(`{"ts":"2026-05-11T15:30:42Z","method":"POST","url":"/x","status":204,"latency_ms":7}`)
	e, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if e.Method != "POST" || e.Status != 204 || e.LatencyMs != 7 {
		t.Errorf("parsed wrong fields: %+v", e)
	}
}
