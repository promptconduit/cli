package outbound

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DefaultMaxBodyBytes is the per-direction body cap recorded in the mirror.
// 64KB keeps the file small enough to tail comfortably while still
// preserving the full envelope JSON for typical hook events (a few KB).
// Image-bearing UserPromptSubmit envelopes or large transcript chunks
// will exceed this and be recorded as truncated.
const DefaultMaxBodyBytes = 64 * 1024 // 64 KB

// Mirror is an http.RoundTripper that records each request/response pair
// to an ndjson file before returning the response to the caller. It
// wraps a base transport — typically http.DefaultTransport.
//
// Mirror is safe for concurrent use.
type Mirror struct {
	Path         string            // destination ndjson file
	Base         http.RoundTripper // wrapped transport
	MaxBodyBytes int               // per-direction body cap; 0 ⇒ DefaultMaxBodyBytes
	RotateAt     int64             // file-size rotation threshold; 0 ⇒ DefaultRotateAt
	Now          func() time.Time  // injectable clock for tests; nil ⇒ time.Now
}

// New constructs a Mirror writing to path. If base is nil,
// http.DefaultTransport is used. The parent directory is created with
// mode 0700 if missing; the file itself is created on first write with
// mode 0600.
func New(path string, base http.RoundTripper) *Mirror {
	if base == nil {
		base = http.DefaultTransport
	}
	// Best-effort: the parent dir is usually already created by
	// SaveFileConfig, but be defensive in case the user has only ever
	// run hooks before running anything else.
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	return &Mirror{
		Path:         path,
		Base:         base,
		MaxBodyBytes: DefaultMaxBodyBytes,
		RotateAt:     DefaultRotateAt,
		Now:          time.Now,
	}
}

// RoundTrip captures the request, forwards it through the base
// transport, captures the response, and appends a single ndjson line
// to the mirror file. Mirror write failures are intentionally
// swallowed: observability must never break the network path.
func (m *Mirror) RoundTrip(req *http.Request) (*http.Response, error) {
	start := m.now()

	reqBody, reqOriginal, reqTrunc, err := drainAndReplace(req, m.maxBody())
	if err != nil {
		// We failed to read the request body before sending. Forward
		// to the base transport with whatever Go has left of the body
		// (often nothing) and record the error.
		resp, sendErr := m.Base.RoundTrip(req)
		m.record(req, reqBody, reqOriginal, reqTrunc, resp, nil, false, 0, time.Since(start), errors.Join(err, sendErr))
		return resp, sendErr
	}

	resp, sendErr := m.Base.RoundTrip(req)

	var respBody []byte
	var respOriginal int
	var respTrunc bool
	if resp != nil {
		respBody, respOriginal, respTrunc, _ = drainAndReplaceResponse(resp, m.maxBody())
		_ = respOriginal // recorded for completeness; not currently exposed
	}

	m.record(req, reqBody, reqOriginal, reqTrunc, resp, respBody, respTrunc, 0, time.Since(start), sendErr)
	return resp, sendErr
}

func (m *Mirror) maxBody() int {
	if m.MaxBodyBytes > 0 {
		return m.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

func (m *Mirror) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// record builds an Entry and appends one ndjson line. Best-effort:
// errors are dropped so a failed write never corrupts the actual
// network call. We do attempt rotation first.
func (m *Mirror) record(req *http.Request, reqBody []byte, reqOriginal int, reqTrunc bool, resp *http.Response, respBody []byte, respTrunc bool, _ int, latency time.Duration, sendErr error) {
	entry := Entry{
		TS:        m.now().UTC(),
		Method:    req.Method,
		URL:       safeURL(req),
		LatencyMs: latency.Milliseconds(),
	}

	if reqHeaders := redactHeaders(req.Header); reqHeaders != nil {
		entry.ReqHeaders = headerMap(reqHeaders)
		entry.ReqContentType = req.Header.Get("Content-Type")
	}
	if len(reqBody) > 0 {
		entry.ReqBody = string(reqBody)
		entry.ReqTruncated = reqTrunc
		if reqTrunc {
			entry.ReqOriginalSize = reqOriginal
		}
	}

	if resp != nil {
		entry.Status = resp.StatusCode
		entry.RespContentType = resp.Header.Get("Content-Type")
		if len(respBody) > 0 {
			entry.RespBody = string(respBody)
			entry.RespTruncated = respTrunc
		}
	}
	if sendErr != nil {
		entry.Error = sendErr.Error()
	}

	rotateAt := m.RotateAt
	if rotateAt == 0 {
		rotateAt = DefaultRotateAt
	}
	_ = rotateIfNeeded(m.Path, rotateAt)

	line, err := entry.MarshalLine()
	if err != nil {
		return
	}
	_ = appendLine(m.Path, line)
}

// drainAndReplace reads at most max bytes from req.Body for the mirror,
// then replaces req.Body with a new ReadCloser that yields the full
// original content (read part + remainder). Returns (bytes-for-mirror,
// original-size, truncated?, error).
//
// If the request has no body, returns (nil, 0, false, nil) and leaves
// req.Body untouched.
func drainAndReplace(req *http.Request, max int) ([]byte, int, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, 0, false, nil
	}
	all, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, 0, false, err
	}
	if cerr := req.Body.Close(); cerr != nil {
		return nil, 0, false, cerr
	}
	// Replace the body so the inner transport sees the full bytes.
	req.Body = io.NopCloser(bytes.NewReader(all))
	// Reset ContentLength so chunked/length headers stay consistent.
	req.ContentLength = int64(len(all))
	clipped, truncated, original := truncateBody(all, max)
	return clipped, original, truncated, nil
}

// drainAndReplaceResponse is the response-side equivalent. The caller
// must still close the returned response body when done; we replace it
// with a NopCloser around a bytes.Reader containing the original
// content so downstream consumers see the same bytes.
func drainAndReplaceResponse(resp *http.Response, max int) ([]byte, int, bool, error) {
	if resp.Body == nil || resp.Body == http.NoBody {
		return nil, 0, false, nil
	}
	all, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, false, err
	}
	if cerr := resp.Body.Close(); cerr != nil {
		return nil, 0, false, cerr
	}
	resp.Body = io.NopCloser(bytes.NewReader(all))
	clipped, truncated, original := truncateBody(all, max)
	return clipped, original, truncated, nil
}

func safeURL(req *http.Request) string {
	if req.URL == nil {
		return ""
	}
	// Don't redact query strings here — endpoints don't put secrets in
	// query strings today. If that changes, redact at this layer.
	return req.URL.String()
}
