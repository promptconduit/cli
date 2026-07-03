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
//   - Keep it fast: the hook is latency-sensitive. Anything slow (network,
//     API lookups) must be cached on disk and refreshed out-of-band — see
//     vcs.go's detached PR refresh for the pattern.
//   - Readers (editor extension, platform, panels) ignore unknown slugs, so
//     shipping a new slug requires no coordinated release.
package enrich

import (
	"encoding/json"
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
func Run(ctx *Context) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(registry))
	for _, e := range registry {
		if !e.Applies(ctx) {
			continue
		}
		payload, ok := runOne(e, ctx)
		if !ok || payload == nil {
			continue
		}
		data, err := json.Marshal(payload)
		if err != nil {
			logger.Debug("enrich: %s payload did not serialize: %v", e.Slug(), err)
			continue
		}
		out[e.Slug()] = data
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runOne executes a single enricher with panic recovery and a timeout. A
// timed-out enricher's goroutine is abandoned (the hook process is short-lived,
// so nothing leaks for long).
func runOne(e Enricher, ctx *Context) (payload any, ok bool) {
	type result struct {
		payload any
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Debug("enrich: %s panicked: %v", e.Slug(), r)
				ch <- result{nil, nil}
			}
		}()
		p, err := e.Enrich(ctx)
		ch <- result{p, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			logger.Debug("enrich: %s failed: %v", e.Slug(), r.err)
			return nil, false
		}
		return r.payload, true
	case <-time.After(enricherTimeout):
		logger.Debug("enrich: %s timed out after %s", e.Slug(), enricherTimeout)
		return nil, false
	}
}
