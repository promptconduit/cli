package enrich

import (
	"bufio"
	"os"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

// SubagentEnrichment is the "subagent" slug, attached to SubagentStart and
// SubagentStop events. Identity and parallelism come from the hook payloads;
// type and duration require joining Stop back to Start (SubagentStop's own
// agent_type field arrives empty), which the per-session state provides; and
// tokens/USD come from pricing the subagent's own transcript at Stop with the
// same engine the cost slug uses. Numbers and identifiers only — no content.
type SubagentEnrichment struct {
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type,omitempty"`
	// Phase is "start" or "stop".
	Phase string `json:"phase"`
	// Concurrent (start only): open subagents in this session including this
	// one — the parallelism signal.
	Concurrent int `json:"concurrent,omitempty"`
	// DurationMs (stop only): wall time since the matching SubagentStart.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Requests/Model/Tokens/USD (stop only): summed from the subagent's own
	// transcript. Model is the costliest one observed.
	Requests int          `json:"requests,omitempty"`
	Model    string       `json:"model,omitempty"`
	Tokens   *cost.Tokens `json:"tokens,omitempty"`
	USD      *cost.Cost   `json:"usd,omitempty"`
}

type subagentEnricher struct{}

func init() { Register(subagentEnricher{}) }

func (subagentEnricher) Slug() string { return "subagent" }

func (subagentEnricher) Applies(ctx *Context) bool {
	if ctx.Tool != "claude-code" {
		return false
	}
	return ctx.HookEvent == "SubagentStart" || ctx.HookEvent == "SubagentStop"
}

func (subagentEnricher) Enrich(ctx *Context) (any, error) {
	agentID, _ := ctx.RawEvent["agent_id"].(string)
	if agentID == "" {
		return nil, nil
	}
	if ctx.HookEvent == "SubagentStart" {
		return subagentStart(ctx, agentID), nil
	}
	return subagentStop(ctx, agentID), nil
}

func subagentStart(ctx *Context, agentID string) *SubagentEnrichment {
	agentType, _ := ctx.RawEvent["agent_type"].(string)

	concurrent := 1
	if ctx.SessionID != "" {
		st := loadState(ctx.SessionID)
		if st.Subagents == nil {
			st.Subagents = map[string]subagentInfo{}
		}
		st.Subagents[agentID] = subagentInfo{
			Type:      agentType,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		}
		concurrent = len(st.Subagents)
		saveState(ctx.SessionID, st)
	}

	return &SubagentEnrichment{
		AgentID:    agentID,
		AgentType:  agentType,
		Phase:      "start",
		Concurrent: concurrent,
	}
}

func subagentStop(ctx *Context, agentID string) *SubagentEnrichment {
	out := &SubagentEnrichment{AgentID: agentID, Phase: "stop"}

	// SubagentStop's own agent_type is empty in real traffic; recover type and
	// start time from the state SubagentStart recorded. A missing entry (e.g. a
	// lost write race between parallel hook processes) degrades gracefully to
	// whatever the raw payload carries.
	out.AgentType, _ = ctx.RawEvent["agent_type"].(string)
	if ctx.SessionID != "" {
		st := loadState(ctx.SessionID)
		if info, ok := st.Subagents[agentID]; ok {
			if info.Type != "" {
				out.AgentType = info.Type
			}
			if started, err := time.Parse(time.RFC3339, info.StartedAt); err == nil {
				out.DurationMs = time.Since(started).Milliseconds()
			}
			delete(st.Subagents, agentID)
			saveState(ctx.SessionID, st)
		}
	}

	// Price the subagent's own transcript (bounded: per-agent transcripts are
	// small). Best-effort — a missing file or pricing table just omits the
	// token/USD fields.
	if path, _ := ctx.RawEvent["agent_transcript_path"].(string); path != "" {
		sumAgentTranscript(path, out)
	}
	return out
}

// sumAgentTranscript folds every priced request in the subagent transcript
// into the enrichment: request count, token and dollar totals, and the
// costliest model.
func sumAgentTranscript(path string, out *SubagentEnrichment) {
	table, err := cost.LoadPriceTable()
	if err != nil {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var tokens cost.Tokens
	var usd cost.Cost
	usd.Currency = cost.Currency
	modelCost := map[string]float64{}
	seen := map[string]bool{}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	for scanner.Scan() {
		ev, dedupKey, ok := cost.ParseTranscriptLine(scanner.Bytes(), table, "")
		if !ok || seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		out.Requests++
		tokens.Input += ev.Tokens.Input
		tokens.Output += ev.Tokens.Output
		tokens.CacheRead += ev.Tokens.CacheRead
		tokens.CacheWrite += ev.Tokens.CacheWrite
		usd.Input += ev.Cost.Input
		usd.Output += ev.Cost.Output
		usd.CacheRead += ev.Cost.CacheRead
		usd.CacheWrite += ev.Cost.CacheWrite
		usd.Total += ev.Cost.Total
		modelCost[ev.Model] += ev.Cost.Total
	}
	if out.Requests == 0 {
		return
	}
	best, bestCost := "", -1.0
	for m, c := range modelCost {
		if c > bestCost {
			best, bestCost = m, c
		}
	}
	out.Model = best
	out.Tokens = &tokens
	out.USD = &usd
}
