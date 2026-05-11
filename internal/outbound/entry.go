// Package outbound mirrors every HTTP request the CLI makes to a local
// ndjson file, so users can run `promptconduit watch` and see what their
// hooks are actually uploading to the platform.
//
// The mirror is a single http.RoundTripper that wraps http.DefaultTransport
// and is installed into every *http.Client constructed by
// internal/client.NewClient. Both the foreground command and the
// `hook --send-event` subprocess thus log into the same file. The file
// is owner-only-readable (0600), bodies are capped at 64KB, and the file
// rotates to a .1 backup when it crosses 50MB.
package outbound

import (
	"encoding/json"
	"net/http"
	"time"
)

// MirrorFileName is the basename of the on-disk mirror, written into
// client.ConfigDir(). Exposed so callers (the watch command, the HTTP
// client wiring) agree on a single source of truth.
const MirrorFileName = "outbound.ndjson"

// Entry is one line in outbound.ndjson — one HTTP request + response.
//
// Field ordering matches the ndjson layout for easy `jq` use. Bodies are
// stored as strings (not raw JSON) so the mirror file remains
// well-formed even when the body bytes are themselves valid JSON
// containing embedded newlines.
type Entry struct {
	TS              time.Time         `json:"ts"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	ReqHeaders      map[string]string `json:"req_headers,omitempty"`
	ReqContentType  string            `json:"req_content_type,omitempty"`
	ReqBody         string            `json:"req_body,omitempty"`
	ReqTruncated    bool              `json:"req_truncated,omitempty"`
	ReqOriginalSize int               `json:"req_original_size_bytes,omitempty"`
	Status          int               `json:"status,omitempty"`
	RespContentType string            `json:"resp_content_type,omitempty"`
	RespBody        string            `json:"resp_body,omitempty"`
	RespTruncated   bool              `json:"resp_truncated,omitempty"`
	LatencyMs       int64             `json:"latency_ms"`
	Error           string            `json:"error,omitempty"`
}

// MarshalLine produces one ndjson line for the entry (no trailing newline).
// Use AppendLine to write to a file; this is split out so it can be tested
// without disk.
func (e Entry) MarshalLine() ([]byte, error) {
	return json.Marshal(e)
}

// headerMap returns a single-value header map keyed by canonical name.
// Multi-value headers are joined with ", "; this is fine for an
// observability surface and avoids the cost of nested slices in ndjson.
func headerMap(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) == 1 {
			out[k] = v[0]
			continue
		}
		joined := ""
		for i, s := range v {
			if i > 0 {
				joined += ", "
			}
			joined += s
		}
		out[k] = joined
	}
	return out
}
