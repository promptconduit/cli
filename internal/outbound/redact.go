package outbound

import (
	"net/http"
	"strings"
)

// redactedValue is what every auth-bearing header value becomes in the mirror.
const redactedValue = "***"

// alwaysRedact is the hard-coded list of header names whose values must
// never reach the mirror file. Matched case-insensitively against
// http.Header's canonical form.
var alwaysRedact = map[string]struct{}{
	"Authorization":       {},
	"Cookie":              {},
	"Set-Cookie":          {},
	"X-Api-Key":           {},
	"Proxy-Authorization": {},
}

// substrRedact catches anything whose name contains one of these tokens
// (case-insensitive). Belt-and-braces in case the platform grows new
// header names later.
var substrRedact = []string{"token", "secret", "key"}

// redactHeaders returns a copy of h with values that look like
// credentials replaced by "***". Multi-value headers collapse to a
// single redacted entry.
func redactHeaders(h http.Header) http.Header {
	if len(h) == 0 {
		return nil
	}
	out := make(http.Header, len(h))
	for name, values := range h {
		if shouldRedact(name) {
			out[name] = []string{redactedValue}
			continue
		}
		// Copy the slice so the caller can't mutate ours by surprise.
		dup := make([]string, len(values))
		copy(dup, values)
		out[name] = dup
	}
	return out
}

func shouldRedact(name string) bool {
	if _, ok := alwaysRedact[http.CanonicalHeaderKey(name)]; ok {
		return true
	}
	lower := strings.ToLower(name)
	for _, needle := range substrRedact {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
