// Package loop builds "The Loop" view of one prompt: the prompt as a sequence
// of model round-trips (the ledger), and re-pricings of the same steps under
// other approaches (the advice). Almost all agent cost is the model re-reading
// the conversation on every round-trip, so cost ≈ context size × round-trips;
// the ledger shows that and the advice says what would have been cheaper.
//
// The JSON shapes (camelCase) and rules are mirrored by the editor extension
// and the platform. Keep them in sync.
package loop

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

// Tokens is a call's token breakdown.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// USD is a call's dollar breakdown.
type USD struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Total      float64 `json:"total"`
}

// Tool is one tool call a model call asked for (names and numbers only).
type Tool struct {
	Name       string `json:"name"`
	OK         *bool  `json:"ok,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	MCPServer  string `json:"mcpServer,omitempty"`
}

// Call is one model round-trip.
type Call struct {
	RequestID string `json:"requestId"`
	TS        string `json:"ts"`
	Model     string `json:"model"`
	Tokens    Tokens `json:"tokens"`
	USD       USD    `json:"usd"`
	// ContextTokens is what the model read: input + cache_read + cache_write.
	ContextTokens int64  `json:"contextTokens"`
	Tools         []Tool `json:"tools"`
	// AfterPrompt marks a call that ran after the prompt's Stop (a background
	// task woke the agent).
	AfterPrompt bool `json:"afterPrompt,omitempty"`
}

// Totals splits a prompt's USD into re-reading cached history, writing new
// context (cache writes + uncached input) and model output.
type Totals struct {
	USD        float64 `json:"usd"`
	Reread     float64 `json:"reread"`
	NewContext float64 `json:"newContext"`
	Output     float64 `json:"output"`
}

// Cache TTL values for Ledger.CacheTTL.
const (
	TTL1h      = "1h"
	TTL5m      = "5m"
	TTLUnknown = "unknown"
)

// Ledger is one prompt as round-trips.
type Ledger struct {
	PromptID  string `json:"promptId"`
	TaskKind  string `json:"taskKind"`
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
	// Calls are in time order, after-prompt calls last.
	Calls  []Call `json:"calls"`
	Totals Totals `json:"totals"`
	// RoundTrips counts the calls inside the prompt (no after-prompt calls).
	RoundTrips int     `json:"roundTrips"`
	AvgContext float64 `json:"avgContext"`
	// LongestGapMs is the longest wait between two consecutive in-prompt
	// calls; LongestGapAfterCall is the index of the call before it (-1 when
	// there are fewer than two calls).
	LongestGapMs        int64  `json:"longestGapMs"`
	LongestGapAfterCall int    `json:"longestGapAfterCall"`
	CacheTTL            string `json:"cacheTtl"`
}

// Advice ids.
const (
	AdviceFewerRoundTrips  = "fewer-round-trips"
	AdviceFreshSession     = "fresh-session"
	AdviceFreshPlusScript  = "fresh-plus-script"
	AdviceAvoidModelSwitch = "avoid-model-switch"
	AdviceCacheExpiry      = "cache-expiry"
	AdviceAfterPromptWake  = "after-prompt-wake"
)

// Advice kinds: an option re-prices the same steps another way; a note
// explains something that already happened.
const (
	KindOption = "option"
	KindNote   = "note"
)

// Advice is one "what would have been cheaper" line.
type Advice struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// USDIfApplied is the same steps re-priced (option) or the amount the
	// note is about (note).
	USDIfApplied float64 `json:"usdIfApplied"`
	// DeltaUSD is USDIfApplied − actual (negative = saving); 0 for notes.
	DeltaUSD float64 `json:"deltaUsd"`
	// GuideSlug names the public guide at /coaching/<slug>.
	GuideSlug string `json:"guideSlug,omitempty"`
}

// SessionOpening is how the prompt's session started: a fresh session pays
// this again.
type SessionOpening struct {
	FirstCallUSD     float64 `json:"firstCallUsd"`
	FirstCallContext int64   `json:"firstCallContext"`
}

const (
	fiveMinMs = int64(5 * time.Minute / time.Millisecond)
	// minSaving: options that save less than this aren't worth a line.
	minSaving = 0.02
	// switchMinContext: below this the model-switch comparison is noise.
	switchMinContext = 50_000
	// captureSlack: event captured_at is whole seconds, call timestamps carry
	// milliseconds.
	captureSlack = time.Second
)

// ---- inputs ----------------------------------------------------------------

// Request is one priced API request as the CLI's cost enrichment records it.
type Request struct {
	RequestID string
	TS        string
	Model     string
	Tokens    cost.Tokens
	USD       cost.Cost
	// ToolNames is the request's names-only tool summary (name -> count).
	ToolNames map[string]int
}

// ToolResult is one finished tool call (from the tools enrichment).
type ToolResult struct {
	At         time.Time // when the result was captured
	Name       string
	OK         bool
	DurationMs int64
	MCPServer  string
}

// Prompt is one prompt's span in a session.
type Prompt struct {
	PromptID  string
	TaskKind  string
	StartedAt time.Time
	EndedAt   time.Time // zero while the prompt is open
	// Requests and ToolResults are filled by Attribute / the caller.
	Requests    []Request
	AfterPrompt map[string]bool // request ids that ran after EndedAt
	ToolResults []ToolResult
}

// Attribute assigns a session's requests to its prompts by time, defensively
// re-attributing what a Stop event mislabelled: a request earlier than a
// prompt's start belongs to the prompt before it, and a request after a
// prompt's end but before the next prompt is an after-prompt call of the
// earlier prompt. prompts must be in time order; requests are deduped by id
// (first sighting wins, later sightings add their tools). Requests before the
// first prompt are dropped.
func Attribute(prompts []*Prompt, requests []Request) {
	seen := map[string]*Request{}
	var unique []*Request
	for i := range requests {
		r := requests[i]
		if prev, ok := seen[r.RequestID]; ok && r.RequestID != "" {
			for name, n := range r.ToolNames {
				if prev.ToolNames == nil {
					prev.ToolNames = map[string]int{}
				}
				prev.ToolNames[name] += n
			}
			continue
		}
		cp := r
		cp.ToolNames = copyCounts(r.ToolNames)
		seen[r.RequestID] = &cp
		unique = append(unique, &cp)
	}
	for _, r := range unique {
		ts, ok := parseTS(r.TS)
		if !ok {
			continue
		}
		var owner *Prompt
		for _, p := range prompts {
			if !p.StartedAt.After(ts) {
				owner = p
			}
		}
		if owner == nil {
			continue
		}
		owner.Requests = append(owner.Requests, *r)
		if !owner.EndedAt.IsZero() && ts.After(owner.EndedAt) {
			if owner.AfterPrompt == nil {
				owner.AfterPrompt = map[string]bool{}
			}
			owner.AfterPrompt[r.RequestID] = true
		}
	}
}

func copyCounts(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// ---- ledger ----------------------------------------------------------------

// BuildLedger builds one prompt's ledger. table detects the cache TTL from
// the rate the requests were priced at (nil = unknown). ok is false when the
// prompt has no requests.
func BuildLedger(p *Prompt, table *cost.PriceTable) (Ledger, bool) {
	if len(p.Requests) == 0 {
		return Ledger{}, false
	}
	reqs := append([]Request(nil), p.Requests...)
	sort.SliceStable(reqs, func(i, j int) bool {
		a, _ := parseTS(reqs[i].TS)
		b, _ := parseTS(reqs[j].TS)
		return a.Before(b)
	})
	var calls, after []Call
	for _, r := range reqs {
		c := toCall(r)
		if p.AfterPrompt[r.RequestID] {
			c.AfterPrompt = true
			after = append(after, c)
		} else {
			calls = append(calls, c)
		}
	}
	attachTools(calls, reqs, p.ToolResults)
	calls = append(calls, after...)

	l := Ledger{
		PromptID:            p.PromptID,
		TaskKind:            p.TaskKind,
		Calls:               calls,
		LongestGapAfterCall: -1,
		CacheTTL:            detectTTL(calls, table),
	}
	if l.TaskKind == "" {
		l.TaskKind = "other"
	}
	if !p.StartedAt.IsZero() {
		l.StartedAt = p.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if !p.EndedAt.IsZero() {
		l.EndedAt = p.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	var ctxSum int64
	var prev time.Time
	for i, c := range calls {
		l.Totals.USD += c.USD.Total
		l.Totals.Reread += c.USD.CacheRead
		l.Totals.NewContext += c.USD.CacheWrite + c.USD.Input
		l.Totals.Output += c.USD.Output
		if c.AfterPrompt {
			continue
		}
		l.RoundTrips++
		ctxSum += c.ContextTokens
		ts, ok := parseTS(c.TS)
		if i > 0 && ok && !prev.IsZero() {
			if gap := ts.Sub(prev).Milliseconds(); gap > l.LongestGapMs {
				l.LongestGapMs = gap
				l.LongestGapAfterCall = i - 1
			}
		}
		if ok {
			prev = ts
		}
	}
	if l.RoundTrips > 0 {
		l.AvgContext = float64(ctxSum) / float64(l.RoundTrips)
	}
	return l, true
}

func toCall(r Request) Call {
	return Call{
		RequestID: r.RequestID,
		TS:        r.TS,
		Model:     r.Model,
		Tokens: Tokens{
			Input: r.Tokens.Input, Output: r.Tokens.Output,
			CacheRead: r.Tokens.CacheRead, CacheWrite: r.Tokens.CacheWrite,
		},
		USD: USD{
			Input: r.USD.Input, Output: r.USD.Output,
			CacheRead: r.USD.CacheRead, CacheWrite: r.USD.CacheWrite, Total: r.USD.Total,
		},
		ContextTokens: r.Tokens.Input + r.Tokens.CacheRead + r.Tokens.CacheWrite,
		Tools:         []Tool{},
	}
}

var mcpNameRE = regexp.MustCompile(`^mcp__(.+?)__`)

// attachTools gives each in-prompt call the tools it asked for. Tool results
// with timing go to the latest call that started at or before the tool
// started (finish − duration, plus capture slack). Calls then get their own
// names-only counts topped up with any tools results didn't cover, so a call
// always lists what it issued even without result events.
func attachTools(calls []Call, reqs []Request, results []ToolResult) {
	if len(calls) == 0 {
		return
	}
	for _, t := range results {
		start := t.At.Add(-time.Duration(t.DurationMs) * time.Millisecond).Add(captureSlack)
		host := 0
		for i, c := range calls {
			if ts, ok := parseTS(c.TS); ok && !ts.After(start) {
				host = i
			}
		}
		ok := t.OK
		calls[host].Tools = append(calls[host].Tools, Tool{Name: t.Name, OK: &ok, DurationMs: t.DurationMs, MCPServer: t.MCPServer})
	}
	byID := map[string]map[string]int{}
	for _, r := range reqs {
		byID[r.RequestID] = r.ToolNames
	}
	for i := range calls {
		want := byID[calls[i].RequestID]
		have := map[string]int{}
		for _, t := range calls[i].Tools {
			have[t.Name]++
		}
		names := make([]string, 0, len(want))
		for n := range want {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for k := have[n]; k < want[n]; k++ {
				tool := Tool{Name: n}
				if m := mcpNameRE.FindStringSubmatch(n); m != nil {
					tool.MCPServer = m[1]
				}
				calls[i].Tools = append(calls[i].Tools, tool)
			}
		}
	}
}

// detectTTL reads the cache TTL off the first call with a cache write: the
// per-token rate it was priced at is nearer the model's 1h or 5m write rate.
func detectTTL(calls []Call, table *cost.PriceTable) string {
	if table == nil {
		return TTLUnknown
	}
	for _, c := range calls {
		if c.Tokens.CacheWrite <= 0 || c.USD.CacheWrite <= 0 {
			continue
		}
		mp, ok := table.ResolvePrice(c.Model)
		if !ok || mp.CacheCreation1h == 0 || mp.CacheCreation == 0 {
			continue
		}
		rate := c.USD.CacheWrite / float64(c.Tokens.CacheWrite)
		if math.Abs(rate-mp.CacheCreation1h) < math.Abs(rate-mp.CacheCreation) {
			return TTL1h
		}
		return TTL5m
	}
	return TTLUnknown
}

// ---- advice ----------------------------------------------------------------

// readOnlyTools are tools that only look things up or check status, so a
// run of calls issuing nothing else could have been one call. The CLI never
// reads tool inputs, so Bash can't be split into status checks (gh, curl, git
// status, ls) and real work; it counts as a status call.
var readOnlyTools = map[string]bool{
	"Bash": true, "BashOutput": true, "Read": true, "Grep": true, "Glob": true, "LS": true,
	"ToolSearch": true, "WebFetch": true, "WebSearch": true, "NotebookRead": true,
	"TodoRead": true, "TaskList": true, "TaskGet": true, "TaskOutput": true,
}

func readOnlyCall(c Call) bool {
	if len(c.Tools) == 0 {
		return false
	}
	for _, t := range c.Tools {
		if !readOnlyTools[t.Name] {
			return false
		}
	}
	return true
}

// CollapsedRoundTrips is the round-trip count after collapsing each run of
// ≥ 2 consecutive calls that issued only read-only/status tools into one call.
func CollapsedRoundTrips(calls []Call) int {
	kept, inRun := 0, false
	for _, c := range calls {
		ro := readOnlyCall(c)
		if ro && inRun {
			continue
		}
		kept++
		inRun = ro
	}
	return kept
}

var premiumModelRE = regexp.MustCompile(`opus|fable|mythos`)

// SiblingModel is the cheaper model to test "just switch models" against.
func SiblingModel(model string) string {
	switch {
	case premiumModelRE.MatchString(model):
		return "claude-sonnet-5"
	case strings.Contains(model, "sonnet"):
		return "claude-haiku-4-5"
	}
	return ""
}

func writeRate(mp cost.ModelPrice) float64 {
	if mp.CacheCreation1h > 0 {
		return mp.CacheCreation1h
	}
	if mp.CacheCreation > 0 {
		return mp.CacheCreation
	}
	return mp.Input
}

// repriceFresh prices the calls in a fresh session: the first call costs
// what this session's first call cost (plus its own output), later calls
// re-read the opening context plus what the new session has accumulated and
// keep their own writes, input and output.
func repriceFresh(calls []Call, open SessionOpening, mp cost.ModelPrice) float64 {
	total, grown := 0.0, int64(0)
	for i, c := range calls {
		if i == 0 {
			total += open.FirstCallUSD + c.USD.Output
		} else {
			total += float64(open.FirstCallContext+grown)*mp.CacheRead + c.USD.CacheWrite + c.USD.Input + c.USD.Output
		}
		grown += c.Tokens.CacheWrite + c.Tokens.Input + c.Tokens.Output
	}
	return total
}

// repriceOnModel prices the calls on another model; call 1 re-caches the
// whole context (1h write rate) because the new model has no cache yet.
func repriceOnModel(calls []Call, mp cost.ModelPrice) float64 {
	write, read := writeRate(mp), mp.CacheRead
	if read == 0 {
		read = mp.Input
	}
	total := 0.0
	for i, c := range calls {
		t := c.Tokens
		out := float64(t.Output) * mp.Output
		if i == 0 {
			total += float64(c.ContextTokens)*write + out
		} else {
			total += float64(t.CacheRead)*read + float64(t.CacheWrite)*write + float64(t.Input)*mp.Input + out
		}
	}
	return total
}

func usd(v float64) string { return fmt.Sprintf("$%.2f", v) }

func tokensK(n float64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", n/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dK", int64(math.Round(n/1000)))
	}
	return fmt.Sprintf("%d", int64(math.Round(n)))
}

func fmtGap(ms int64) string {
	s := int64(math.Round(float64(ms) / 1000))
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

// BuildAdvice derives the advice for a ledger: options (cheapest first), then
// notes. open is the session's opening call (nil when unknown); table is the
// CLI's rate table.
func BuildAdvice(l Ledger, table *cost.PriceTable, open *SessionOpening) []Advice {
	var calls, wakes []Call
	for _, c := range l.Calls {
		if c.AfterPrompt {
			wakes = append(wakes, c)
		} else {
			calls = append(calls, c)
		}
	}
	if len(calls) == 0 || table == nil {
		return []Advice{}
	}
	actual := 0.0
	for _, c := range calls {
		actual += c.USD.Total
	}
	model := calls[0].Model
	price, priced := table.ResolvePrice(model)

	var options, notes []Advice
	option := func(a Advice) {
		a.Kind = KindOption
		a.DeltaUSD = a.USDIfApplied - actual
		options = append(options, a)
	}

	kept := CollapsedRoundTrips(calls)
	scripted := actual / float64(len(calls)) * float64(kept)
	if kept < len(calls) && actual-scripted >= minSaving {
		option(Advice{
			ID:    AdviceFewerRoundTrips,
			Title: "Fewer round-trips",
			Detail: fmt.Sprintf("%d of the %d round-trips only looked things up or checked status, back to back. Batching those into one call (or one script) gets this down to %d round-trips, and every round-trip skipped saves a full re-read of ~%s tokens.",
				len(calls)-kept, len(calls), kept, tokensK(l.AvgContext)),
			USDIfApplied: scripted,
			GuideSlug:    "batch-tool-calls",
		})
	}

	if open != nil && priced && l.AvgContext > float64(open.FirstCallContext)*1.5 {
		fresh := repriceFresh(calls, *open, price)
		if actual-fresh >= minSaving {
			option(Advice{
				ID:    AdviceFreshSession,
				Title: "Same steps in a fresh session (or a subagent)",
				Detail: fmt.Sprintf("A fresh session first pays %s to cache its ~%s tokens of setup, then each round-trip re-reads %s instead of %s. A subagent works the same way and keeps this conversation intact. Only worth it when the task doesn't need the history.",
					usd(open.FirstCallUSD), tokensK(float64(open.FirstCallContext)), tokensK(float64(open.FirstCallContext)), tokensK(l.AvgContext)),
				USDIfApplied: fresh,
				GuideSlug:    "delegate-with-subagents",
			})
		}
		if kept < len(calls) {
			both := repriceFresh(calls[:kept], *open, price)
			if actual-both >= minSaving && both > scripted {
				option(Advice{
					ID:    AdviceFreshPlusScript,
					Title: "Fewer round-trips, in a fresh session",
					Detail: fmt.Sprintf("Combining both doesn't help here. With only %d round-trips, the %s start-up cost of a fresh session never pays back.",
						kept, usd(open.FirstCallUSD)),
					USDIfApplied: both,
				})
			}
		}
	}

	if sib := SiblingModel(model); sib != "" && priced && l.AvgContext > switchMinContext {
		if sibPrice, ok := table.ResolvePrice(sib); ok {
			switched := repriceOnModel(calls, sibPrice)
			var detail string
			if switched > actual {
				same := ""
				if sibPrice.CacheRead == price.CacheRead {
					same = fmt.Sprintf("%s reads cache at the same rate as %s, and ", sib, model)
				}
				detail = fmt.Sprintf("Costs more, not less. %sswitching models means caching all ~%s tokens again for the new model on the first call.",
					same, tokensK(float64(calls[0].ContextTokens)))
			} else {
				detail = fmt.Sprintf("Cheaper even after re-caching ~%s tokens for the new model. Switch at the start of the next session, where the re-cache is smallest.",
					tokensK(float64(calls[0].ContextTokens)))
			}
			option(Advice{
				ID:           AdviceAvoidModelSwitch,
				Title:        fmt.Sprintf("Switch to %s for this part", sib),
				Detail:       detail,
				USDIfApplied: switched,
			})
		}
	}

	if l.LongestGapMs > fiveMinMs && l.LongestGapAfterCall >= 0 && l.LongestGapAfterCall+1 < len(calls) && priced {
		next := calls[l.LongestGapAfterCall+1]
		gap := fmtGap(l.LongestGapMs)
		switch {
		case l.CacheTTL == TTL1h:
			rate5m := price.CacheCreation
			if rate5m == 0 {
				rate5m = price.Input * 1.25
			}
			avoided := float64(next.ContextTokens) * rate5m
			notes = append(notes, Advice{
				ID:    AdviceCacheExpiry,
				Kind:  KindNote,
				Title: fmt.Sprintf("%s wait, cache kept", gap),
				Detail: fmt.Sprintf("The model waited %s before call #%d, longer than a standard 5-minute prompt cache. This session caches for 1 hour, so nothing was rewritten. With a 5-minute cache that call would have rewritten ~%s tokens for about %s.",
					gap, l.LongestGapAfterCall+2, tokensK(float64(next.ContextTokens)), usd(avoided)),
				USDIfApplied: avoided,
				GuideSlug:    "improve-cache-hit-rate",
			})
		case next.USD.CacheWrite > minSaving:
			notes = append(notes, Advice{
				ID:    AdviceCacheExpiry,
				Kind:  KindNote,
				Title: fmt.Sprintf("%s wait, cache expired", gap),
				Detail: fmt.Sprintf("The cache expired during a %s wait, so call #%d rewrote ~%s tokens. Avoid long blocking waits mid-task, or use a 1-hour cache.",
					gap, l.LongestGapAfterCall+2, tokensK(float64(next.Tokens.CacheWrite))),
				USDIfApplied: next.USD.CacheWrite,
				GuideSlug:    "improve-cache-hit-rate",
			})
		}
	}

	if len(wakes) > 0 {
		w := 0.0
		for _, c := range wakes {
			w += c.USD.Total
		}
		who := "A background task"
		if len(wakes) > 1 {
			who = fmt.Sprintf("%d background tasks", len(wakes))
		}
		notes = append(notes, Advice{
			ID:           AdviceAfterPromptWake,
			Kind:         KindNote,
			Title:        "Charged after the prompt finished",
			Detail:       who + " finished after this prompt ended and woke the agent, which re-read the conversation to report it.",
			USDIfApplied: w,
		})
	}

	sort.SliceStable(options, func(i, j int) bool { return options[i].USDIfApplied < options[j].USDIfApplied })
	return append(append([]Advice{}, options...), notes...)
}
