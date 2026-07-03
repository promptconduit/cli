package enrich

import (
	"regexp"
	"strings"
)

// ToolsEnrichment is the "tools" slug, attached to tool-result events
// (PostToolUse, PostToolUseFailure, PostToolBatch). It normalizes Claude
// Code's single-call and batched shapes into one flat list so readers stop
// re-deriving tool metrics from tool-specific raw payloads.
//
// Privacy invariant (load-bearing — this ships in a public repo): calls carry
// NAMES and NUMBERS only, never tool_input contents. The one exception is the
// invoked SKILL.md name, matching the existing coaching extraction.
type ToolsEnrichment struct {
	Total  int        `json:"total"`
	Failed int        `json:"failed"`
	Calls  []ToolCall `json:"calls"`
}

// ToolCall is one normalized tool invocation.
type ToolCall struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// DurationMs from the single-call top-level field, or the Agent/Task
	// tool_response's totalDurationMs.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// MCPServer is the <server> of an mcp__<server>__<tool> name.
	MCPServer string `json:"mcp_server,omitempty"`
	// Skill is the invoked SKILL.md name when Name == "Skill".
	Skill string `json:"skill,omitempty"`
	// AgentType for Agent/Task subagent-spawning calls (tool_response.agentType).
	AgentType string `json:"agent_type,omitempty"`
}

var mcpNameRE = regexp.MustCompile(`^mcp__(.+?)__`)

type toolsEnricher struct{}

func init() { Register(toolsEnricher{}) }

func (toolsEnricher) Slug() string { return "tools" }

func (toolsEnricher) Applies(ctx *Context) bool {
	if ctx.Tool != "claude-code" {
		return false
	}
	switch ctx.HookEvent {
	case "PostToolUse", "PostToolUseFailure", "PostToolBatch":
		return true
	}
	return false
}

func (toolsEnricher) Enrich(ctx *Context) (any, error) {
	failed := ctx.HookEvent == "PostToolUseFailure"

	var calls []ToolCall
	if ctx.HookEvent == "PostToolBatch" {
		raw, _ := ctx.RawEvent["tool_calls"].([]interface{})
		for _, c := range raw {
			if m, ok := c.(map[string]interface{}); ok {
				if call, ok := normalizeCall(m, false); ok {
					calls = append(calls, call)
				}
			}
		}
	} else if call, ok := normalizeCall(ctx.RawEvent, failed); ok {
		calls = append(calls, call)
	}
	if len(calls) == 0 {
		return nil, nil
	}

	out := &ToolsEnrichment{Total: len(calls), Calls: calls}
	for _, c := range calls {
		if !c.OK {
			out.Failed++
		}
	}
	return out, nil
}

// normalizeCall maps one raw call (a PostToolBatch element, or the event's own
// top-level fields for the single-call shapes) into a ToolCall.
func normalizeCall(m map[string]interface{}, failed bool) (ToolCall, bool) {
	name, _ := m["tool_name"].(string)
	if name == "" {
		return ToolCall{}, false
	}
	resp, _ := m["tool_response"].(map[string]interface{})

	call := ToolCall{
		Name: name,
		OK:   !failed && !responseIsError(resp),
	}
	if d, ok := m["duration_ms"].(float64); ok {
		call.DurationMs = int64(d)
	}
	if s := mcpNameRE.FindStringSubmatch(name); s != nil {
		call.MCPServer = s[1]
	}
	input, _ := m["tool_input"].(map[string]interface{})
	if name == "Skill" {
		if skill, ok := input["skill"].(string); ok && strings.TrimSpace(skill) != "" {
			call.Skill = skill
		}
	}
	if name == "Agent" || name == "Task" {
		if at, ok := resp["agentType"].(string); ok {
			call.AgentType = at
		} else if at, ok := input["subagent_type"].(string); ok {
			call.AgentType = at
		}
		if d, ok := resp["totalDurationMs"].(float64); ok && call.DurationMs == 0 {
			call.DurationMs = int64(d)
		}
	}
	return call, true
}

// responseIsError mirrors the shared rule (extension coaching + platform
// ingest): a tool_response with is_error / isError / interrupted true failed.
func responseIsError(resp map[string]interface{}) bool {
	if resp == nil {
		return false
	}
	return resp["is_error"] == true || resp["isError"] == true || resp["interrupted"] == true
}
