package envelope

// ExtractIDs lifts the session and prompt identifiers out of a tool's raw hook
// payload. Both are best-effort: a tool/event that doesn't report one yields "".
//
// Per-tool mapping:
//   - claude-code: session_id + prompt_id (present on hook payloads as of
//     Claude Code 2.x; older events simply omit it)
//   - cursor: session_id (conversation_id fallback) + generation_id as the
//     prompt-scoped id — it identifies one generation, the closest Cursor
//     equivalent of a prompt id
//   - anything else: the generic session_id / sessionId keys
func ExtractIDs(tool string, rawEvent map[string]interface{}) (sessionID, promptID string) {
	sessionID = firstString(rawEvent, "session_id", "sessionId")

	switch tool {
	case "cursor":
		if sessionID == "" {
			sessionID = firstString(rawEvent, "conversation_id")
		}
		promptID = firstString(rawEvent, "generation_id")
	default:
		promptID = firstString(rawEvent, "prompt_id")
	}
	return sessionID, promptID
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
