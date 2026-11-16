package eventlog

import (
	"bytes"
	"regexp"
)

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
//
// Performance: this runs inline in the hook over payloads that can be several
// MB, and a regex pass costs far more than a literal search. Each rule
// therefore carries a cheap prefilter, mayMatch, that is a NECESSARY condition
// for the regex to match (it looks for the literal every match must contain).
// A rule whose prefilter fails can't change the payload and is skipped, so the
// output is byte-identical to running every regex unconditionally.
type redactRule struct {
	re       *regexp.Regexp
	replace  []byte
	mayMatch func(b []byte) bool
}

var redactRules = []redactRule{
	// Bearer <token> in any embedded auth string — keep the "Bearer " prefix.
	{
		regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{12,}`), []byte(`${1}` + RedactionMask),
		func(b []byte) bool { return containsFoldASCII(b, []byte("bearer")) },
	},
	// OpenAI-style and PromptConduit-style keys: sk-..., pc_..., pck_...
	{
		regexp.MustCompile(`\bsk-[A-Za-z0-9]{16,}\b`), []byte(RedactionMask),
		containsAny("sk-"),
	},
	{
		regexp.MustCompile(`\bpc[ks]?_[A-Za-z0-9]{16,}\b`), []byte(RedactionMask),
		containsAny("pc_", "pck_", "pcs_"),
	},
	// AWS access key IDs.
	{
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), []byte(RedactionMask),
		containsAny("AKIA"),
	},
	// GitHub personal access / fine-grained tokens.
	{
		regexp.MustCompile(`\bgh[opsu]_[A-Za-z0-9]{20,}\b`), []byte(RedactionMask),
		containsAny("gho_", "ghp_", "ghs_", "ghu_"),
	},
	// Generic "<secret-ish key>": "<value>" JSON pairs (api_key, token,
	// password, secret, access_token, ...). Keep the key, mask the value.
	{
		regexp.MustCompile(`(?i)("(?:[a-z_]*(?:api[_-]?key|secret|token|password)[a-z_]*)"\s*:\s*")[^"]+(")`), []byte(`${1}` + RedactionMask + `${2}`),
		secretPairMayMatch,
	},
}

// RedactBody returns a copy of b with well-known secret patterns masked. The
// input is left unmodified. Full fidelity otherwise: nothing is truncated.
func RedactBody(b []byte) []byte {
	out := b
	copied := false
	for _, rule := range redactRules {
		if !rule.mayMatch(out) {
			continue
		}
		out = rule.re.ReplaceAll(out, rule.replace)
		copied = true
	}
	if !copied {
		// ReplaceAll always returns a fresh slice; keep that contract so a
		// caller can never alias (and mutate) the input through the result.
		out = append([]byte(nil), b...)
	}
	return out
}

// containsAny returns a prefilter that is true when b contains any literal.
func containsAny(lits ...string) func([]byte) bool {
	bs := make([][]byte, len(lits))
	for i, l := range lits {
		bs[i] = []byte(l)
	}
	return func(b []byte) bool {
		for _, l := range bs {
			if bytes.Contains(b, l) {
				return true
			}
		}
		return false
	}
}

// foldTraps are the only non-ASCII runes that Go's (?i) matching folds onto
// ASCII letters: U+212A KELVIN SIGN (k/K) and U+017F LATIN SMALL LETTER LONG S
// (s/S). The ASCII-only case-insensitive prefilters below can't see them, so a
// payload containing either always runs the case-insensitive regexes.
var foldTraps = [][]byte{[]byte("K"), []byte("ſ")}

func hasFoldTrap(b []byte) bool {
	for _, t := range foldTraps {
		if bytes.Contains(b, t) {
			return true
		}
	}
	return false
}

// secretPairMayMatch is the generic rule's prefilter. Every match contains
// (case-insensitively) one of key/secret/token/password, followed by
// [a-z_]* and then `"`, optional whitespace, `:`, optional whitespace, `"`.
// So unless some occurrence of a needle is followed by exactly that shape the
// regex can't match. This skips the common `"input_tokens": 123` case.
func secretPairMayMatch(b []byte) bool {
	if hasFoldTrap(b) {
		return true
	}
	for _, needle := range secretNeedles {
		found := false
		forEachFoldASCII(b, needle, func(i int) bool {
			found = pairTailFollows(b, i+len(needle))
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// secretNeedles: one literal every alternative of the generic rule contains
// ("api[_-]?key" always contains "key").
var secretNeedles = [][]byte{[]byte("key"), []byte("secret"), []byte("token"), []byte("password")}

// pairTailFollows reports whether b[j:] starts with [A-Za-z_]* `"` \s* `:` \s* `"`
// (RE2's \s is [\t\n\f\r ]).
func pairTailFollows(b []byte, j int) bool {
	for j < len(b) && (isASCIILetter(b[j]) || b[j] == '_') {
		j++
	}
	if j >= len(b) || b[j] != '"' {
		return false
	}
	j = skipRE2Space(b, j+1)
	if j >= len(b) || b[j] != ':' {
		return false
	}
	j = skipRE2Space(b, j+1)
	return j < len(b) && b[j] == '"'
}

func skipRE2Space(b []byte, j int) int {
	for j < len(b) {
		switch b[j] {
		case '\t', '\n', '\f', '\r', ' ':
			j++
		default:
			return j
		}
	}
	return j
}

func isASCIILetter(c byte) bool { return (c|0x20) >= 'a' && (c|0x20) <= 'z' }

// containsFoldASCII reports whether b contains needle (lowercase ASCII
// letters) under ASCII case folding.
func containsFoldASCII(b, needle []byte) bool {
	found := false
	forEachFoldASCII(b, needle, func(int) bool { found = true; return false })
	return found
}

// forEachFoldASCII calls fn with the index of every ASCII-case-insensitive
// occurrence of needle (non-empty, lowercase ASCII letters) in b, in order,
// until fn returns false. It hops between candidate first bytes with
// bytes.IndexByte, caching the next position of each case so the whole walk
// stays linear (and close to memchr speed on a miss).
func forEachFoldASCII(b, needle []byte, fn func(i int) bool) {
	end := len(b) - len(needle) + 1 // candidates start before end
	if end <= 0 {
		return
	}
	lo, up := needle[0], needle[0]&^0x20
	next := func(c byte, from int) int {
		if i := bytes.IndexByte(b[from:end], c); i >= 0 {
			return from + i
		}
		return -1
	}
	nLo, nUp := next(lo, 0), next(up, 0)
	for nLo >= 0 || nUp >= 0 {
		at := nLo
		if at < 0 || (nUp >= 0 && nUp < at) {
			at = nUp
		}
		if equalFoldASCII(b[at:at+len(needle)], needle) && !fn(at) {
			return
		}
		if at+1 >= end {
			return
		}
		if nLo == at {
			nLo = next(lo, at+1)
		}
		if nUp == at {
			nUp = next(up, at+1)
		}
	}
}

func equalFoldASCII(s, lowerNeedle []byte) bool {
	for i, c := range lowerNeedle {
		if s[i]|0x20 != c {
			return false
		}
	}
	return true
}
