package outbound

import (
	"net/http"
	"testing"
)

func TestRedactHeaders(t *testing.T) {
	cases := []struct {
		name string
		in   http.Header
		want map[string]string
	}{
		{
			name: "authorization is redacted",
			in:   http.Header{"Authorization": []string{"Bearer sk_live_1234"}},
			want: map[string]string{"Authorization": redactedValue},
		},
		{
			name: "cookie is redacted",
			in:   http.Header{"Cookie": []string{"session=abc"}},
			want: map[string]string{"Cookie": redactedValue},
		},
		{
			name: "x-api-key is redacted (canonical form)",
			in:   http.Header{"X-Api-Key": []string{"super-secret"}},
			want: map[string]string{"X-Api-Key": redactedValue},
		},
		{
			name: "x-secret-token is redacted (substring rule)",
			in:   http.Header{"X-Secret-Token": []string{"nope"}},
			want: map[string]string{"X-Secret-Token": redactedValue},
		},
		{
			name: "content-type is preserved",
			in:   http.Header{"Content-Type": []string{"application/json"}},
			want: map[string]string{"Content-Type": "application/json"},
		},
		{
			name: "user-agent is preserved",
			in:   http.Header{"User-Agent": []string{"PromptConduit-CLI/0.5.0"}},
			want: map[string]string{"User-Agent": "PromptConduit-CLI/0.5.0"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := redactHeaders(c.in)
			got := headerMap(out)
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestRedactHeaders_NilSafe(t *testing.T) {
	if h := redactHeaders(nil); h != nil {
		t.Errorf("expected nil for empty input; got %v", h)
	}
}

func TestRedactHeaders_DoesNotMutateInput(t *testing.T) {
	in := http.Header{"Authorization": []string{"Bearer real-token"}}
	_ = redactHeaders(in)
	if in.Get("Authorization") != "Bearer real-token" {
		t.Errorf("input header was mutated: %q", in.Get("Authorization"))
	}
}
