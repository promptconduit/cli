# PromptConduit CLI

Open-source Go CLI (`promptconduit`) that captures events from AI coding assistants (Claude Code, Cursor; others behind `--experimental`) via hooks, writes them to a local event log, and, when an API key is set, forwards them to the PromptConduit service. Free tier is fully local. Live at https://promptconduit.dev (install: `curl -fsSL https://promptconduit.dev/install | bash`).

## Commands

```bash
scripts/verify        # lint + test + build (what PR CI runs, plus golangci-lint); --fast skips lint
make build            # build binary
make build-all        # all platforms
make test             # go test ./...
make lint             # golangci-lint (must be installed)
make dev              # build and install hooks for local testing
make tidy             # go mod tidy
make snapshot         # GoReleaser snapshot (no publish)
PROMPTCONDUIT_API_URL=http://localhost:8787 ./promptconduit test   # against a local API
```

No dev server and no e2e suite: this is a CLI.

## Layout

- `cmd/` CLI commands (install, uninstall, status, test, hook, config, sync, ...)
- `internal/client/` HTTP client, config loading (`ShouldSend()` is the single send gate)
- `internal/envelope/` v2 event envelope types + per-tool id extraction
- `internal/enrich/` slug-keyed enrichment registry (env, trace, vcs, prompt, cost, ...)
- `internal/eventlog/`, `internal/outbound/` local event log and outbound request mirror
- `internal/sync/`, `internal/transcript/` transcript + plan sync and parsing
- `internal/extension/` bundled editor-extension `.vsix` (`go:embed`) + sideload
- `internal/graph/` tail-and-serve shell for `promptconduit graph`
- `internal/mcp/` hand-rolled MCP server for `promptconduit mcp`; `cmd/team.go` team commands
- `internal/updater/` release check + self-replace upgrade
- `scripts/` install script; `main.go` entry point

## Deploy & release

- PRs run `.github/workflows/test.yml` (`make test`, `make build`).
- Releases are built by GoReleaser via `release.yml` on a `v*` tag push. Per the workspace merge/deploy rule (owner, 2026-11-24), an agent tags a release itself once its PR is merged and green, then checks the GitHub release and `promptconduit upgrade` on this machine.
- The binary auto-updates from GitHub Releases (opt out with config or `PROMPTCONDUIT_AUTO_UPDATE=0`).
- `refresh-extension.yml` / `refresh-graph.yml` open PRs that refresh the embedded `.vsix` and graph page.

## Rules

- Feature branch + PR only (`feat/`, `fix/`, `chore/`, ...); never commit to `main`.
- Public repo: talk to the service only through its public API-key `/v1` routes. No secrets, no private-service internals, no unreleased plans in code, commits or PR text.
- All event transformation happens server-side; the CLI wraps raw tool events in an envelope and forwards them. Capture to `~/.promptconduit/events.jsonl` happens before (and regardless of) sending.
- Envelope v2 and enrichment slug shapes are mirrored in the editor extension and the service: additive-only changes. A failing enrichment drops its slug, never the event (except `cost` on a Claude Code Stop, which carries `{"error": ...}` so a gap is never silent); keep the hook fast.
- Prefer `~/.config/promptconduit/config.json` over env vars for config.
- Embedded artifacts (`internal/extension/embedded/*.vsix`, `internal/graph/ui/graph.html`) are committed; refresh with `make refresh-extension` / `make refresh-graph`. Keep them out of `dist/` (gitignored).
- `sync` is manual only; hooks are real-time.
- JSON field names shared with the extension (`extension-update.json`, `sessions.Session`) must stay in sync with it.

## Docs

- `docs/architecture.md`: envelope v2 + enrichments, batched sending, MCP server/team, auto-update, sync, bundled extension, live graph, free tier
- `README.md`: user-facing usage
