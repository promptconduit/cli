package eventlog

import "regexp"

// RedactionMask replaces any matched secret-looking substring.
const RedactionMask = "***REDACTED***"

// We write the FULL envelope payload to events.ndjson (full fidelity), so the
// only safety pass is a conservative scrub of substrings that look like
// credentials. The envelope body itself never carries the PromptConduit API
// key — that travels in the Authorization header, which is not part of what we
// log here. These patterns mainly guard against a secret a user pasted into a
// prompt (which lands inside native_payload).
//
// Patterns are intentionally narrow to avoid corrupting legitimate payload
// content. Each rule's replacement keeps any captured context groups and masks
// only the secret value, never surrounding JSON.
type redactRule struct {
	re      *regexp.Regexp
	replace string
}

var redactRules = []redactRule{
	// Bearer <token> in any embedded auth string — keep the "Bearer " prefix.
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{12,}`), `${1}` + RedactionMask},
	// OpenAI-style and PromptConduit-style keys: sk-..., pc_..., pck_...
	{regexp.MustCompile(`\bsk-[A-Za-z0-9]{16,}\b`), RedactionMask},
	{regexp.MustCompile(`\bpc[ks]?_[A-Za-z0-9]{16,}\b`), RedactionMask},
	// AWS access key IDs.
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), RedactionMask},
	// GitHub personal access / fine-grained tokens.
	{regexp.MustCompile(`\bgh[opsu]_[A-Za-z0-9]{20,}\b`), RedactionMask},
	// Generic "<secret-ish key>": "<value>" JSON pairs (api_key, token,
	// password, secret, access_token, ...). Keep the key, mask the value.
	{regexp.MustCompile(`(?i)("(?:[a-z_]*(?:api[_-]?key|secret|token|password)[a-z_]*)"\s*:\s*")[^"]+(")`), `${1}` + RedactionMask + `${2}`},
}

// RedactBody returns a copy of b with well-known secret patterns masked. The
// input is left unmodified. Full fidelity otherwise: nothing is truncated.
func RedactBody(b []byte) []byte {
	out := b
	for _, rule := range redactRules {
		out = rule.re.ReplaceAll(out, []byte(rule.replace))
	}
	return out
}
