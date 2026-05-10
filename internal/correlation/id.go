// Package correlation generates and persists W3C Trace Context-compatible
// correlation IDs (trace_id, span_id, parent_span_id) so events emitted by
// the CLI can be stitched into a single trace server-side.
//
// The package is intentionally light: it does not depend on the OpenTelemetry
// SDK and does not speak OTLP. It only produces IDs in the format an OTEL
// exporter would later need.
package correlation

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

const (
	// TraceIDLen is the W3C trace ID length in bytes (16 bytes / 32 hex chars).
	TraceIDLen = 16
	// SpanIDLen is the W3C span ID length in bytes (8 bytes / 16 hex chars).
	SpanIDLen = 8

	zeroTraceID = "00000000000000000000000000000000"
	zeroSpanID  = "0000000000000000"
)

var (
	traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// NewTraceID returns a fresh 16-byte trace ID encoded as 32 lowercase hex chars.
// The all-zero value is rejected per the W3C spec; on the astronomically
// unlikely chance crypto/rand produces it, a single retry is attempted.
func NewTraceID() string {
	for i := 0; i < 2; i++ {
		var b [TraceIDLen]byte
		if _, err := rand.Read(b[:]); err != nil {
			continue
		}
		id := hex.EncodeToString(b[:])
		if id != zeroTraceID {
			return id
		}
	}
	// Fallback: flip a bit so we never return the invalid all-zero value.
	return "00000000000000000000000000000001"
}

// NewSpanID returns a fresh 8-byte span ID encoded as 16 lowercase hex chars.
// The all-zero value is rejected per the W3C spec.
func NewSpanID() string {
	for i := 0; i < 2; i++ {
		var b [SpanIDLen]byte
		if _, err := rand.Read(b[:]); err != nil {
			continue
		}
		id := hex.EncodeToString(b[:])
		if id != zeroSpanID {
			return id
		}
	}
	return "0000000000000001"
}

// IsValidTraceID reports whether s is a non-zero 32-char lowercase hex string.
func IsValidTraceID(s string) bool {
	return s != zeroTraceID && traceIDPattern.MatchString(s)
}

// IsValidSpanID reports whether s is a non-zero 16-char lowercase hex string.
func IsValidSpanID(s string) bool {
	return s != zeroSpanID && spanIDPattern.MatchString(s)
}
