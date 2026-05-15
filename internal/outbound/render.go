package outbound

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// RenderSummary formats one mirror entry for the `watch` command.
// When verbose is true, the request body (and response body, if any)
// follow the summary line, pretty-printed and indented. When color is
// true the HTTP status is ANSI-colored — green 2xx, yellow 3xx-4xx,
// red ≥500 or transport error.
func RenderSummary(e Entry, verbose, color bool) string {
	var b strings.Builder

	b.WriteString(e.TS.Local().Format("15:04:05"))
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%-5s", e.Method))
	b.WriteString(" ")
	b.WriteString(pathOnly(e.URL))
	b.WriteString("  ")
	b.WriteString(humanBytes(len(e.ReqBody)))
	if e.ReqTruncated {
		b.WriteString("+")
	}

	if e.Error != "" {
		b.WriteString("  → ")
		b.WriteString(colorize("ERR", colorRed, color))
		b.WriteString(" (")
		b.WriteString(fmt.Sprintf("%dms", e.LatencyMs))
		b.WriteString(") ")
		b.WriteString(e.Error)
	} else {
		b.WriteString("  → ")
		b.WriteString(colorize(fmt.Sprintf("%d", e.Status), statusColor(e.Status), color))
		b.WriteString(" (")
		b.WriteString(fmt.Sprintf("%dms", e.LatencyMs))
		b.WriteString(")")
	}

	if verbose {
		if e.ReqBody != "" {
			b.WriteString("\n")
			b.WriteString(indent("  ", pretty(e.ReqBody)))
		}
		if e.RespBody != "" {
			b.WriteString("\n  -- response --\n")
			b.WriteString(indent("  ", pretty(e.RespBody)))
		}
	}

	return b.String()
}

// ParseLine deserializes one ndjson line into an Entry.
func ParseLine(line []byte) (Entry, error) {
	var e Entry
	err := json.Unmarshal(line, &e)
	return e, err
}

// IsTerminal reports whether f is connected to a character device,
// i.e. a real terminal rather than a pipe or file redirect. Used to
// decide whether to emit ANSI color codes.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// pathOnly strips scheme/host from a URL for the summary line; full URL
// is still in the underlying entry for users who want it.
func pathOnly(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

// pretty re-indents body if it parses as JSON; otherwise returns it
// unchanged. We don't want a malformed body to drop the row entirely.
func pretty(s string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(out)
}

func indent(prefix, s string) string {
	if s == "" {
		return s
	}
	lines := bytes.Split([]byte(s), []byte{'\n'})
	for i, l := range lines {
		lines[i] = append([]byte(prefix), l...)
	}
	return string(bytes.Join(lines, []byte{'\n'}))
}

const (
	colorReset  = "\x1b[0m"
	colorRed    = "\x1b[31m"
	colorYellow = "\x1b[33m"
	colorGreen  = "\x1b[32m"
)

func statusColor(status int) string {
	switch {
	case status >= 500 || status == 0:
		return colorRed
	case status >= 400:
		return colorRed
	case status >= 300:
		return colorYellow
	case status >= 200:
		return colorGreen
	}
	return ""
}

func colorize(s, code string, enabled bool) string {
	if !enabled || code == "" {
		return s
	}
	return code + s + colorReset
}
