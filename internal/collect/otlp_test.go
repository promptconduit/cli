package collect

import (
	"bytes"
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
	if v := r.Attributes["tokens.input"]; v != int64(412) {
		t.Errorf("tokens.input = %v (%T)", v, v)
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
