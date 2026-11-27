// Package enrich computes the slug-keyed `enrichments` map carried on every
// v2 event envelope (see internal/envelope). Each enrichment is an independent
// Enricher: it declares its slug, which events it applies to, and how to build
// its payload from the hook context.
//
// Adding a new enrichment = one new file in this package implementing Enricher
// plus a Register call in init(). Rules every enricher must honor:
//   - Payloads are plain JSON-serializable values.
//   - Failure is isolated: an error or panic drops that slug only, never the
//     event and never the hook. Enrichers must be best-effort by construction.
//     A slug whose absence would be misread (cost) implements failureReporter
//     to carry an explicit error instead.
//   - Keep it fast: the hook is latency-sensitive. Anything slow (network,
//     API lookups) must be cached on disk and refreshed out-of-band — see
//     vcs.go's detached PR refresh for the pattern.
//   - Readers (editor extension, platform, panels) ignore unknown slugs, so
//     shipping a new slug requires no coordinated release.
package enrich

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/promptconduit/cli/internal/logger"
)

// Context is what every enricher receives: the parsed hook event plus the
// identifiers the envelope already lifted out of it.
type Context struct {
	Tool      string
	HookEvent string
	SessionID string
	PromptID  string
	// RawEvent is the parsed native hook payload; RawJSON its original bytes.
	RawEvent map[string]interface{}
	RawJSON  []byte
	// Cwd is the tool-reported working directory ("" when unknown).
	Cwd string
	// TranscriptPath is the tool-reported transcript file ("" when unknown).
	TranscriptPath string
}

// Enricher computes one enrichment slug.
type Enricher interface {
	// Slug is the enrichments-map key this enricher produces (e.g. "vcs").
	Slug() string
	// Applies reports whether this enricher should run for the event.
	Applies(ctx *Context) bool
	// Enrich builds the payload. Returning (nil, nil) or an error omits the
	// slug from this event.
	Enrich(ctx *Context) (any, error)
}

// enricherTimeout bounds each enricher. Generous because git extraction runs
// several subprocesses (each with its own 2s cap); a stuck enricher is cut
// loose rather than stalling the hook indefinitely.
const enricherTimeout = 5 * time.Second

// registry holds the enrichers in registration order. Populated by init()
// funcs in this package; not mutated after program start.
var registry []Enricher

// Register appends an enricher. Called from init() in each enricher's file.
func Register(e Enricher) {
	registry = append(registry, e)
}

// Run executes every applicable enricher and returns the enrichments map for
// the envelope. Never returns an error: a failing enricher just loses its slug.
//
// Applicable enrichers run concurrently (git, transcript and disk work
// overlap instead of adding up), except those sharing the per-session state
// file (sessionStateUser), which run one after another in registration order
// on a single goroutine. Each enricher's payload lands in its own slot and the
// map is assembled after all finish, so the output does not depend on timing.
func Run(ctx *Context) map[string]json.RawMessage {
	var (
		parallel []int // indexes into applicable
		stateful []int
	)
	applicable := make([]Enricher, 0, len(registry))
	for _, e := range registry {
		if !e.Applies(ctx) {
			continue
		}
		if _, ok := e.(sessionStateUser); ok {
			stateful = append(stateful, len(applicable))
		} else {
			parallel = append(parallel, len(applicable))
		}
		applicable = append(applicable, e)
	}

	results := make([]json.RawMessage, len(applicable))
	runAt := func(i int) { results[i] = runAndMarshal(applicable[i], ctx) }

	var wg sync.WaitGroup
	for _, i := range parallel {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runAt(i)
		}(i)
	}
	if len(stateful) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, i := range stateful {
				runAt(i)
			}
		}()
	}
	wg.Wait()

	out := make(map[string]json.RawMessage, len(applicable))
	for i, data := range results {
		if data != nil {
			out[applicable[i].Slug()] = data
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runAndMarshal runs one enricher (isolated, bounded) and serializes its
// payload. nil means the slug is omitted.
func runAndMarshal(e Enricher, ctx *Context) json.RawMessage {
	payload, failure := runOne(e, ctx)
	if failure != "" {
		payload = nil
		if fr, ok := e.(failureReporter); ok {
			payload = fr.failurePayload(ctx, failure)
		}
	}
	if payload == nil {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		logger.Debug("enrich: %s payload did not serialize: %v", e.Slug(), err)
		return nil
	}
	return data
}

// failureReporter is an optional Enricher extension for slugs whose absence
// would be misread (cost: a missing slug looks like a free turn). When Enrich
// errors, panics or times out, Run asks it for a payload that names the
// failure instead of dropping the slug; returning nil still drops it.
type failureReporter interface {
	failurePayload(ctx *Context, reason string) any
}

// runOne executes a single enricher with panic recovery and a timeout. A
// timed-out enricher's goroutine is abandoned (the hook process is short-lived,
// so nothing leaks for long). failure is non-empty, and payload nil, when the
// enricher errored, panicked or timed out.
func runOne(e Enricher, ctx *Context) (payload any, failure string) {
	type result struct {
		payload any
		failure string
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Debug("enrich: %s panicked: %v", e.Slug(), r)
				ch <- result{nil, fmt.Sprintf("enricher panicked: %v", r)}
			}
		}()
		p, err := e.Enrich(ctx)
		if err != nil {
			logger.Debug("enrich: %s failed: %v", e.Slug(), err)
			ch <- result{nil, err.Error()}
			return
		}
		ch <- result{p, ""}
	}()

	select {
	case r := <-ch:
		return r.payload, r.failure
	case <-time.After(enricherTimeout):
		logger.Debug("enrich: %s timed out after %s", e.Slug(), enricherTimeout)
		return nil, fmt.Sprintf("enricher timed out after %s", enricherTimeout)
	}
}
