package collect

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

type signalKind string

const (
	signalTraces signalKind = "traces"
	signalLogs   signalKind = "logs"
)

// otlpRequest is the minimal subset of the OTLP/HTTP+JSON request shape we
// need to land traces and logs on disk. The OTLP spec is far larger; this
// keeps just the fields the dashboard surfaces. Anything we don't model is
// preserved at the attribute level (flattenKV handles arbitrary keys).
type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans,omitempty"`
	ResourceLogs  []otlpResourceLogs  `json:"resourceLogs,omitempty"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScope struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind,omitempty"`
	StartTimeUnixNano any            `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   any            `json:"endTimeUnixNano,omitempty"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            *otlpStatus    `json:"status,omitempty"`
}

type otlpLogRecord struct {
	TimeUnixNano   any            `json:"timeUnixNano,omitempty"`
	SeverityNumber int            `json:"severityNumber,omitempty"`
	SeverityText   string         `json:"severityText,omitempty"`
	Body           otlpAnyValue   `json:"body,omitempty"`
	Attributes     []otlpKeyValue `json:"attributes,omitempty"`
	TraceID        string         `json:"traceId,omitempty"`
	SpanID         string         `json:"spanId,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpAnyValue mirrors OTLP's AnyValue oneof. We deserialize whichever
// branch is present and flatten it to a Go scalar/slice/map for display.
type otlpAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	IntValue    *json.Number    `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
	KVListValue *otlpKVList     `json:"kvlistValue,omitempty"`
	BytesValue  *string         `json:"bytesValue,omitempty"`
}

type otlpArrayValue struct {
	Values []otlpAnyValue `json:"values"`
}

type otlpKVList struct {
	Values []otlpKeyValue `json:"values"`
}

// newOTLPHandler returns an http.Handler that accepts OTLP/HTTP+JSON for a
// given signal and writes flattened rows to the store.
func newOTLPHandler(store *Store, signal signalKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ct := r.Header.Get("Content-Type")
		if ct != "" && ct != "application/json" {
			// Protobuf is on the roadmap; be explicit so users can tell the
			// local stack apart from the cloud one (which will accept both).
			http.Error(w, "only application/json is accepted (set OTEL_EXPORTER_OTLP_PROTOCOL=http/json)", http.StatusUnsupportedMediaType)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 16*1024*1024))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req otlpRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
			return
		}

		received := time.Now().UTC()
		switch signal {
		case signalTraces:
			for _, rs := range req.ResourceSpans {
				resourceAttrs := flattenKV(rs.Resource.Attributes)
				for _, ss := range rs.ScopeSpans {
					for _, s := range ss.Spans {
						_ = store.AppendSpan(spanToRow(received, resourceAttrs, ss.Scope, s))
					}
				}
			}
		case signalLogs:
			for _, rl := range req.ResourceLogs {
				resourceAttrs := flattenKV(rl.Resource.Attributes)
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						_ = store.AppendLog(logRecordToRow(received, resourceAttrs, sl.Scope, lr))
					}
				}
			}
		}

		// OTLP success response is an empty PartialSuccess.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}
}

// SpanRow is the on-disk shape for a single span. Kept flat so the
// dashboard can render rows without re-walking nested structures.
type SpanRow struct {
	ReceivedAt        time.Time      `json:"received_at"`
	TraceID           string         `json:"trace_id"`
	SpanID            string         `json:"span_id"`
	ParentSpanID      string         `json:"parent_span_id,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind,omitempty"`
	StartTimeUnixNano uint64         `json:"start_unix_nano,omitempty"`
	EndTimeUnixNano   uint64         `json:"end_unix_nano,omitempty"`
	DurationMs        float64        `json:"duration_ms,omitempty"`
	StatusCode        int            `json:"status_code,omitempty"`
	StatusMessage     string         `json:"status_message,omitempty"`
	ScopeName         string         `json:"scope_name,omitempty"`
	ScopeVersion      string         `json:"scope_version,omitempty"`
	ServiceName       string         `json:"service_name,omitempty"`
	ResourceAttrs     map[string]any `json:"resource_attrs,omitempty"`
	Attributes        map[string]any `json:"attributes,omitempty"`
}

// LogRow is the on-disk shape for a single log record.
type LogRow struct {
	ReceivedAt    time.Time      `json:"received_at"`
	TimeUnixNano  uint64         `json:"time_unix_nano,omitempty"`
	TraceID       string         `json:"trace_id,omitempty"`
	SpanID        string         `json:"span_id,omitempty"`
	SeverityText  string         `json:"severity_text,omitempty"`
	SeverityNum   int            `json:"severity_num,omitempty"`
	Body          any            `json:"body,omitempty"`
	ScopeName     string         `json:"scope_name,omitempty"`
	ScopeVersion  string         `json:"scope_version,omitempty"`
	ServiceName   string         `json:"service_name,omitempty"`
	ResourceAttrs map[string]any `json:"resource_attrs,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
}

func spanToRow(now time.Time, resourceAttrs map[string]any, scope otlpScope, s otlpSpan) SpanRow {
	start := toNano(s.StartTimeUnixNano)
	end := toNano(s.EndTimeUnixNano)
	row := SpanRow{
		ReceivedAt:        now,
		TraceID:           s.TraceID,
		SpanID:            s.SpanID,
		ParentSpanID:      s.ParentSpanID,
		Name:              s.Name,
		Kind:              s.Kind,
		StartTimeUnixNano: start,
		EndTimeUnixNano:   end,
		ScopeName:         scope.Name,
		ScopeVersion:      scope.Version,
		ResourceAttrs:     resourceAttrs,
		Attributes:        flattenKV(s.Attributes),
	}
	if end > start && start > 0 {
		row.DurationMs = float64(end-start) / 1e6
	}
	if v, ok := resourceAttrs["service.name"].(string); ok {
		row.ServiceName = v
	}
	if s.Status != nil {
		row.StatusCode = s.Status.Code
		row.StatusMessage = s.Status.Message
	}
	return row
}

func logRecordToRow(now time.Time, resourceAttrs map[string]any, scope otlpScope, lr otlpLogRecord) LogRow {
	row := LogRow{
		ReceivedAt:    now,
		TimeUnixNano:  toNano(lr.TimeUnixNano),
		TraceID:       lr.TraceID,
		SpanID:        lr.SpanID,
		SeverityText:  lr.SeverityText,
		SeverityNum:   lr.SeverityNumber,
		Body:          anyValueToGo(lr.Body),
		ScopeName:     scope.Name,
		ScopeVersion:  scope.Version,
		ResourceAttrs: resourceAttrs,
		Attributes:    flattenKV(lr.Attributes),
	}
	if v, ok := resourceAttrs["service.name"].(string); ok {
		row.ServiceName = v
	}
	return row
}

// toNano accepts the JSON encoding of a uint64 nanosecond timestamp, which
// OTLP exporters serialize as either a string (the spec) or a number (some
// emitters). Returns 0 when unparseable.
func toNano(v any) uint64 {
	switch t := v.(type) {
	case string:
		n, err := strconv.ParseUint(t, 10, 64)
		if err != nil {
			return 0
		}
		return n
	case float64:
		return uint64(t)
	case json.Number:
		n, err := strconv.ParseUint(t.String(), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

func flattenKV(kvs []otlpKeyValue) map[string]any {
	if len(kvs) == 0 {
		return nil
	}
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = anyValueToGo(kv.Value)
	}
	return out
}

func anyValueToGo(v otlpAnyValue) any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.IntValue != nil:
		s := v.IntValue.String()
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		return s
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.ArrayValue != nil:
		out := make([]any, len(v.ArrayValue.Values))
		for i, x := range v.ArrayValue.Values {
			out[i] = anyValueToGo(x)
		}
		return out
	case v.KVListValue != nil:
		return flattenKV(v.KVListValue.Values)
	case v.BytesValue != nil:
		return *v.BytesValue
	}
	return nil
}
