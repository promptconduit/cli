package cost

import "testing"

// TestResolvePrice_CursorFastIDs: Cursor's fast-mode ids bill at the fast
// rate (Anthropic fast mode for Opus; Cursor's GPT-5 Fast row for gpt-5-*-fast).
// Without an exact key or alias, prefix-trim lands each on its standard row
// and understates spend 2x (6x for Opus 4.7 fast mode).
func TestResolvePrice_CursorFastIDs(t *testing.T) {
	tbl := mustTable(t)
	cases := []struct {
		model, wantKey                     string
		in, out, write5m, write1h, cacheRd float64
	}{
		{"claude-opus-5-5-fast", "claude-opus-5-5-fast", 8e-6, 40e-6, 10e-6, 16e-6, 0.4e-6},
		{"claude-opus-5-fast", "claude-opus-5-fast", 10e-6, 50e-6, 12.5e-6, 20e-6, 1e-6},
		{"claude-opus-4-8-fast", "claude-opus-4-8-fast", 10e-6, 50e-6, 12.5e-6, 20e-6, 1e-6},
		{"claude-opus-4-7-fast", "claude-opus-4-7-fast", 30e-6, 150e-6, 37.5e-6, 60e-6, 3e-6},
		{"gpt-5-high-fast", "gpt-5-fast", 2.5e-6, 20e-6, 0, 0, 0.25e-6},
		{"gpt-5-low-fast", "gpt-5-fast", 2.5e-6, 20e-6, 0, 0, 0.25e-6},
		{"gpt-5-high", "gpt-5", 1.25e-6, 10e-6, 0, 0, 0.125e-6},
	}
	for _, tc := range cases {
		key, mp, ok := tbl.resolve(tc.model)
		if !ok || key != tc.wantKey {
			t.Errorf("%s: resolved to %q ok=%v, want %q", tc.model, key, ok, tc.wantKey)
			continue
		}
		if !near(mp.Input, tc.in) || !near(mp.Output, tc.out) || !near(mp.CacheCreation, tc.write5m) ||
			!near(mp.CacheCreation1h, tc.write1h) || !near(mp.CacheRead, tc.cacheRd) {
			t.Errorf("%s: rates %+v", tc.model, mp)
		}
	}
}

// TestResolvePrice_DottedClaudeIDs: dotted display ids for every 5.x model
// resolve to their own row. claude-sonnet-5.5 and claude-haiku-5.5 used to
// trim to "claude-sonnet" / "claude-haiku" and go unpriced.
func TestResolvePrice_DottedClaudeIDs(t *testing.T) {
	tbl := mustTable(t)
	for dotted, want := range map[string]string{
		"claude-fable-5.1":  "claude-fable-5-1",
		"claude-mythos-5.1": "claude-mythos-5-1",
		"claude-opus-5.5":   "claude-opus-5-5",
		"claude-sonnet-5.5": "claude-sonnet-5-5",
		"claude-haiku-5.5":  "claude-haiku-5-5",
	} {
		if key, _, ok := tbl.resolve(dotted); !ok || key != want {
			t.Errorf("%s: resolved to %q ok=%v, want %q", dotted, key, ok, want)
		}
	}
}
