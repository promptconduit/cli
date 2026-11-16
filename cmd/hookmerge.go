package cmd

import "regexp"

// Shared-file hook merging.
//
// Claude Code, Cursor, Gemini CLI and Codex keep every hook — the user's and
// ours — in one settings file, keyed by event name. Each event maps to an
// array whose elements are either:
//
//   - a matcher group: {"matcher": "...", "hooks": [ {handler}, ... ]}
//     (Claude Code, Gemini CLI, Codex), or
//   - a flat handler:  {"command": "..."} (Cursor).
//
// Install and uninstall must only ever touch OUR handlers. The helpers below
// remove PromptConduit handlers at the handler level (never a whole event or a
// whole matcher group that still holds someone else's hook) and append ours
// alongside whatever the user already has.

// ourHookCommand matches the command we install: the promptconduit binary
// (any directory, optional suffix like ".exe") followed by the `hook`
// subcommand, e.g. "/usr/local/bin/promptconduit hook --tool codex". Requiring
// the binary basename + `hook` keeps user scripts that merely live under a
// directory named "promptconduit" from being mistaken for ours.
var ourHookCommand = regexp.MustCompile(`(?i)promptconduit[^/\\\s]*\s+hook(\s|$)`)

// isPromptConduitHandler reports whether a single hook handler is one we
// installed.
func isPromptConduitHandler(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok {
		return false
	}
	cmd, ok := m["command"].(string)
	return ok && ourHookCommand.MatchString(cmd)
}

// stripPromptConduitHooks removes every PromptConduit handler from a hooks
// map in place and returns how many handlers were removed. Matcher groups and
// events left empty by the removal are dropped; anything that wasn't emptied
// by us (including entries of unknown shape) is left untouched.
func stripPromptConduitHooks(hooks map[string]interface{}) int {
	removed := 0
	for event, raw := range hooks {
		entries, ok := raw.([]interface{})
		if !ok {
			continue
		}
		kept := make([]interface{}, 0, len(entries))
		for _, entry := range entries {
			if isPromptConduitHandler(entry) {
				removed++
				continue
			}
			group, ok := entry.(map[string]interface{})
			if !ok {
				kept = append(kept, entry)
				continue
			}
			inner, ok := group["hooks"].([]interface{})
			if !ok {
				kept = append(kept, entry)
				continue
			}
			innerKept := make([]interface{}, 0, len(inner))
			for _, h := range inner {
				if isPromptConduitHandler(h) {
					removed++
					continue
				}
				innerKept = append(innerKept, h)
			}
			if len(innerKept) == len(inner) {
				kept = append(kept, entry)
				continue
			}
			if len(innerKept) == 0 {
				continue // group held only our handlers
			}
			group["hooks"] = innerKept
			kept = append(kept, group)
		}
		if len(kept) == len(entries) {
			continue
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	return removed
}

// mergePromptConduitHooks replaces any previously-installed PromptConduit
// handlers in settings["hooks"] with ours, keeping every other hook. Our
// entries are appended after the user's on each event, so re-running install
// is idempotent. All other settings keys are left as-is.
func mergePromptConduitHooks(settings map[string]interface{}, ours map[string]interface{}) {
	existing, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		existing = make(map[string]interface{})
		settings["hooks"] = existing
	}
	stripPromptConduitHooks(existing)
	for event, raw := range ours {
		add := toInterfaceSlice(raw)
		switch cur := existing[event].(type) {
		case []interface{}:
			existing[event] = append(cur, add...)
		case nil:
			existing[event] = add
		default:
			// Unknown shape for this event: we can't merge into it safely,
			// so keep the user's value and skip registering ours there.
		}
	}
}

// toInterfaceSlice converts the []map[string]interface{} values built by the
// buildXHooks helpers into the []interface{} shape json.Unmarshal produces,
// so merged arrays have a single element type.
func toInterfaceSlice(v interface{}) []interface{} {
	switch s := v.(type) {
	case []interface{}:
		return s
	case []map[string]interface{}:
		out := make([]interface{}, len(s))
		for i, m := range s {
			out[i] = m
		}
		return out
	default:
		return []interface{}{v}
	}
}
