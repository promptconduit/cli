package loop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/promptconduit/cli/internal/cost"
)

// Result is one prompt's loop read from the local event log.
type Result struct {
	SessionID string          `json:"sessionId"`
	Tool      string          `json:"tool,omitempty"`
	Ledger    Ledger          `json:"ledger"`
	Advice    []Advice        `json:"advice"`
	Opening   *SessionOpening `json:"sessionOpening,omitempty"`
	// Prompts is how many prompts the session has so far.
	Prompts int `json:"sessionPrompts"`
}

// ErrNotFound is returned when the log has no matching prompt with calls.
type ErrNotFound struct{ Msg string }

func (e ErrNotFound) Error() string { return e.Msg }

// envelopeLine is the subset of a v2 envelope the loop reads.
type envelopeLine struct {
	SessionID   string `json:"session_id"`
	PromptID    string `json:"prompt_id"`
	Tool        string `json:"tool"`
	HookEvent   string `json:"hook_event"`
	CapturedAt  string `json:"captured_at"`
	Enrichments struct {
		Prompt *struct {
			TaskKind string `json:"task_kind"`
		} `json:"prompt"`
		Cost *struct {
			Requests []struct {
				RequestID string            `json:"request_id"`
				Model     string            `json:"model"`
				Timestamp string            `json:"ts"`
				Tokens    cost.Tokens       `json:"tokens"`
				USD       cost.Cost         `json:"usd"`
				Tools     *cost.ToolSummary `json:"tools"`
			} `json:"requests"`
		} `json:"cost"`
		Tools *struct {
			Calls []struct {
				Name       string `json:"name"`
				OK         bool   `json:"ok"`
				DurationMs int64  `json:"duration_ms"`
				MCPServer  string `json:"mcp_server"`
			} `json:"calls"`
		} `json:"tools"`
	} `json:"enrichments"`
}

func isPromptEvent(h string) bool { return h == "UserPromptSubmit" || h == "beforeSubmitPrompt" }
func isStopEvent(h string) bool   { return h == "Stop" || h == "StopFailure" || h == "stop" }

// FromEventLogFile reads events.jsonl at path. See FromEventLog.
func FromEventLogFile(path, sessionID, promptID string, table *cost.PriceTable) (*Result, error) {
	open := func() (io.ReadCloser, error) { return os.Open(path) }
	return FromEventLog(open, sessionID, promptID, table)
}

// FromEventLog builds the loop for one prompt from v2 envelopes. With no
// sessionID it uses the session of promptID, or else the session with the
// most recent prompt. With no promptID it uses the session's latest prompt
// that has priced calls. read is called once per pass over the log.
func FromEventLog(read func() (io.ReadCloser, error), sessionID, promptID string, table *cost.PriceTable) (*Result, error) {
	if sessionID == "" {
		var err error
		if sessionID, err = findSession(read, promptID); err != nil {
			return nil, err
		}
		if sessionID == "" {
			if promptID != "" {
				return nil, ErrNotFound{fmt.Sprintf("no prompt %s in the local event log", promptID)}
			}
			return nil, ErrNotFound{"no prompts in the local event log yet"}
		}
	}

	var (
		prompts      []*Prompt
		byID         = map[string]*Prompt{}
		stops        []time.Time
		requests     []Request
		results      []ToolResult
		batchResults []ToolResult
		tool         string
	)
	err := scan(read, []byte(sessionID), func(e *envelopeLine) {
		if e.SessionID != sessionID {
			return
		}
		at, _ := time.Parse(time.RFC3339Nano, e.CapturedAt)
		if tool == "" {
			tool = e.Tool
		}
		switch {
		case isPromptEvent(e.HookEvent):
			if e.PromptID != "" && byID[e.PromptID] != nil {
				return
			}
			p := &Prompt{PromptID: e.PromptID, StartedAt: at}
			if e.Enrichments.Prompt != nil {
				p.TaskKind = e.Enrichments.Prompt.TaskKind
			}
			prompts = append(prompts, p)
			if e.PromptID != "" {
				byID[e.PromptID] = p
			}
		case isStopEvent(e.HookEvent):
			stops = append(stops, at)
		}
		if c := e.Enrichments.Cost; c != nil {
			for _, r := range c.Requests {
				req := Request{RequestID: r.RequestID, TS: r.Timestamp, Model: r.Model, Tokens: r.Tokens, USD: r.USD}
				if r.Tools != nil {
					req.ToolNames = r.Tools.ByName
				}
				requests = append(requests, req)
			}
		}
		if tl := e.Enrichments.Tools; tl != nil {
			for _, c := range tl.Calls {
				r := ToolResult{At: at, Name: c.Name, OK: c.OK, DurationMs: c.DurationMs, MCPServer: c.MCPServer}
				if e.HookEvent == "PostToolBatch" {
					batchResults = append(batchResults, r)
				} else {
					results = append(results, r)
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	// Claude Code reports each call in its own PostToolUse AND again in the
	// PostToolBatch that closes the batch. Per-call events carry the timing,
	// so batches only count when a session has no per-call events.
	if len(results) == 0 {
		results = batchResults
	}
	if len(prompts) == 0 {
		return nil, ErrNotFound{fmt.Sprintf("session %s has no prompts in the local event log", sessionID)}
	}
	sort.SliceStable(prompts, func(i, j int) bool { return prompts[i].StartedAt.Before(prompts[j].StartedAt) })

	// A turn ends at the first Stop after its prompt (and before the next).
	for i, p := range prompts {
		for _, s := range stops {
			if s.Before(p.StartedAt) || (i+1 < len(prompts) && !s.Before(prompts[i+1].StartedAt)) {
				continue
			}
			p.EndedAt = s
			break
		}
	}
	Attribute(prompts, requests)
	for i, p := range prompts {
		var next time.Time
		if i+1 < len(prompts) {
			next = prompts[i+1].StartedAt
		}
		for _, r := range results {
			if !r.At.Before(p.StartedAt) && (next.IsZero() || r.At.Before(next)) {
				p.ToolResults = append(p.ToolResults, r)
			}
		}
	}

	var target *Prompt
	if promptID != "" {
		target = byID[promptID]
		if target == nil {
			return nil, ErrNotFound{fmt.Sprintf("no prompt %s in session %s", promptID, sessionID)}
		}
	} else {
		for i := len(prompts) - 1; i >= 0; i-- {
			if len(prompts[i].Requests) > 0 {
				target = prompts[i]
				break
			}
		}
	}
	if target == nil {
		return nil, ErrNotFound{fmt.Sprintf("session %s has no priced calls yet (cost is recorded when a turn ends)", sessionID)}
	}
	ledger, ok := BuildLedger(target, table)
	if !ok {
		return nil, ErrNotFound{fmt.Sprintf("prompt %s has no priced calls yet (cost is recorded when its turn ends)", target.PromptID)}
	}

	res := &Result{SessionID: sessionID, Tool: tool, Ledger: ledger, Prompts: len(prompts)}
	res.Opening = sessionOpening(requests)
	res.Advice = BuildAdvice(ledger, table, res.Opening)
	return res, nil
}

// sessionOpening is the session's earliest call.
func sessionOpening(reqs []Request) *SessionOpening {
	var first *Request
	var firstTS time.Time
	for i := range reqs {
		ts, ok := parseTS(reqs[i].TS)
		if ok && (first == nil || ts.Before(firstTS)) {
			first, firstTS = &reqs[i], ts
		}
	}
	if first == nil {
		return nil
	}
	return &SessionOpening{
		FirstCallUSD:     first.USD.Total,
		FirstCallContext: first.Tokens.Input + first.Tokens.CacheRead + first.Tokens.CacheWrite,
	}
}

// findSession returns the session of promptID, or of the newest prompt event.
func findSession(read func() (io.ReadCloser, error), promptID string) (string, error) {
	var (
		session string
		newest  time.Time
	)
	filter := []byte("rompt") // UserPromptSubmit / beforeSubmitPrompt
	if promptID != "" {
		filter = []byte(promptID)
	}
	err := scan(read, filter, func(e *envelopeLine) {
		if !isPromptEvent(e.HookEvent) || e.SessionID == "" {
			return
		}
		if promptID != "" {
			if e.PromptID == promptID && session == "" {
				session = e.SessionID
			}
			return
		}
		at, _ := time.Parse(time.RFC3339Nano, e.CapturedAt)
		if session == "" || !at.Before(newest) {
			session, newest = e.SessionID, at
		}
	})
	return session, err
}

// scan decodes every log line containing filter.
func scan(read func() (io.ReadCloser, error), filter []byte, fn func(*envelopeLine)) error {
	rc, err := read()
	if os.IsNotExist(err) {
		return ErrNotFound{"no local event log yet: install hooks with `promptconduit install`"}
	}
	if err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	defer func() { _ = rc.Close() }()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(filter) > 0 && !bytes.Contains(line, filter) {
			continue
		}
		var e envelopeLine
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		fn(&e)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read event log: %w", err)
	}
	return nil
}
