package enrich

// PermissionEnrichment is the "permission" slug, attached to
// PermissionRequest / PermissionDenied events: the permission-friction
// signal. A high request rate is the "add allowlist rules" coaching insight;
// denials mark policy blocks. Tool names only — the payload's tool_input and
// denial reason can carry content and are deliberately excluded.
type PermissionEnrichment struct {
	// Decision is "requested" (PermissionRequest) or "denied" (PermissionDenied).
	Decision string `json:"decision"`
	ToolName string `json:"tool_name,omitempty"`
	// MCPServer is the <server> of an mcp__<server>__<tool> tool name.
	MCPServer string `json:"mcp_server,omitempty"`
}

type permissionEnricher struct{}

func init() { Register(permissionEnricher{}) }

func (permissionEnricher) Slug() string { return "permission" }

func (permissionEnricher) Applies(ctx *Context) bool {
	if ctx.Tool != "claude-code" {
		return false
	}
	return ctx.HookEvent == "PermissionRequest" || ctx.HookEvent == "PermissionDenied"
}

func (permissionEnricher) Enrich(ctx *Context) (any, error) {
	decision := "requested"
	if ctx.HookEvent == "PermissionDenied" {
		decision = "denied"
	}
	out := PermissionEnrichment{Decision: decision}
	out.ToolName, _ = ctx.RawEvent["tool_name"].(string)
	if m := mcpNameRE.FindStringSubmatch(out.ToolName); m != nil {
		out.MCPServer = m[1]
	}
	return out, nil
}
