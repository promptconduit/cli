package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/git"
	"github.com/promptconduit/cli/internal/mcp"
	skillspkg "github.com/promptconduit/cli/internal/skills"
)

// MCP tools for skills: the slash commands PromptConduit detects from
// recurring prompts. They share code with `promptconduit skills` (resolve by
// name or uuid, manifest-tracked install, approve/reject via PATCH), so the
// Skills web page is never required.

var skillProp = strProp("Skill name or uuid. Names shared by several skills (common after re-running generation) error with each candidate's uuid.")

func skillsMCPTools() []*mcp.Tool {
	return []*mcp.Tool{
		{
			Name:  "skills_list",
			Title: "List skills",
			Description: "Skills detected from your AI coding sessions, with name, description, type, confidence, repo scope and approval state. " +
				"filter \"new\" (default) is what still needs a decision, \"ready\" is approved and installable, \"removed\" is rejected. Needs promptconduit login.",
			InputSchema: objectSchema(schemaProps{
				"filter": strProp("Which skills (default new).", "new", "ready", "removed", "all"),
				"repo":   strProp("Only skills for this repo (e.g. acme/api). Omit for every scope; each skill's repo_name says which repo it belongs to, and an empty repo_name means global."),
				"type":   strProp("Only this skill type.", "workflow", "command", "template", "checklist"),
				"limit":  intProp("Maximum skills to return (default 50).", 1, 100),
			}),
			Annotations: readOnly("List skills"),
			Handler:     toolSkillsList,
		},
		{
			Name:  "skills_patterns",
			Title: "Recurring prompt patterns",
			Description: "The recurring themes in your prompts that skill detection is based on, grouped by theme with counts and examples. " +
				"Scoped to the current repo unless repo is given. Needs promptconduit login.",
			InputSchema: objectSchema(schemaProps{
				"repo": strProp("Repo to scope to (e.g. acme/api); empty string for global patterns. Default: the repo of cwd."),
				"cwd":  strProp("Directory used to detect the repo. Default: the directory the agent runs in."),
			}),
			Annotations: readOnly("Recurring prompt patterns"),
			Handler:     toolSkillsPatterns,
		},
		{
			Name:  "skills_generate",
			Title: "Detect skills",
			Description: "Run skill detection on your synced transcripts and return the skills found (new ones appear in skills_list as \"new\"). " +
				"Can take up to 30 seconds. Results are cached; set force to re-analyze. Needs synced transcripts (promptconduit sync) and promptconduit login.",
			InputSchema: objectSchema(schemaProps{
				"repo":  strProp("Repo to scope to (e.g. acme/api); empty string for global. Default: the repo of cwd."),
				"cwd":   strProp("Directory used to detect the repo. Default: the directory the agent runs in."),
				"force": boolProp("Re-analyze even if a cached result exists."),
			}),
			Annotations: teamWrite("Detect skills", false, false),
			Handler:     toolSkillsGenerate,
		},
		{
			Name:        "skills_approve",
			Title:       "Approve a skill",
			Description: "Mark a skill approved so it can be installed (skills_list filter \"ready\"). Reversible with skills_reject.",
			InputSchema: objectSchema(schemaProps{"skill": skillProp}, "skill"),
			Annotations: teamWrite("Approve a skill", false, true),
			Handler:     toolSkillsApprove,
		},
		{
			Name:        "skills_reject",
			Title:       "Reject a skill",
			Description: "Mark a skill rejected so it leaves the new list (skills_list filter \"removed\"). Reversible with skills_approve; installed files are not touched.",
			InputSchema: objectSchema(schemaProps{"skill": skillProp}, "skill"),
			Annotations: teamWrite("Reject a skill", false, true),
			Handler:     toolSkillsReject,
		},
		{
			Name:  "skills_install",
			Title: "Install a skill",
			Description: "Download an approved skill to .claude/skills/<name>/SKILL.md so it is available as a slash command, and record it so it can be uninstalled. " +
				"Project-scoped skills go in the repo at cwd, global ones in ~/.claude/skills, unless scope says otherwise. " +
				"A file edited since install is skipped unless force is true. Pass all instead of skill to install every approved skill.",
			InputSchema: objectSchema(schemaProps{
				"skill": skillProp,
				"all":   boolProp("Install every approved skill instead of one."),
				"scope": strProp("Where to install.", "project", "global"),
				"force": boolProp("Overwrite a SKILL.md that was edited locally."),
				"cwd":   strProp("Repo directory for project scope. Default: the directory the agent runs in."),
			}),
			Annotations: teamWrite("Install a skill", false, true),
			Handler:     toolSkillsInstall,
		},
		{
			Name:  "skills_delete",
			Title: "Delete a skill",
			Description: "Soft-delete a skill on the platform so it disappears from every list. Use it for duplicates or skills you will never want; " +
				"skills_reject is enough to hide one. Needs confirm: true after the user has agreed. A deleted skill no longer resolves, so don't retry on a not-found error.",
			InputSchema: objectSchema(schemaProps{"skill": skillProp, "confirm": confirmProp}, "skill", "confirm"),
			Annotations: teamWrite("Delete a skill", true, false),
			Handler:     toolSkillsDelete,
		},
	}
}

// ---- helpers ----

// skillsCall runs fn with a signed-in client.
func skillsCall(fn func(c *client.Client) (interface{}, error)) (interface{}, error) {
	c, err := insightsClient()
	if err != nil {
		return nil, err
	}
	return fn(c)
}

// skillRepo mirrors the CLI's --repo handling: an absent repo means "the
// repo of cwd", an explicit empty string means global.
func skillRepo(repo *string, cwd string) string {
	if repo != nil {
		return *repo
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return git.GetRepoName(cwd)
}

// skillSummary keeps the fields an agent needs to decide about a skill.
func skillSummary(m map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, k := range []string{"id", "name", "display_name", "description", "skill_type", "confidence", "repo_name", "conversation_count", "created_at"} {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
		}
	}
	state := "new"
	if v, ok := m["is_approved"].(bool); ok {
		if v {
			state = "ready"
		} else {
			state = "removed"
		}
	}
	out["state"] = state
	return out
}

func summarizeSkills(data map[string]interface{}) []interface{} {
	list, _ := data["skills"].([]interface{})
	out := make([]interface{}, 0, len(list))
	for _, s := range list {
		if m, ok := s.(map[string]interface{}); ok {
			out = append(out, skillSummary(m))
		}
	}
	return out
}

// ---- handlers ----

func toolSkillsList(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Filter string `json:"filter"`
		Repo   string `json:"repo"`
		Type   string `json:"type"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Filter == "" {
		a.Filter = "new"
	}
	approved := ""
	switch a.Filter {
	case "new", "all":
	case "ready":
		approved = "true"
	case "removed":
		approved = "false"
	default:
		return nil, fmt.Errorf("filter must be new, ready, removed or all, got %q", a.Filter)
	}
	if a.Limit == 0 {
		a.Limit = 50
	}
	return skillsCall(func(c *client.Client) (interface{}, error) {
		// "new" (is_approved null) can't be expressed server-side, so fetch
		// the server maximum and filter here before applying limit.
		fetch := a.Limit
		if a.Filter == "new" {
			fetch = 100
		}
		resp := c.GetSkills(approved, a.Type, fetch, a.Repo)
		if !resp.Success {
			return nil, teamAPIError("failed to list skills", resp)
		}
		if a.Filter == "new" {
			filterUnapproved(resp.Data)
		}
		skills := summarizeSkills(resp.Data)
		if len(skills) > a.Limit {
			skills = skills[:a.Limit]
		}
		out := map[string]interface{}{"filter": a.Filter, "skills": skills, "count": len(skills)}
		if len(skills) == 0 {
			switch a.Filter {
			case "new":
				out["note"] = "No new skills. Run skills_generate to detect skills from synced transcripts, or check filter \"ready\" for approved ones."
			case "ready":
				out["note"] = "No approved skills. Approve one from filter \"new\" with skills_approve."
			}
		}
		return out, nil
	})
}

func toolSkillsPatterns(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Repo *string `json:"repo"`
		Cwd  string  `json:"cwd"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	repo := skillRepo(a.Repo, a.Cwd)
	return skillsCall(func(c *client.Client) (interface{}, error) {
		resp := c.GetSkillPatterns(repo)
		if !resp.Success {
			return nil, teamAPIError("failed to get patterns", resp)
		}
		data := resp.Data
		if data == nil {
			data = map[string]interface{}{}
		}
		data["scope"] = scopeLabel(repo)
		return data, nil
	})
}

func toolSkillsGenerate(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Repo  *string `json:"repo"`
		Cwd   string  `json:"cwd"`
		Force bool    `json:"force"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	repo := skillRepo(a.Repo, a.Cwd)
	return skillsCall(func(c *client.Client) (interface{}, error) {
		resp := c.GenerateSkills(a.Force, repo)
		if !resp.Success {
			return nil, teamAPIError("failed to generate skills", resp)
		}
		skills := summarizeSkills(resp.Data)
		out := map[string]interface{}{"scope": scopeLabel(repo), "skills": skills, "count": len(skills)}
		for _, k := range []string{"conversationCount", "fromCache"} {
			if v, ok := resp.Data[k]; ok {
				out[k] = v
			}
		}
		if len(skills) == 0 {
			out["note"] = "No skills detected. Detection needs synced transcripts: run `promptconduit sync`, then try again with force."
		} else {
			out["next"] = "Ask the user which to keep, then skills_approve and skills_install them."
		}
		return out, nil
	})
}

func scopeLabel(repo string) string {
	if repo == "" {
		return "global"
	}
	return repo
}

func toolSkillsApprove(_ context.Context, raw json.RawMessage) (interface{}, error) {
	return approveOrRejectTool(raw, true)
}

func toolSkillsReject(_ context.Context, raw json.RawMessage) (interface{}, error) {
	return approveOrRejectTool(raw, false)
}

func approveOrRejectTool(raw json.RawMessage, approve bool) (interface{}, error) {
	var a struct {
		Skill string `json:"skill"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	return skillsCall(func(c *client.Client) (interface{}, error) {
		skill, err := resolveSkill(c, a.Skill)
		if err != nil {
			return nil, err
		}
		id, _ := skill["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("skill %q has no id", a.Skill)
		}
		resp := c.ApproveSkill(id, approve)
		if !resp.Success {
			return nil, teamAPIError("failed to update skill", resp)
		}
		out := skillSummary(skill)
		if approve {
			out["state"] = "ready"
			out["next"] = "Install it with skills_install."
		} else {
			out["state"] = "removed"
		}
		return out, nil
	})
}

func toolSkillsInstall(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Skill string `json:"skill"`
		All   bool   `json:"all"`
		Scope string `json:"scope"`
		Force bool   `json:"force"`
		Cwd   string `json:"cwd"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.All == (a.Skill != "") {
		return nil, errors.New("pass either skill or all: true")
	}
	if a.Scope != "" && a.Scope != "project" && a.Scope != "global" {
		return nil, fmt.Errorf("scope must be project or global, got %q", a.Scope)
	}
	cwd := a.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return skillsCall(func(c *client.Client) (interface{}, error) {
		manifest, err := skillspkg.Load(client.ConfigDir())
		if err != nil {
			return nil, fmt.Errorf("load manifest: %w", err)
		}
		var targets []map[string]interface{}
		if a.All {
			resp := c.GetSkills("true", "", 100, "")
			if !resp.Success {
				return nil, teamAPIError("failed to fetch approved skills", resp)
			}
			list, _ := resp.Data["skills"].([]interface{})
			for _, s := range list {
				if m, ok := s.(map[string]interface{}); ok {
					targets = append(targets, m)
				}
			}
		} else {
			skill, err := resolveSkill(c, a.Skill)
			if err != nil {
				return nil, err
			}
			if v, ok := skill["is_approved"].(bool); !ok || !v {
				name, _ := skill["name"].(string)
				return nil, fmt.Errorf("skill %q is not approved; call skills_approve first", name)
			}
			targets = []map[string]interface{}{skill}
		}

		opts := installOptions{cwd: cwd, scope: a.Scope, force: a.Force}
		var installed, skipped, failed []map[string]interface{}
		for _, skill := range targets {
			name, _ := skill["name"].(string)
			path, err := installSkill(c, manifest, skill, opts)
			row := map[string]interface{}{"name": name, "path": path}
			switch {
			case err == nil:
				row["command"] = "/" + name
				installed = append(installed, row)
			case errors.Is(err, errAlreadyCurrent):
				row["reason"] = "already up to date"
				skipped = append(skipped, row)
			case errors.Is(err, errAbortLocalChanges):
				row["reason"] = "edited locally since install; pass force: true to overwrite"
				skipped = append(skipped, row)
			default:
				row["error"] = err.Error()
				failed = append(failed, row)
			}
		}
		if err := skillspkg.Save(client.ConfigDir(), manifest); err != nil {
			return nil, fmt.Errorf("save manifest: %w", err)
		}
		out := map[string]interface{}{
			"installed": installed, "skipped": skipped, "failed": failed,
			"installed_count": len(installed), "skipped_count": len(skipped), "failed_count": len(failed),
		}
		if len(targets) == 0 {
			out["note"] = "No approved skills to install. Approve one with skills_approve first."
		} else if len(installed) > 0 {
			out["note"] = "Claude Code picks up new skills on its next session; the slash command is /<name>."
		}
		if len(failed) > 0 {
			// Return the breakdown rather than an error: the MCP server drops
			// the value when err != nil, and the agent needs to know what did
			// get installed.
			out["error"] = fmt.Sprintf("%d skill(s) failed to install; see failed", len(failed))
		}
		return out, nil
	})
}

func toolSkillsDelete(_ context.Context, raw json.RawMessage) (interface{}, error) {
	var a struct {
		Skill   string `json:"skill"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if !a.Confirm {
		return nil, errors.New("deleting a skill hides it for good; ask the user, then call again with confirm: true")
	}
	return skillsCall(func(c *client.Client) (interface{}, error) {
		skill, err := resolveSkill(c, a.Skill)
		if err != nil {
			return nil, err
		}
		id, _ := skill["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("skill %q has no id", a.Skill)
		}
		resp := c.DeleteSkill(id)
		if !resp.Success {
			return nil, teamAPIError("failed to delete skill", resp)
		}
		name, _ := skill["name"].(string)
		return map[string]interface{}{"deleted": true, "id": id, "name": name}, nil
	})
}
