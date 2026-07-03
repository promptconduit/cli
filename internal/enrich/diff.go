package enrich

import "github.com/promptconduit/cli/internal/git"

// DiffEnrichment is the "diff" slug, attached to turn-end events (Stop,
// SessionEnd): the working tree's change stats vs HEAD (staged + unstaged),
// so reports can tie a session's cost to its output — "this $1.61 session
// changed +120/−40 across 3 files". Counts only, never file names or content.
type DiffEnrichment struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

type diffEnricher struct{}

func init() { Register(diffEnricher{}) }

func (diffEnricher) Slug() string { return "diff" }

func (diffEnricher) Applies(ctx *Context) bool {
	if ctx.Cwd == "" {
		return false
	}
	return ctx.HookEvent == "Stop" || ctx.HookEvent == "SessionEnd"
}

func (diffEnricher) Enrich(ctx *Context) (any, error) {
	files, insertions, deletions, ok := git.DiffShortstat(ctx.Cwd)
	if !ok {
		return nil, nil // not a git repo — omit the slug; zeros mean "clean tree"
	}
	return DiffEnrichment{
		FilesChanged: files,
		Insertions:   insertions,
		Deletions:    deletions,
	}, nil
}
