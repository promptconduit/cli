package enrich

import (
	"os"
	"runtime"
)

// EnvEnrichment is the "env" slug: where the event was produced.
type EnvEnrichment struct {
	Host string `json:"host,omitempty"` // machine hostname (best-effort)
	OS   string `json:"os,omitempty"`   // runtime.GOOS
	Arch string `json:"arch,omitempty"` // runtime.GOARCH
	Cwd  string `json:"cwd,omitempty"`  // tool-reported working directory
}

type envEnricher struct{}

func init() { Register(envEnricher{}) }

func (envEnricher) Slug() string              { return "env" }
func (envEnricher) Applies(ctx *Context) bool { return true }

func (envEnricher) Enrich(ctx *Context) (any, error) {
	host, _ := os.Hostname()
	return EnvEnrichment{
		Host: host,
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
		Cwd:  ctx.Cwd,
	}, nil
}
