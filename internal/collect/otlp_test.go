package collect

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const sampleTraces = `{
  "resourceSpans": [{
    "resource": { "attributes": [
      { "key": "service.name", "value": { "stringValue": "claude-code" } },
      { "key": "claude_code.version", "value": { "stringValue": "1.0.0" } }
    ]},
    "scopeSpans": [{
      "scope": { "name": "anthropic.claude-code", "version": "1.0.0" },
      "spans": [{
        "traceId": "0123456789abcdef0123456789abcdef",
        "spanId": "0123456789abcdef",
        "name": "tool.call",
        "kind": 1,
        "startTimeUnixNano": "1700000000000000000",
        "endTimeUnixNano": "1700000000500000000",
        "attributes": [
          { "key": "tool.name", "value": { "stringValue": "Read" } },
          { "key": "tokens.input", "value": { "intValue": "412" } }
        ],
        "status": { "code": 1 }
      }]
    }]
  }]
}`

func TestOTLPTracesHandler(t *testing.T) {
	dir, err := os.MkdirTemp("", "collect-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler := newOTLPHandler(store, signalTraces)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader([]byte(sampleTraces)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rows, err := store.ReadSpans(0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("trace_id = %q", r.TraceID)
	}
	if r.Name != "tool.call" {
		t.Errorf("name = %q", r.Name)
	}
	if r.DurationMs != 500 {
		t.Errorf("duration_ms = %v, want 500", r.DurationMs)
	}
	if v := r.ResourceAttrs["service.name"]; v != "claude-code" {
		t.Errorf("service.name = %v", v)
	}
	// Round-tripping a SpanRow through NDJSON loses the int64-vs-float64
	// distinction: encoding/json emits int64 as a bare JSON number, then
	// decoding back into map[string]any (SpanRow.Attributes) rebuilds it as
	// float64. The exact integer-typing guarantee from anyValueToGo before
	// the round-trip is covered separately by TestAnyValueToGoTypes below.
	if v := r.Attributes["tokens.input"]; v != float64(412) {
		t.Errorf("tokens.input = %v (%T), want 412", v, v)
	}
	if r.ServiceName != "claude-code" {
		t.Errorf("ServiceName = %q", r.ServiceName)
	}

	if _, err := os.Stat(filepath.Join(dir, "spans.ndjson")); err != nil {
		t.Errorf("spans.ndjson not written: %v", err)
	}

	traces, err := store.ListTraces(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].ServiceName != "claude-code" {
		t.Errorf("traces = %+v", traces)
	}
}

func TestOTLPRejectsNonJSON(t *testing.T) {
	dir, _ := os.MkdirTemp("", "collect-test-*")
	defer os.RemoveAll(dir)
	store, _ := OpenStore(dir)
	defer store.Close()

	handler := newOTLPHandler(store, signalTraces)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader([]byte{0x08, 0x01}))
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestOTLPRejectsGET(t *testing.T) {
	dir, _ := os.MkdirTemp("", "collect-test-*")
	defer os.RemoveAll(dir)
	store, _ := OpenStore(dir)
	defer store.Close()

	handler := newOTLPHandler(store, signalTraces)
	req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestAnyValueToGoTypes pins down the in-process Go types anyValueToGo emits
// before the SpanRow is serialized to NDJSON. The round-trip in
// TestOTLPTracesHandler asserts the post-decode shape (float64); this test
// asserts the pre-encode shape, so a regression that returns the int as a
// string or float64 directly out of anyValueToGo would still get caught.
func TestAnyValueToGoTypes(t *testing.T) {
	s := "hello"
	b := true
	d := 3.14
	n := json.Number("42")
	bs := "Ynl0ZXM="

	cases := []struct {
		name string
		in   otlpAnyValue
		want any
	}{
		{"string", otlpAnyValue{StringValue: &s}, "hello"},
		{"bool", otlpAnyValue{BoolValue: &b}, true},
		{"int-as-string", otlpAnyValue{IntValue: &n}, int64(42)},
		{"double", otlpAnyValue{DoubleValue: &d}, 3.14},
		{"bytes", otlpAnyValue{BytesValue: &bs}, "Ynl0ZXM="},
	}
	for _, c := range cases {
		got := anyValueToGo(c.in)
		if got != c.want {
			t.Errorf("%s: got %v (%T), want %v (%T)", c.name, got, got, c.want, c.want)
		}
	}
}
