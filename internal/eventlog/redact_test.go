package eventlog

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"unicode"
)

// redactReference is the pre-prefilter implementation: every regex over the
// whole payload, unconditionally. RedactBody must match it byte for byte.
func redactReference(b []byte) []byte {
	out := b
	for _, rule := range redactRules {
		out = rule.re.ReplaceAll(out, rule.replace)
	}
	return out
}

var redactCases = []struct {
	name string
	in   string
}{
	// Positive: each rule fires.
	{"bearer", `{"h":"Authorization: Bearer abcdefghijklmnop123"}`},
	{"bearer mixed case + tabs", "{\"h\":\"bEaReR\t\tabcdefghijklmnop.-_x\"}"},
	{"openai key", `{"k":"sk-abcdefghijklmnopqrstuvwx"}`},
	{"pc key", `{"k":"pc_abcdefghijklmnopqrst"}`},
	{"pck key", `{"k":"pck_abcdefghijklmnopqrst"}`},
	{"pcs key", `{"k":"pcs_abcdefghijklmnopqrst"}`},
	{"aws", `{"k":"AKIAABCDEFGHIJKLMNOP"}`},
	{"github", `{"k":"ghp_abcdefghijklmnopqrstuvwxyz"} ghs_abcdefghijklmnopqrstuv gho_abcdefghijklmnopqrstuvwx ghu_abcdefghijklmnopqrstuvw`},
	{"generic api_key", `{"api_key":"hunter2"}`},
	{"generic api-key spaced", "{\"X_API-KEY\" \t:\n \"hunter2\"}"},
	{"generic token suffix", `{"access_token":"abc","refresh_token" : "def"}`},
	{"generic password upper", `{"DB_PASSWORD":"p@ss"}`},
	{"generic secret mid", `{"client_secret_value":"s3cr3t"}`},
	{"all at once", `{"api_key":"a","t":"Bearer abcdefghijklmnop","o":"sk-abcdefghijklmnopqrstu","p":"pc_abcdefghijklmnopqr","a":"AKIAABCDEFGHIJKLMNOP","g":"ghp_abcdefghijklmnopqrstuv"}`},
	// Unicode case-fold traps: Go's (?i) folds U+212A KELVIN SIGN onto k and
	// U+017F LONG S onto s, so these DO match the case-insensitive rules.
	{"kelvin in key", "{\"api_\u212aey\":\"hunter2\"}"},
	{"long s in secret", "{\"\u017fecret\":\"hunter2\"}"},
	{"long s in password", "{\"pa\u017f\u017fword\":\"hunter2\"}"},
	// Negative / near misses: must be left exactly as is.
	{"plain", `{"tool_name":"Read","tool_input":{"file_path":"/x"}}`},
	{"token counts are numbers", `{"usage":{"input_tokens":123,"output_tokens":4,"cache_read_input_tokens":0}}`},
	{"escaped pair inside a string", `{"content":"{\"api_key\": \"hunter2\"}"}`},
	{"empty value", `{"api_key":""}`},
	{"short bearer", `{"h":"Bearer short"}`},
	{"sk too short", `{"k":"sk-abc"}`},
	{"sk inside word", `{"k":"task-abcdefghijklmnopqrstu"}`},
	{"pc inside word", `{"k":"npc_abcdefghijklmnopqrst"}`},
	{"akia lowercase", `{"k":"akiaABCDEFGHIJKLMNOP"}`},
	{"gh wrong letter", `{"k":"ghx_abcdefghijklmnopqrstuvwxyz"}`},
	{"key not a pair", `{"keyboard":1,"monkey":[1],"token":null,"secret":true}`},
	{"needle at very end", `"tok`},
	{"empty", ``},
}

func TestRedactBodyMatchesReference(t *testing.T) {
	for _, tc := range redactCases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			got := RedactBody(in)
			want := redactReference([]byte(tc.in))
			if !bytes.Equal(got, want) {
				t.Fatalf("RedactBody differs from reference\n in:   %q\n got:  %q\n want: %q", tc.in, got, want)
			}
			if string(in) != tc.in {
				t.Fatalf("input was mutated")
			}
		})
	}
}

func TestRedactBodyPositiveCasesActuallyRedact(t *testing.T) {
	for _, tc := range redactCases[:17] {
		if got := RedactBody([]byte(tc.in)); !bytes.Contains(got, []byte(RedactionMask)) {
			t.Errorf("%s: expected a redaction, got %q", tc.name, got)
		}
	}
}

func TestRedactBodyReturnsFreshSlice(t *testing.T) {
	in := []byte(`{"plain":"nothing secret here"}`)
	out := RedactBody(in)
	out[0] = 'X'
	if in[0] != '{' {
		t.Fatal("result must not alias the input")
	}
}

// TestRedactBodyRandomizedMatchesReference splices secret-ish fragments,
// case variants and fold traps into random JSON-ish noise.
func TestRedactBodyRandomizedMatchesReference(t *testing.T) {
	frags := []string{
		`"`, `:`, ` `, "\t", "\n", `_`, `-`, `\`, `{`, `}`, `,`, `0`, `9`,
		`api`, `API`, `key`, `KEY`, `Key`, `secret`, `SeCrEt`, `token`, `TOKENS`, `password`,
		`bearer `, `Bearer	`, `sk-`, `pc_`, `pck_`, `pcs_`, `AKIA`, `ghp_`, `gho_`, `ghs_`, `ghu_`,
		`abcdefghij`, `ABCDEFGHIJ`, `0123456789`, `KLMNOPQRST`, "\u212a", "\u017f", `é`, `"value"`,
	}
	r := rand.New(rand.NewSource(176))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := r.Intn(40); n > 0; n-- {
			b.WriteString(frags[r.Intn(len(frags))])
		}
		in := b.String()
		if got, want := RedactBody([]byte(in)), redactReference([]byte(in)); !bytes.Equal(got, want) {
			t.Fatalf("mismatch on %q\n got:  %q\n want: %q", in, got, want)
		}
	}
}

func FuzzRedactBodyMatchesReference(f *testing.F) {
	for _, tc := range redactCases {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if got, want := RedactBody([]byte(in)), redactReference([]byte(in)); !bytes.Equal(got, want) {
			t.Fatalf("mismatch on %q\n got:  %q\n want: %q", in, got, want)
		}
	})
}

// TestFoldTrapsCoverEveryNonASCIIFold guards the prefilters' assumption: the
// only non-ASCII runes Go's (?i) folds onto ASCII letters are the foldTraps.
func TestFoldTrapsCoverEveryNonASCIIFold(t *testing.T) {
	traps := map[rune]bool{}
	for _, tr := range foldTraps {
		traps[[]rune(string(tr))[0]] = true
	}
	for c := rune('A'); c <= 'z'; c++ {
		for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
			if f > unicode.MaxASCII && !traps[f] {
				t.Errorf("%q folds to non-ASCII %q, which is not in foldTraps", c, f)
			}
		}
	}
}

func TestForEachFoldASCII(t *testing.T) {
	var got []int
	forEachFoldASCII([]byte("Token tOKEN xtoken tok"), []byte("token"), func(i int) bool {
		got = append(got, i)
		return true
	})
	want := []int{0, 6, 13}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if containsFoldASCII([]byte("bea"), []byte("bearer")) {
		t.Fatal("needle longer than input")
	}
}

// redactBenchPayload is a ~1 MB envelope-like payload with no secrets — the
// common case on the hook hot path (a big Read/Bash tool response).
func redactBenchPayload() []byte {
	body := strings.Repeat("line of source code with some text = 42; // a comment\\n", 1<<20/56)
	return []byte(`{"schema":2,"event_id":"e","hook_event":"PostToolUse","raw_event":{"tool_name":"Read","tool_response":{"content":"` +
		body + `"},"usage":{"input_tokens":123,"output_tokens":45}}}`)
}

func BenchmarkRedactBody1MBNoSecrets(b *testing.B) {
	p := redactBenchPayload()
	b.SetBytes(int64(len(p)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RedactBody(p)
	}
}

func BenchmarkRedactReference1MBNoSecrets(b *testing.B) {
	p := redactBenchPayload()
	b.SetBytes(int64(len(p)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = redactReference(p)
	}
}

func BenchmarkRedactBody1MBWithSecret(b *testing.B) {
	p := append(redactBenchPayload()[:0:0], redactBenchPayload()...)
	p = append(p[:len(p)-1], []byte(`,"api_key":"hunter2"}`)...)
	b.SetBytes(int64(len(p)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RedactBody(p)
	}
}
